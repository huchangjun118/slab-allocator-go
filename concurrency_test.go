package slab

import (
	"sync"
	"testing"
)

// 并发申请/释放：不得损坏空闲结构、不得产生重复分配。在 -race 下运行。
func TestConcurrentAllocFree(t *testing.T) {
	cfg := Config{
		Alignment:   8,
		Classes:     []int{16, 32, 64, 128},
		RegionSize:  1 << 16,
		MaxCapacity: 1 << 20,
	}
	p, _ := New(cfg)
	const workers = 16
	const rounds = 200
	var wg sync.WaitGroup
	errCh := make(chan error, workers)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			held := make([]*Block, 0, 16)
			for r := 0; r < rounds; r++ {
				size := 16 << ((r + id) % 4)
				b, err := p.Alloc(size)
				if err != nil {
					errCh <- err
					return
				}
				d := b.Bytes()
				for i := range d {
					d[i] = byte(id + r)
				}
				held = append(held, b)
				if len(held) == 16 {
					for _, x := range held {
						if err := p.Free(x); err != nil {
							errCh <- err
							return
						}
					}
					held = held[:0]
				}
			}
			for _, x := range held {
				if err := p.Free(x); err != nil {
					errCh <- err
					return
				}
			}
		}(w)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}
	s := p.Stats()
	if s.LiveBlocks != 0 || s.UsedBytes != 0 {
		t.Fatalf("leftover: %+v", s)
	}
	if s.TotalAlloc != s.TotalFree {
		t.Fatalf("alloc/free mismatch: %d vs %d", s.TotalAlloc, s.TotalFree)
	}
}

// 并发重复释放：同一块被多个 goroutine 释放，恰好一个成功，其余 ErrDoubleFree。
func TestConcurrentDoubleFree(t *testing.T) {
	p, _ := New(DefaultConfig())
	b, _ := p.Alloc(64)
	const n = 16
	var wg sync.WaitGroup
	ok, dbl := make(chan struct{}, 1), make(chan int, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := p.Free(b)
			switch {
			case err == nil:
				ok <- struct{}{}
			default:
				dbl <- 1
			}
		}()
	}
	wg.Wait()
	close(ok)
	close(dbl)
	if len(ok) != 1 {
		t.Fatalf("successful frees = %d want 1", len(ok))
	}
	if len(dbl) != n-1 {
		t.Fatalf("double-free rejections = %d want %d", len(dbl), n-1)
	}
}

// ReleaseIdle：全部释放后缓存归位、整区间空闲，应能把增长出的区间交还。
func TestReleaseIdle(t *testing.T) {
	cfg := Config{Alignment: 8, Classes: []int{4096}, RegionSize: 4096, MaxCapacity: 0}
	p, _ := New(cfg)
	var blk []*Block
	for i := 0; i < 4; i++ {
		b, _ := p.Alloc(4096)
		blk = append(blk, b)
	}
	if s := p.Stats(); s.RegionCount != 4 {
		t.Fatalf("pre: regions=%d", s.RegionCount)
	}
	for _, b := range blk {
		if err := p.Free(b); err != nil {
			t.Fatal(err)
		}
	}
	// 此时块都在分级缓存中，底层各区间 live==0 但空闲表未覆盖全区间。
	released := p.ReleaseIdle()
	if released != 3*4096 {
		t.Fatalf("released=%d want %d", released, 3*4096)
	}
	if s := p.Stats(); s.RegionCount != 1 || s.CapacityBytes != 4096 {
		t.Fatalf("post: %+v", s)
	}
	// 归还后仍可正常分配（保留区间）。
	b, err := p.Alloc(4096)
	if err != nil {
		t.Fatalf("alloc after release: %v", err)
	}
	_ = b
}

// 统计确定性：相同的串行操作序列下两个池的逐条统计完全一致。
func TestStatsDeterminism(t *testing.T) {
	seq := func(p *Pool) []Stats {
		var snap []Stats
		b1, _ := p.Alloc(100)
		snap = append(snap, p.Stats())
		b2, _ := p.Alloc(200)
		snap = append(snap, p.Stats())
		b3, _ := p.Alloc(60)
		snap = append(snap, p.Stats())
		_ = p.Free(b2)
		snap = append(snap, p.Stats())
		b4, _ := p.Alloc(200) // 复用 b2
		snap = append(snap, p.Stats())
		_ = p.Free(b1)
		_ = p.Free(b3)
		_ = p.Free(b4)
		snap = append(snap, p.Stats())
		return snap
	}
	p1, _ := New(DefaultConfig())
	p2, _ := New(DefaultConfig())
	s1, s2 := seq(p1), seq(p2)
	if len(s1) != len(s2) {
		t.Fatal("snapshot length differs")
	}
	for i := range s1 {
		if s1[i] != s2[i] {
			t.Fatalf("snapshot %d differs:\n%+v\n%+v", i, s1[i], s2[i])
		}
	}
	last := s1[len(s1)-1]
	if last.TotalAlloc != 4 || last.TotalFree != 4 ||
		last.ReuseCount != 1 || last.PeakBytes != 448 ||
		last.LiveBlocks != 0 || last.UsedBytes != 0 {
		t.Fatalf("unexpected final stats: %+v", last)
	}
}
