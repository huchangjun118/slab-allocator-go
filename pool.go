package slab

import (
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
)

// Pool 是并发安全的尺寸分级内存池。
//
// 两级复用结构：
//
//  1. 每个尺寸分级一条 LIFO 空闲缓存（freeLst[i]），命中时只争用本分级的
//     锁，不争用全局结构——因此某一分级争用严重不会阻塞无关分级。
//  2. regionSet 保存跨分级的空闲段，支持块内切分与邻接合并。仅当分级缓存
//     未命中时访问；若仍无合适段，会一次性把全部分级缓存归位（触发合并）
//     后重试，最后才按 RegionSize 增长新区间。
//
// 锁序（全局唯一，避免死锁）：先按分级下标升序获取任意需要的分级锁，
// 再获取 regionSet 全局锁；不存在持全局锁后再取分级锁的路径。
type Pool struct {
	cfg     Config
	align   int // 内部对齐粒度 = max(Alignment, 8)，保证 64 位原子安全
	classes []int
	classMu []sync.Mutex
	freeLst [][]*Block // 每个分级一条 LIFO 缓存

	rs          *regionSet
	gen         atomic.Uint64
	outstanding map[*Block]struct{} // 未失效句柄（存活 + 分级缓存），Reset 统一作废
	outMu       sync.Mutex          // 仅保护 outstanding，不参与分级/rs 锁序
	statsMu     sync.Mutex
	stats       Stats
}

// New 依据 cfg 构造内存池，构造时立即建立首个底层区间。
// 非法配置返回包装了 ErrInvalidConfig 的错误。
func New(cfg Config) (*Pool, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	p := &Pool{
		cfg:         cfg,
		align:       cfg.internalAlign(),
		classes:     append([]int(nil), cfg.Classes...),
		classMu:     make([]sync.Mutex, len(cfg.Classes)),
		freeLst:     make([][]*Block, len(cfg.Classes)),
		rs:          newRegionSet(),
		outstanding: make(map[*Block]struct{}),
	}
	p.gen.Store(1)
	p.rs.mu.Lock()
	if _, err := p.rs.grow(cfg.RegionSize, p.align, cfg.MaxCapacity); err != nil {
		p.rs.mu.Unlock()
		return nil, err
	}
	p.rs.mu.Unlock()
	p.stats.RegionCount = 1
	p.stats.CapacityBytes = int64(cfg.RegionSize)
	return p, nil
}

// classIndex 返回 size 命中的分级下标（第一个 classes[i] >= size）；
// size 大于最大分级时返回 len(classes)。
func (p *Pool) classIndex(size int) int {
	return sort.SearchInts(p.classes, size)
}

// spanFor 返回授予 grant 字节在区间内占用的对齐跨度。
func (p *Pool) spanFor(grant int) int {
	return (grant + p.align - 1) &^ (p.align - 1)
}

// lockAllClasses 按固定顺序获取全部分级锁。
func (p *Pool) lockAllClasses() {
	for i := range p.classMu {
		p.classMu[i].Lock()
	}
}

func (p *Pool) unlockAllClasses() {
	for i := range p.classMu {
		p.classMu[i].Unlock()
	}
}

// drainCachesLocked 把全部分级缓存归位到全局空闲结构（触发邻接合并），并把
// 已归位句柄从“未失效”表移除。调用方必须已持有全部分级锁与 rs 锁。
func (p *Pool) drainCachesLocked() {
	var drained []*Block
	for i := range p.freeLst {
		span := p.spanFor(p.classes[i])
		for _, b := range p.freeLst[i] {
			p.rs.insertFree(b.region, b.off, span)
			drained = append(drained, b)
		}
		p.freeLst[i] = p.freeLst[i][:0]
	}
	// 归位后的旧句柄从“未失效”表移除，允许 GC 回收；对它们重复 Free 仍会
	// 因状态为 free 被拒绝（ErrDoubleFree）。
	p.outMu.Lock()
	for _, b := range drained {
		delete(p.outstanding, b)
	}
	p.outMu.Unlock()
}

