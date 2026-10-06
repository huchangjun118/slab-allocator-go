package slab

import (
	"sort"
	"sync"
	"time"
	"unsafe"
)

// heap 为一个并发分片：独立的锁、空闲分级表与区间集合。
// 不同分片之间互不阻塞；同一分片内部所有操作串行化，
// 因此在固定操作序列下行为完全确定。
type heap struct {
	mu         sync.Mutex
	classes    []int      // 尺寸分级（与配置一致，升序）
	alignment  int        // 对齐粒度
	regionSize int        // 单区间容量
	freeLists  [][]*block // 每个分级一个 LIFO 空闲块栈
	regions    []*region  // 按 base 升序
	gen        uint64     // 当前代际，Reset 时递增

	stats   *statsCounters
	reserve func(n int) bool // 向全局预留 n 字节区间容量，成功返回 true
	release func(n int)      // 归还 n 字节区间容量
}

func newHeap(cfg *Config, stats *statsCounters, reserve func(int) bool, release func(int)) *heap {
	return &heap{
		classes:    cfg.SizeClasses,
		alignment:  cfg.Alignment,
		regionSize: cfg.RegionSize,
		freeLists:  make([][]*block, len(cfg.SizeClasses)),
		gen:        1,
		stats:      stats,
		reserve:    reserve,
		release:    release,
	}
}

// classFit 返回能完全放入 size 字节空闲块的最大分级下标，
// 即空闲块应归属的空闲表。size 必须 >= classes[0]。
func (h *heap) classFit(size int) int {
	// 最大的 i 使得 classes[i] <= size。
	i := sort.Search(len(h.classes), func(i int) bool { return h.classes[i] > size })
	return i - 1
}

// pushFree 把空闲块放入其归属分级表（LIFO，顺序确定）。
func (h *heap) pushFree(b *block) {
	ci := h.classFit(b.size)
	h.freeLists[ci] = append(h.freeLists[ci], b)
}

// removeFree 把空闲块从其归属分级表中摘除。
// 调用前提：b 空闲且一定在对应空闲表中。
func (h *heap) removeFree(b *block) {
	ci := h.classFit(b.size)
	st := h.freeLists[ci]
	for i, x := range st {
		if x == b {
			h.freeLists[ci] = append(st[:i], st[i+1:]...)
			return
		}
	}
}

// popFor 从分级空闲表中取出能满足 need（= classes[ci]）的最佳适配空闲块。
// 找不到返回 nil。调用前提：持有 h.mu。
func (h *heap) popFor(ci int) *block {
	for j := ci; j < len(h.classes); j++ {
		st := h.freeLists[j]
		if len(st) == 0 {
			continue
		}
		b := st[len(st)-1]
		h.freeLists[j] = st[:len(st)-1]
		return b
	}
	return nil
}

// findRegion 按地址二分查找包含 ptr 的区间，未找到返回 nil。
func (h *heap) findRegion(ptr uintptr) *region {
	i := sort.Search(len(h.regions), func(i int) bool { return h.regions[i].base > ptr })
	if i == 0 {
		return nil
	}
	r := h.regions[i-1]
	if r.contains(ptr) {
		return r
	}
	return nil
}

// grow 预留全局容量并创建新区间，达到上限时返回 false。
// 调用前提：持有 h.mu。
func (h *heap) grow() bool {
	if !h.reserve(h.regionSize) {
		return false
	}
	r := newRegion(h.regionSize, h.alignment)
	// 保持 regions 按 base 升序。
	i := sort.Search(len(h.regions), func(i int) bool { return h.regions[i].base > r.base })
	h.regions = append(h.regions, nil)
	copy(h.regions[i+1:], h.regions[i:])
	h.regions[i] = r
	h.pushFree(r.head)
	return true
}

// alloc 从本分片分配一个 classes[ci] 尺寸的块。
func (h *heap) alloc(ci int) (unsafe.Pointer, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	need := h.classes[ci]
	minRem := h.classes[0]

	b := h.popFor(ci)
	if b == nil {
		// 空闲表无可复用块，按需增长一个新区间。
		if !h.grow() {
			return nil, ErrOutOfMemory
		}
		// 新区间可用容量 >= 最大分级 >= need，必然可取到块。
		b = h.popFor(ci)
	}
	// 只有来自释放路径的块才计入复用；新区间首次切分不算。
	recycled := b.recycled
	r := b.region
	ab, rem := r.carve(b, need, minRem, h.gen)
	if rem != nil {
		// carve 切出的剩余块重新入表。
		h.pushFree(rem)
	}
	if recycled {
		h.stats.addReuse()
	}
	h.stats.addAlloc(int64(ab.size))
	return r.ptr(ab), nil
}

// free 释放 ptr 指向的块，返回可区分的错误。
func (h *heap) free(ptr unsafe.Pointer) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	p := uintptr(ptr)
	r := h.findRegion(p)
	if r == nil {
		return ErrInvalidPointer
	}
	b, ok := r.byOffset[int(p-r.base)]
	if !ok {
		// 落在区间内但不是块起始地址。
		return ErrInvalidPointer
	}
	if b.gen != h.gen {
		return ErrStalePointer
	}
	if b.free {
		return ErrDoubleFree
	}
	h.stats.addFree(int64(b.size))
	// 合并前必须先把空闲邻居从分级空闲表中摘除，
	// 否则被合并的块会留在空闲表中被重复分配。
	if b.prev != nil && b.prev.free {
		h.removeFree(b.prev)
	}
	if b.next != nil && b.next.free {
		h.removeFree(b.next)
	}
	merged := r.release(b)
	merged.recycled = true
	h.pushFree(merged)
	return nil
}

// reset 使全部存活块失效：所有区间合并为单一空闲块，代际递增。
func (h *heap) reset() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.gen++
	for i := range h.freeLists {
		h.freeLists[i] = h.freeLists[i][:0]
	}
	for _, r := range h.regions {
		r.byOffset = make(map[int]*block)
		b := &block{region: r, offset: 0, size: r.size, free: true}
		r.head = b
		r.byOffset[0] = b
		r.idleSince = time.Now()
		h.pushFree(b)
	}
}

// releaseIdle 把整体空闲且空闲时长超过 maxIdle 的区间交还底层，返回归还的区间数。
func (h *heap) releaseIdle(maxIdle time.Duration, now time.Time) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	released := 0
	kept := h.regions[:0]
	for _, r := range h.regions {
		if r.fullyFree() && now.Sub(r.idleSince) >= maxIdle {
			// 从空闲表中摘除该区间的唯一空闲块。
			ci := h.classFit(r.head.size)
			st := h.freeLists[ci]
			for i, b := range st {
				if b == r.head {
					h.freeLists[ci] = append(st[:i], st[i+1:]...)
					break
				}
			}
			h.release(r.size)
			released++
			continue
		}
		kept = append(kept, r)
	}
	// 清空被释放区间的引用，便于 GC。
	for i := len(kept); i < len(h.regions); i++ {
		h.regions[i] = nil
	}
	h.regions = kept
	return released
}
