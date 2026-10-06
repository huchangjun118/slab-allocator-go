package slab

import "errors"

// 分配器以哨兵错误的形式显式拒绝各类非法操作，调用方可通过 errors.Is 判别。
var (
	// ErrInvalidConfig 表示配置非法或越界。
	ErrInvalidConfig = errors.New("slab: invalid config")
	// ErrInvalidArgument 表示入参非法（如非正尺寸）。
	ErrInvalidArgument = errors.New("slab: invalid argument")
	// ErrOversized 表示申请尺寸超过单个底层区间可容纳的上限。
	ErrOversized = errors.New("slab: allocation size exceeds region size")
	// ErrPoolExhausted 表示已达到总量上限且无可用空间，分配被拒绝。
	ErrPoolExhausted = errors.New("slab: pool capacity exhausted")
	// ErrInvalidPointer 表示释放的地址不属于本池或不是块首地址。
	ErrInvalidPointer = errors.New("slab: pointer not owned by pool")
	// ErrDoubleFree 表示该块已被释放，拒绝重复释放。
	ErrDoubleFree = errors.New("slab: double free")
	// ErrUseAfterReset 表示块在池重置后已失效。
	ErrUseAfterReset = errors.New("slab: pointer invalidated by reset")
)
