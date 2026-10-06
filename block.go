package slab

import "sync/atomic"

// blockState 为 Block 的生命周期状态。
type blockState uint32

const (
	stateLive blockState = 1 // 由用户持有
	stateFree blockState = 2 // 已释放，处于分级缓存或全局空闲结构
	stateDead blockState = 3 // 因池重置而失效
)

// Block 是一次成功分配返回的显式句柄。
//
// 通过句柄而非裸指针归还，使“不属于本池 / 非块首 / 重复释放 / 重置后使用”
// 等非法情形都能被精确识别，无需在用户数据区前后写入内联头部，也就不存在
// 内联头部被用户数据覆盖、伪造或 ABA 复用导致的静默误判。
type Block struct {
	pool   *Pool
	region *region
	off    int    // span 在 region.mem 内的起始偏移
	span   int    // 实际占用跨度（内部对齐粒度向上取整）
	grant  int    // 所属分级尺寸
	size   int    // 调用方请求尺寸
	gen    uint64 // 分配时的池代际
	state  atomic.Uint32
}

// Bytes 返回可读写的用户数据切片，长度为申请尺寸，容量等于所属分级尺寸。
// 块因池重置失效后返回 nil；块被正常 Free 后继续读写不被保证（空间可能已
// 复用给其他存活块）。
func (b *Block) Bytes() []byte {
	if blockState(b.state.Load()) == stateDead {
		return nil
	}
	return b.region.mem[b.off:][:b.size:b.grant]
}

// Valid 报告句柄当前是否为可正常使用的存活块。
func (b *Block) Valid() bool {
	return blockState(b.state.Load()) == stateLive
}
