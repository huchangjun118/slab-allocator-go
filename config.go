package slab

import (
	"errors"
	"fmt"
	"runtime"
)

// 分配器返回的可区分错误。
var (
	// ErrInvalidSize 表示申请尺寸非法（<=0）。
	ErrInvalidSize = errors.New("slab: invalid size")
	// ErrSizeTooLarge 表示申请尺寸超过最大分级。
	ErrSizeTooLarge = errors.New("slab: size exceeds largest class")
	// ErrOutOfMemory 表示达到总量上限，无法再扩展。
	ErrOutOfMemory = errors.New("slab: capacity limit reached")
	// ErrInvalidPointer 表示指针并非本分配器分配的块起始地址。
	ErrInvalidPointer = errors.New("slab: pointer not allocated by this allocator")
	// ErrDoubleFree 表示对同一块重复释放。
	ErrDoubleFree = errors.New("slab: double free")
	// ErrStalePointer 表示指针已被 Reset 失效。
	ErrStalePointer = errors.New("slab: pointer invalidated by reset")
)

// Config 描述分配器的可配置项。
//
// 所有字段在 NewAllocator 时校验，非法配置返回错误而不是静默降级。
type Config struct {
	// SizeClasses 为尺寸分级（字节），必须严格升序、每个值 > 0
	// 且为 Alignment 的整数倍。请求尺寸 n 会被路由到满足
	// class >= n 的最小分级；n 大于最大分级时 Alloc 返回 ErrSizeTooLarge。
	SizeClasses []int
	// Alignment 为返回地址的对齐粒度（字节），必须是 2 的幂且 >= 8。
	Alignment int
	// RegionSize 为单个底层区间的容量（字节），必须 >= 最大分级尺寸。
	RegionSize int
	// MaxTotalBytes 为所有区间总量上限（字节），必须 >= RegionSize；
	// 0 表示不限。达到上限后 Alloc 以 ErrOutOfMemory 失败。
	MaxTotalBytes int
	// NumHeaps 为并发分片数，不同尺寸分级按哈希路由到不同分片，
	// 从而降低跨分级争用；<= 0 时使用默认值。
	NumHeaps int
}

// 配置非法时返回的具体错误，可通过 errors.Is 判定。
var ErrInvalidConfig = errors.New("slab: invalid config")

func configErr(format string, args ...interface{}) error {
	return fmt.Errorf("%w: %s", ErrInvalidConfig, fmt.Sprintf(format, args...))
}

// validate 校验配置，任何非法或越界项都返回带原因的错误。
func (c *Config) validate() error {
	if c.Alignment == 0 {
		c.Alignment = 8
	}
	if c.Alignment < 8 || c.Alignment&(c.Alignment-1) != 0 {
		return configErr("Alignment must be a power of two and >= 8, got %d", c.Alignment)
	}
	if len(c.SizeClasses) == 0 {
		return configErr("SizeClasses must not be empty")
	}
	prev := 0
	for i, s := range c.SizeClasses {
		if s <= 0 {
			return configErr("SizeClasses[%d] must be > 0, got %d", i, s)
		}
		if s%c.Alignment != 0 {
			return configErr("SizeClasses[%d]=%d is not a multiple of Alignment=%d", i, s, c.Alignment)
		}
		if s <= prev {
			return configErr("SizeClasses must be strictly increasing, got %d after %d", s, prev)
		}
		prev = s
	}
	if c.RegionSize <= 0 {
		return configErr("RegionSize must be > 0, got %d", c.RegionSize)
	}
	if c.RegionSize < c.SizeClasses[len(c.SizeClasses)-1] {
		return configErr("RegionSize=%d is smaller than the largest size class %d",
			c.RegionSize, c.SizeClasses[len(c.SizeClasses)-1])
	}
	if c.MaxTotalBytes < 0 {
		return configErr("MaxTotalBytes must be >= 0, got %d", c.MaxTotalBytes)
	}
	if c.MaxTotalBytes > 0 && c.MaxTotalBytes < c.RegionSize {
		return configErr("MaxTotalBytes=%d is smaller than RegionSize=%d", c.MaxTotalBytes, c.RegionSize)
	}
	return nil
}

// applyDefaults 填充默认值（仅 NumHeaps 有默认值，其余缺失即非法）。
func (c *Config) applyDefaults() {
	if c.NumHeaps <= 0 {
		n := runtime.NumCPU()
		if n < 4 {
			n = 4
		}
		c.NumHeaps = n
	}
}

// classOf 返回能容纳 size 的最小分级下标，找不到返回 -1。
func (c *Config) classOf(size int) int {
	// 二分查找最小的 class >= size。
	lo, hi := 0, len(c.SizeClasses)
	for lo < hi {
		mid := (lo + hi) / 2
		if c.SizeClasses[mid] < size {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	if lo >= len(c.SizeClasses) {
		return -1
	}
	return lo
}
