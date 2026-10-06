package slab

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"unsafe"
)

func alignedPtr(b *Block, a int) bool {
	p := uintptr(unsafe.Pointer(&b.Bytes()[0]))
	return p%uintptr(a) == 0
}

// 基础：分配可读写、地址按声明对齐、存活块区间互不重叠。
func TestBasicAllocAlignmentNoOverlap(t *testing.T) {
	p, err := New(DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	var blocks []*Block
	for _, size := range []int{1, 8, 16, 17, 100, 4096} {
		for j := 0; j < 8; j++ {
			b, err := p.Alloc(size)
			if err != nil {
				t.Fatalf("Alloc(%d): %v", size, err)
			}
			if !alignedPtr(b, 8) {
				t.Fatalf("size %d returned unaligned %p", size, b.Bytes())
			}
			for i := 0; i < size; i++ {
				b.Bytes()[i] = byte(i)
			}
			blocks = append(blocks, b)
		}
	}
	// 不重叠：以 (region, off, span) 区间两两校验。
	type iv struct {
		r      *region
		lo, hi int
	}
	ivs := make([]iv, len(blocks))
	for i, b := range blocks {
		ivs[i] = iv{b.region, b.off, b.off + b.span}
	}
	for i := 0; i < len(ivs); i++ {
		for j := i + 1; j < len(ivs); j++ {
			if ivs[i].r == ivs[j].r && ivs[i].lo < ivs[j].hi && ivs[j].lo < ivs[i].hi {
				t.Fatalf("overlap: %+v %+v", ivs[i], ivs[j])
			}
		}
	}
	// 数据完整性（无撕裂/覆盖）。
	for _, b := range blocks {
		for i := 0; i < b.size; i++ {
			if b.Bytes()[i] != byte(i) {
				t.Fatalf("corrupt at %d: got %d want %d", i, b.Bytes()[i], byte(i))
			}
		}
	}
}

// 尺寸边界：size 恰好等于某级边界时命中该级，绝不塞入更小分级；
// 大于最大分级则 ErrOversized。
func TestClassBoundaries(t *testing.T) {
	p, _ := New(DefaultConfig())
	for _, c := range []int{16, 32, 64, 128, 256, 512, 1024, 2048, 4096} {
		b, err := p.Alloc(c)
		if err != nil {
			t.Fatal(err)
		}
		if b.grant != c {
			t.Fatalf("Alloc(%d) grant=%d want %d", c, b.grant, c)
		}
		if cap(b.Bytes()) != c {
			t.Fatalf("Alloc(%d) cap=%d want %d", c, cap(b.Bytes()), c)
		}
	}
	b, err := p.Alloc(4097)
	if !errors.Is(err, ErrOversized) {
		t.Fatalf("want ErrOversized, got %v (b=%v)", err, b)
	}
	if _, err := p.Alloc(0); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("want ErrInvalidArgument, got %v", err)
	}
}

// 非默认对齐（16）同样必须成立。
func TestCustomAlignment(t *testing.T) {
	cfg := Config{Alignment: 16, Classes: []int{16, 64, 256}, RegionSize: 4096, MaxCapacity: 8192}
	p, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, size := range []int{1, 16, 17, 200, 256} {
		b, err := p.Alloc(size)
		if err != nil {
			t.Fatal(err)
		}
		if !alignedPtr(b, 16) {
			t.Fatalf("size %d not 16-aligned: %p", size, b.Bytes())
		}
	}
}

func TestAllocFreeReuse(t *testing.T) {
	p, _ := New(DefaultConfig())
	b1, _ := p.Alloc(64)
	r1, off1 := b1.region, b1.off
	if err := p.Free(b1); err != nil {
		t.Fatal(err)
	}
	b2, _ := p.Alloc(64)
	if b2.region != r1 || b2.off != off1 {
		t.Fatalf("LIFO reuse failed: got (%p,%d) want (%p,%d)",
			b2.region, b2.off, r1, off1)
	}
	s := p.Stats()
	if s.ReuseCount != 1 || s.TotalAlloc != 2 || s.TotalFree != 1 {
		t.Fatalf("stats wrong: %+v", s)
	}
}

// 非法释放与重复释放必须被明确拒绝。
func TestInvalidAndDoubleFree(t *testing.T) {
	p, _ := New(DefaultConfig())
	if err := p.Free(nil); !errors.Is(err, ErrInvalidPointer) {
		t.Fatalf("nil free: %v", err)
	}
	other, _ := New(DefaultConfig())
	b, _ := p.Alloc(16)
	if err := other.Free(b); !errors.Is(err, ErrInvalidPointer) {
		t.Fatalf("foreign pool free: %v", err)
	}
	if err := p.Free(b); err != nil {
		t.Fatalf("first free: %v", err)
	}
	if err := p.Free(b); !errors.Is(err, ErrDoubleFree) {
		t.Fatalf("double free: %v", err)
	}
}

func TestResetInvalidates(t *testing.T) {
	p, _ := New(DefaultConfig())
	b, _ := p.Alloc(32)
	p.Reset()
	if b.Valid() {
		t.Fatal("block still valid after reset")
	}
	if b.Bytes() != nil {
		t.Fatal("Bytes non-nil after reset")
	}
	if err := p.Free(b); !errors.Is(err, ErrUseAfterReset) {
		t.Fatalf("free after reset: %v", err)
	}
	// 重置后池可继续正常分配。
	b2, err := p.Alloc(32)
	if err != nil {
		t.Fatal(err)
	}
	if !b2.Valid() {
		t.Fatal("new block invalid after reset")
	}
	s := p.Stats()
	if s.TotalAlloc != 1 || s.LiveBlocks != 1 {
		t.Fatalf("stats not reset: %+v", s)
	}
}

// 配置非法时必须返回 ErrInvalidConfig，且不静默降级。
func TestInvalidConfigs(t *testing.T) {
	bad := []Config{
		{Alignment: 3, Classes: []int{16}, RegionSize: 4096},                 // 非 2 幂
		{Alignment: 8, Classes: nil, RegionSize: 4096},                       // 无分级
		{Alignment: 8, Classes: []int{12}, RegionSize: 4096},                 // 非对齐倍数
		{Alignment: 8, Classes: []int{32, 16}, RegionSize: 4096},             // 非升序
		{Alignment: 8, Classes: []int{16}, RegionSize: 4096, MaxCapacity: 1}, // 上限太小
		{Alignment: 8, Classes: []int{4096}, RegionSize: 64},                 // 区间太小
	}
	for i, c := range bad {
		if _, err := New(c); !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("case %d: want ErrInvalidConfig, got %v", i, err)
		}
	}
}

var _ = fmt.Sprintf
var _ sync.Locker
var _ = unsafe.Sizeof(0)