// Alloc 申请一块可容纳 size 字节、对齐符合配置的内存。
//
// 失败以可区分的错误返回：
//   - ErrInvalidArgument：size <= 0
//   - ErrOversized：size 大于最大分级（不会跨块拼接或塞入更小分级）
//   - ErrPoolExhausted：达到总量上限且回收、增长后仍无空间；该失败不会
//     提前吞掉后续无关申请（状态保持不变）
func (p *Pool) Alloc(size int) (*Block, error) {
	if size <= 0 {
		return nil, fmt.Errorf("%w: size must be positive, got %d", ErrInvalidArgument, size)
	}
	idx := p.classIndex(size)
	if idx >= len(p.classes) {
		return nil, fmt.Errorf("%w: requested %d, largest class %d",
			ErrOversized, size, p.classes[len(p.classes)-1])
	}
	grant := p.classes[idx]
	span := p.spanFor(grant)

	// 快路径：命中本分级 LIFO 缓存。整个“取缓存块 + 状态翻转 + live 更新”
	// 在持有本分级锁期间完成 rs 临界区，保证 Reset 无法在中途插入。
	p.classMu[idx].Lock()
	if n := len(p.freeLst[idx]); n > 0 {
		b := p.freeLst[idx][n-1]
		p.freeLst[idx] = p.freeLst[idx][:n-1]
		if !b.state.CompareAndSwap(uint32(stateFree), uint32(stateLive)) {
			p.classMu[idx].Unlock()
			// 不可达：缓存块只可能被持分级锁的路径修改。
			return nil, ErrDoubleFree
		}
		p.rs.mu.Lock()
		b.region.live++
		p.rs.mu.Unlock()
		p.classMu[idx].Unlock()
		p.onAlloc(uint64(grant), true)
		return b, nil
	}
	p.classMu[idx].Unlock()

	// 慢路径：需要触碰全局结构。统一按固定锁序进入——先按升序持全部分级
	// 锁，再持 rs 锁；全程不存在“先 rs 后分级”的路径，杜绝 ABBA 死锁。
	// 持锁后先复查缓存（另一 goroutine 可能刚释放），再查全局空闲。
	p.lockAllClasses()
	if n := len(p.freeLst[idx]); n > 0 {
		b := p.freeLst[idx][n-1]
		p.freeLst[idx] = p.freeLst[idx][:n-1]
		if !b.state.CompareAndSwap(uint32(stateFree), uint32(stateLive)) {
			p.unlockAllClasses()
			return nil, ErrDoubleFree
		}
		p.rs.mu.Lock()
		b.region.live++
		p.rs.mu.Unlock()
		p.unlockAllClasses()
		p.onAlloc(uint64(grant), true)
		return b, nil
	}

	// 授予空间与句柄登记必须在同一个 rs 临界区完成：否则一旦 Reset 在
	// “已 allocate（live++、空间被切走）但尚未登记”之间插入，rs.reset
	// 会把该区间重新记为整段空闲，而新句柄漏过作废标记，导致同一段空间
	// 被再次授予（重复分配）。
	var b *Block
	p.rs.mu.Lock()
	// 慢路径第一步：直接在全局空闲结构中按最佳适配切分。
	r, off, reused := p.rs.allocate(span)
	if r == nil {
		// 慢路径第二步：归位全部分级缓存（邻接合并）后重试。
		p.drainCachesLocked()
		r, off, reused = p.rs.allocate(span)
		if r == nil {
			// 慢路径第三步：按需增长新区间，受 MaxCapacity 约束。
			if _, err := p.rs.grow(p.cfg.RegionSize, p.align, p.cfg.MaxCapacity); err != nil {
				p.rs.mu.Unlock()
				p.unlockAllClasses()
				return nil, err
			}
			r, off, reused = p.rs.allocate(span)
		}
	}
	b = p.newBlockLocked(r, off, span, grant, size)
	p.rs.mu.Unlock()
	p.unlockAllClasses()

	p.onAlloc(uint64(grant), reused)
	return b, nil
}

// newBlockLocked 在已授予的区间偏移处构造存活句柄并登记到未失效表。
// 调用方必须持有 rs 锁（授予空间的同一临界区）。登记紧随授予发生，
// 与 Reset 严格串行：要么整块在重置前完成登记（随后必被标记为 dead），
// 要么发生在重置之后（携带新代际）。不存在“空间已授予但句柄未登记”的窗口。
func (p *Pool) newBlockLocked(r *region, off, span, grant, size int) *Block {
	b := &Block{
		pool:   p,
		region: r,
		off:    off,
		span:   span,
		grant:  grant,
		size:   size,
		gen:    p.gen.Load(),
	}
	b.state.Store(uint32(stateLive))
	p.outMu.Lock()
	p.outstanding[b] = struct{}{}
	p.outMu.Unlock()
	return b
}

