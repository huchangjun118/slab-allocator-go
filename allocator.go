package slab

import (
	"time"
	"unsafe"
)

// Allocator 为内存池分配器：按尺寸分级管理小块内存，
// 支持并发、容量上限、按需增长、合并复用与整体重置。
//
// 路由规则：尺寸 -> 最小可容纳分级 -> 分级哈希到某个并发分片。
// 不同分级通常落到不同分片，因此某一分级争用严重时不会阻塞无关分级。
type Allocator struct {
	cfg   Config
	heaps []*heap
	stats statsCounters
}

// NewAllocator 依据配置创建分配器。配置非法时返回错误，绝不静默降级。
func NewAllocator(cfg Config) (*Allocator, error) {
	cfg.applyDefaults()
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	a := &Allocator{cfg: cfg}
	max := int64(cfg.MaxTotalBytes)
	reserve := func(n int) bool {
		if max <= 0 {
			a.stats.addReserved(int64(n))
			return true
		}
		for {
			cur := a.stats.reservedBytes.Load()
			if cur+int64(n) > max {
				return false
			}
			if a.stats.reservedBytes.CompareAndSwap(cur, cur+int64(n)) {
				return true
			}
		}
	}
	release := func(n int) { a.stats.addReserved(-int64(n)) }
	a.heaps = make([]*heap, cfg.NumHeaps)
	for i := range a.heaps {
		a.heaps[i] = newHeap(&cfg, &a.stats, reserve, release)
	}
	return a, nil
}

// heapFor 返回分级 ci 对应的并发分片。
func (a *Allocator) heapFor(ci int) *heap {
	return a.heaps[ci%len(a.heaps)]
}

// Alloc 申请 size 字节的内存块，返回地址满足配置的对齐要求。
//
// 失败返回可区分的错误：ErrInvalidSize（size<=0）、
// ErrSizeTooLarge（超过最大分级）、ErrOutOfMemory（达到总量上限）。
func (a *Allocator) Alloc(size int) (unsafe.Pointer, error) {
	if size <= 0 {
		return nil, ErrInvalidSize
	}
	ci := a.cfg.classOf(size)
	if ci < 0 {
		return nil, ErrSizeTooLarge
	}
	return a.heapFor(ci).alloc(ci)
}

// AllocBytes 申请 size 字节并以切片形式返回，底层与 Alloc 相同。
func (a *Allocator) AllocBytes(size int) ([]byte, error) {
	p, err := a.Alloc(size)
	if err != nil {
		return nil, err
	}
	return unsafe.Slice((*byte)(p), size), nil
}

// Free 释放此前 Alloc 返回的指针。
//
// 非法指针返回 ErrInvalidPointer，重复释放返回 ErrDoubleFree，
// Reset 之后释放旧指针返回 ErrStalePointer，均不会静默接受。
func (a *Allocator) Free(ptr unsafe.Pointer) error {
	if ptr == nil {
		return ErrInvalidPointer
	}
	// 区间地址范围互不相交，逐个分片尝试归属；
	// 只有真正拥有该指针的分片会返回非 ErrInvalidPointer 的结果。
	for _, h := range a.heaps {
		err := h.free(ptr)
		if err != ErrInvalidPointer {
			return err
		}
	}
	return ErrInvalidPointer
}

// Reset 整体重置：全部存活块视为失效（之后释放旧指针返回 ErrStalePointer），
// 所有区间合并为完整空闲区间，统计计数清零（保留容量除外）。
func (a *Allocator) Reset() {
	for _, h := range a.heaps {
		h.reset()
	}
	a.stats.reset()
}

// Stats 返回统计快照。相同操作序列下各字段逐条确定。
func (a *Allocator) Stats() Stats {
	return a.stats.snapshot()
}

// ReleaseIdle 把整体空闲且空闲时长超过 maxIdle 的区间交还底层，
// 返回归还的区间数。maxIdle 为 0 表示归还所有整体空闲的区间。
func (a *Allocator) ReleaseIdle(maxIdle time.Duration) int {
	now := time.Now()
	n := 0
	for _, h := range a.heaps {
		n += h.releaseIdle(maxIdle, now)
	}
	return n
}
