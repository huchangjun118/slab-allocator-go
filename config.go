package slab

import (
	"fmt"
	"math/bits"
)

// Config 描述内存池的尺寸分级、对齐与容量策略。
//
// 字段语义：
//
//   - Alignment：所有 Alloc 返回地址的声明对齐（字节），必须为 2 的幂且 >= 1。
//   - Classes：升序、互不相同的尺寸分级边界（字节）。请求尺寸 size 命中第一个
//     满足 size <= Classes[i] 的分级，实际授予该分级的整块尺寸；因此每一级尺寸
//     必须是 Alignment 的整数倍。分级边界处的请求确定命中“等于该边界”的一级。
//   - RegionSize：单个底层区间容量（字节），必须为正，且至少容得下最大的分级。
//   - MaxCapacity：全部底层区间总容量上限（字节）；0 表示不限。为正时必须
//     至少容纳一个 RegionSize，否则池连首个区间都无法建立。
type Config struct {
	Alignment   int
	Classes     []int
	RegionSize  int
	MaxCapacity int
}

// DefaultConfig 返回一份开箱即用的默认配置：
//
//   - Alignment = 8
//   - Classes = [16 32 64 128 256 512 1024 2048 4096]
//   - RegionSize = 1 MiB
//   - MaxCapacity = 64 MiB
func DefaultConfig() Config {
	return Config{
		Alignment:   8,
		Classes:     []int{16, 32, 64, 128, 256, 512, 1024, 2048, 4096},
		RegionSize:  1 << 20,
		MaxCapacity: 64 << 20,
	}
}

// Validate 校验配置。返回的错误使用 fmt.Errorf("%w: ...", ErrInvalidConfig)
// 包装，调用方可用 errors.Is(err, ErrInvalidConfig) 判别，错误文本逐条解释
// 具体越界字段；本函数绝不静默修正或降级任何字段。
func (c Config) Validate() error {
	if c.Alignment < 1 || bits.OnesCount(uint(c.Alignment)) != 1 {
		return fmt.Errorf("%w: Alignment must be a positive power of two, got %d",
			ErrInvalidConfig, c.Alignment)
	}
	if len(c.Classes) == 0 {
		return fmt.Errorf("%w: Classes must contain at least one size class", ErrInvalidConfig)
	}
	prev := 0
	for i, s := range c.Classes {
		if s <= 0 {
			return fmt.Errorf("%w: Classes[%d] must be positive, got %d",
				ErrInvalidConfig, i, s)
		}
		if s%c.Alignment != 0 {
			return fmt.Errorf("%w: Classes[%d]=%d must be a multiple of Alignment %d",
				ErrInvalidConfig, i, s, c.Alignment)
		}
		if i > 0 && s <= prev {
			return fmt.Errorf("%w: Classes must be strictly ascending; Classes[%d]=%d <= %d",
				ErrInvalidConfig, i, s, prev)
		}
		prev = s
	}
	if c.RegionSize <= 0 {
		return fmt.Errorf("%w: RegionSize must be positive, got %d",
			ErrInvalidConfig, c.RegionSize)
	}
	// 块在区间内的最大跨度：最大分级尺寸按内部对齐粒度向上取整。
	align := c.internalAlign()
	max := c.Classes[len(c.Classes)-1]
	need := (max + align - 1) &^ (align - 1)
	if c.RegionSize < need {
		return fmt.Errorf("%w: RegionSize %d too small for largest class %d (need %d bytes incl. header/padding)",
			ErrInvalidConfig, c.RegionSize, max, need)
	}
	if c.MaxCapacity < 0 {
		return fmt.Errorf("%w: MaxCapacity must be zero (unlimited) or positive, got %d",
			ErrInvalidConfig, c.MaxCapacity)
	}
	if c.MaxCapacity > 0 && c.MaxCapacity < c.RegionSize {
		return fmt.Errorf("%w: MaxCapacity %d must be at least RegionSize %d",
			ErrInvalidConfig, c.MaxCapacity, c.RegionSize)
	}
	return nil
}

// internalAlign 返回内部使用的对齐粒度：声明对齐与 8 的较大者。
// 下限取 8 是为了让区间内的块起始地址在 32 位平台上也满足 64 位原子
// 操作的天然对齐要求；由于 8 是任意声明对齐的倍数或约数，返回地址对
// 声明对齐同样成立。
func (c Config) internalAlign() int {
	if c.Alignment < 8 {
		return 8
	}
	return c.Alignment
}
