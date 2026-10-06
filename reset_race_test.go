package slab

import (
	"errors"
	"sync"
	"testing"
)

// Reset 与并发申请/释放交错：不崩溃、无内部竞态；旧句柄的 Free 必须得到
// 明确的 ErrUseAfterReset 或 ErrDoubleFree，绝不静默成功。
//
// 注意：本测试不在块内写用户数据。Reset 与“仍在写旧块数据”并发属于调用方
// 必须自行同步的场景（重置后旧内存按设计会复用给新块）；数据完整性由
// TestCoalescePreservesLive 与 TestBasicAllocAlignmentNoOverlap 等串行测试覆盖。
func TestConcurrentReset(t *testing.T) {
	cfg := Config{
		Alignment:   8,
		Classes:     []int{16, 64, 256},
		RegionSize:  1 << 14,
		MaxCapacity: 1 << 18,
	}
	p, _ := New(cfg)
	var wg sync.WaitGroup
	stop := make(chan struct{})

	// 多个 allocator：申请后立即释放，期间不触碰用户数据区。
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				var held []*Block
				for i := 0; i < 12; i++ {
					size := []int{16, 64, 256}[i%3]
					b, err := p.Alloc(size)
					if err != nil {
						if errors.Is(err, ErrPoolExhausted) || errors.Is(err, ErrOversized) {
							continue
						}
						t.Errorf("unexpected alloc err: %v", err)
						return
					}
					held = append(held, b)
				}
				for _, b := range held {
					if err := p.Free(b); err != nil &&
						!errors.Is(err, ErrUseAfterReset) &&
						!errors.Is(err, ErrDoubleFree) {
						t.Errorf("unexpected free err: %v", err)
						return
					}
				}
			}
		}()
	}

	// 周期性重置。
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			p.Reset()
		}
		close(stop)
	}()

	wg.Wait()
	p.Reset()
	if s := p.Stats(); s.LiveBlocks != 0 {
		t.Fatalf("live after final reset: %+v", s)
	}
}
