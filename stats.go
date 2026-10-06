package slab

// Stats 为内存池的统计快照，所有字段在相同操作序列下逐条确定。
type Stats struct {
	// TotalAlloc 为累计成功申请次数。
	TotalAlloc uint64
	// TotalFree 为累计成功释放次数。
	TotalFree uint64
	// LiveBlocks 为当前存活块数量。
	LiveBlocks uint64
	// UsedBytes 为当前存活块占用的授予字节数（按所属分级尺寸计）。
	UsedBytes uint64
	// PeakBytes 为历史峰值占用字节数。
	PeakBytes uint64
	// ReuseCount 为复用已回收空间的申请次数。
	ReuseCount uint64
	// RegionCount 为当前持有的底层区间数量。
	RegionCount int
	// CapacityBytes 为当前底层区间总容量。
	CapacityBytes int64
}