// Free 归还先前由本池 Alloc 返回的块。非法情形被明确拒绝：
//   - nil 或不属于本池：ErrInvalidPointer
//   - 重置代际不符：ErrUseAfterReset
//   - 重复/并发释放：ErrDoubleFree（原子 CAS 只有一个赢家）
func (p *Pool) Free(b *Block) error {
	if b == nil {
		return fmt.Errorf("%w: nil block", ErrInvalidPointer)
	}
	if b.pool != p || b.region == nil {
		return fmt.Errorf("%w: block not owned by this pool", ErrInvalidPointer)
	}
	idx := p.classIndex(b.grant)
	// 尺寸边界确定：grant 必为某个分级尺寸，classIndex 必命中该级。

	// 先取本分级锁再做代际/状态裁决。这与 Reset、drain 等路径的锁序
	// （先分级锁后 rs 锁）一致，从而 Free 与 Reset 并发时不会把已作废的
	// 块重新放回缓存，也不会错误递减 live。
	p.classMu[idx].Lock()
	defer p.classMu[idx].Unlock()

	if b.gen != p.gen.Load() {
		return fmt.Errorf("%w: block from generation %d, current %d",
			ErrUseAfterReset, b.gen, p.gen.Load())
	}
	if !b.state.CompareAndSwap(uint32(stateLive), uint32(stateFree)) {
		switch blockState(b.state.Load()) {
		case stateDead:
			return ErrUseAfterReset
		default:
			return ErrDoubleFree
		}
	}

	p.freeLst[idx] = append(p.freeLst[idx], b)

	p.rs.mu.Lock()
	b.region.live--
	p.rs.mu.Unlock()

	p.onFree(uint64(b.grant))
	return nil
}

// Reset 作废全部存活块并清空统计（代际号递增，底层容量保留）。
// 旧句柄在此之后的使用会被识别：Bytes 返回 nil、Valid 为 false、Free 返回
// ErrUseAfterReset。
func (p *Pool) Reset() {
	p.lockAllClasses()
	p.rs.mu.Lock()
	// 收集并标记所有未失效句柄：用户仍持有的存活块 + 分级缓存中的块。
	p.outMu.Lock()
	for b := range p.outstanding {
		b.state.Store(uint32(stateDead))
	}
	p.outstanding = make(map[*Block]struct{})
	p.outMu.Unlock()
	for i := range p.freeLst {
		p.freeLst[i] = p.freeLst[i][:0]
	}
	p.rs.reset()
	p.gen.Add(1) // 代际在锁内递增，与 newBlock 读取代际严格串行
	p.rs.mu.Unlock()
	p.unlockAllClasses()

	p.statsMu.Lock()
	p.stats = Stats{}
	p.statsMu.Unlock()
}

// ReleaseIdle 归位全部分级缓存（触发邻接合并），然后把完全空闲的底层区间
// 交还给运行时（至少保留一个区间）。返回释放的字节数。
func (p *Pool) ReleaseIdle() int64 {
	p.lockAllClasses()
	p.rs.mu.Lock()
	p.drainCachesLocked()
	released := p.rs.releaseIdle()
	n, cap := len(p.rs.regions), p.rs.cap
	p.rs.mu.Unlock()
	p.unlockAllClasses()

	p.statsMu.Lock()
	p.stats.RegionCount = n
	p.stats.CapacityBytes = cap
	p.statsMu.Unlock()
	return released
}

// Stats 返回统计快照：累计申请/释放次数、当前存活块数与占用、峰值占用、
// 复用次数，以及区间数量与容量。在相同的串行操作序列下各字段逐条一致。
func (p *Pool) Stats() Stats {
	p.statsMu.Lock()
	s := p.stats
	p.statsMu.Unlock()
	p.rs.mu.Lock()
	var live uint64
	for _, r := range p.rs.regions {
		live += uint64(r.live)
	}
	s.LiveBlocks = live
	s.RegionCount = len(p.rs.regions)
	s.CapacityBytes = p.rs.cap
	p.rs.mu.Unlock()
	return s
}

func (p *Pool) onAlloc(grant uint64, reused bool) {
	p.statsMu.Lock()
	p.stats.TotalAlloc++
	p.stats.UsedBytes += grant
	if p.stats.UsedBytes > p.stats.PeakBytes {
		p.stats.PeakBytes = p.stats.UsedBytes
	}
	if reused {
		p.stats.ReuseCount++
	}
	p.statsMu.Unlock()
}

func (p *Pool) onFree(grant uint64) {
	p.statsMu.Lock()
	p.stats.TotalFree++
	p.stats.UsedBytes -= grant
	p.statsMu.Unlock()
}
