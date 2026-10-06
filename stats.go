package slab

import "sync/atomic"

// Stats 为分配器统计快照。
//
// 在固定随机种子与相同操作序列下，各字段逐条确定。
type Stats struct {
	TotalAllocs   int64 // 累计申请次数
	TotalFrees    int64 // 累计释放次数
	CurrentBytes  int64 // 当前占用字节（按分级尺寸计）
	PeakBytes     int64 // 峰值占用字节
	ReuseCount    int64 // 复用次数：由空闲块（而非新切分空间）满足的分配数
	CurrentBlocks int64 // 当前存活块数
	ReservedBytes int64 // 当前向底层保留的区间总字节
}

// statsCounters 为内部原子计数器集合。
type statsCounters struct {
	totalAllocs   atomic.Int64
	totalFrees    atomic.Int64
	currentBytes  atomic.Int64
	peakBytes     atomic.Int64
	reuseCount    atomic.Int64
	currentBlocks atomic.Int64
	reservedBytes atomic.Int64
}

// addAlloc 记录一次成功分配，并维护峰值。
func (s *statsCounters) addAlloc(bytes int64) {
	s.totalAllocs.Add(1)
	s.currentBlocks.Add(1)
	cur := s.currentBytes.Add(bytes)
	for {
		peak := s.peakBytes.Load()
		if cur <= peak || s.peakBytes.CompareAndSwap(peak, cur) {
			break
		}
	}
}

// addFree 记录一次成功释放。
func (s *statsCounters) addFree(bytes int64) {
	s.totalFrees.Add(1)
	s.currentBlocks.Add(-1)
	s.currentBytes.Add(-bytes)
}

// addReuse 记录一次空闲块复用。
func (s *statsCounters) addReuse() {
	s.reuseCount.Add(1)
}

// addReserved 调整保留字节（delta 可正可负）。
func (s *statsCounters) addReserved(delta int64) {
	s.reservedBytes.Add(delta)
}

// snapshot 返回一致性足够查询用的快照（各字段独立原子读取）。
func (s *statsCounters) snapshot() Stats {
	return Stats{
		TotalAllocs:   s.totalAllocs.Load(),
		TotalFrees:    s.totalFrees.Load(),
		CurrentBytes:  s.currentBytes.Load(),
		PeakBytes:     s.peakBytes.Load(),
		ReuseCount:    s.reuseCount.Load(),
		CurrentBlocks: s.currentBlocks.Load(),
		ReservedBytes: s.reservedBytes.Load(),
	}
}

// reset 清零全部计数（ReservedBytes 除外，由调用方按实际保留量重设）。
func (s *statsCounters) reset() {
	s.totalAllocs.Store(0)
	s.totalFrees.Store(0)
	s.currentBytes.Store(0)
	s.peakBytes.Store(0)
	s.reuseCount.Store(0)
	s.currentBlocks.Store(0)
}
