package slab

import (
	"errors"
	"math/rand"
	"sort"
	"sync"
	"testing"
	"unsafe"
)

// testConfig 返回一个确定性测试配置（单分片，行为完全串行确定）。
func testConfig() Config {
	return Config{
		SizeClasses: []int{64, 128, 256, 512, 1024},
		Alignment:   8,
		RegionSize:  4096,
		NumHeaps:    1,
	}
}

func mustNew(t *testing.T, cfg Config) *Allocator {
	t.Helper()
	a, err := NewAllocator(cfg)
	if err != nil {
		t.Fatalf("NewAllocator: %v", err)
	}
	return a
}

func TestAllocAlignmentAndNonOverlap(t *testing.T) {
	a := mustNew(t, testConfig())
	const n = 200
	ptrs := make([]uintptr, 0, n)
	for i := 0; i < n; i++ {
		p, err := a.Alloc(64)
		if err != nil {
			t.Fatalf("Alloc: %v", err)
		}
		u := uintptr(p)
		if u%8 != 0 {
			t.Fatalf("pointer %x not 8-byte aligned", u)
		}
		ptrs = append(ptrs, u)
	}
	sort.Slice(ptrs, func(i, j int) bool { return ptrs[i] < ptrs[j] })
	for i := 1; i < len(ptrs); i++ {
		if ptrs[i] < ptrs[i-1]+64 {
			t.Fatalf("live blocks overlap: %x and %x", ptrs[i-1], ptrs[i])
		}
	}
}

func TestAlignment16(t *testing.T) {
	cfg := testConfig()
	cfg.Alignment = 16
	cfg.SizeClasses = []int{16, 32, 64}
	a := mustNew(t, cfg)
	for i := 0; i < 50; i++ {
		p, err := a.Alloc(16)
		if err != nil {
			t.Fatalf("Alloc: %v", err)
		}
		if uintptr(p)%16 != 0 {
			t.Fatalf("pointer %x not 16-byte aligned", uintptr(p))
		}
	}
}

func TestSizeClassRoutingAndBoundaries(t *testing.T) {
	a := mustNew(t, testConfig())
	cases := []struct {
		req      int
		wantBill int64 // 期望计入占用的分级尺寸
	}{
		{1, 64}, {63, 64}, {64, 64}, // 边界：恰好等于分级尺寸用本分级
		{65, 128}, {128, 128},
		{129, 256}, {1024, 1024},
	}
	var ptrs []unsafe.Pointer
	for _, c := range cases {
		before := a.Stats().CurrentBytes
		p, err := a.Alloc(c.req)
		if err != nil {
			t.Fatalf("Alloc(%d): %v", c.req, err)
		}
		if got := a.Stats().CurrentBytes - before; got != c.wantBill {
			t.Fatalf("Alloc(%d) billed %d bytes, want %d", c.req, got, c.wantBill)
		}
		ptrs = append(ptrs, p)
	}
	for _, p := range ptrs {
		if err := a.Free(p); err != nil {
			t.Fatalf("Free: %v", err)
		}
	}
}

func TestInvalidSizes(t *testing.T) {
	a := mustNew(t, testConfig())
	if _, err := a.Alloc(0); !errors.Is(err, ErrInvalidSize) {
		t.Fatalf("Alloc(0) = %v, want ErrInvalidSize", err)
	}
	if _, err := a.Alloc(-8); !errors.Is(err, ErrInvalidSize) {
		t.Fatalf("Alloc(-8) = %v, want ErrInvalidSize", err)
	}
	// 超过最大分级：可区分的失败，不得塞入更小的块。
	if _, err := a.Alloc(1025); !errors.Is(err, ErrSizeTooLarge) {
		t.Fatalf("Alloc(1025) = %v, want ErrSizeTooLarge", err)
	}
}

func TestIllegalAndDoubleFree(t *testing.T) {
	a := mustNew(t, testConfig())
	p, err := a.Alloc(64)
	if err != nil {
		t.Fatalf("Alloc: %v", err)
	}
	// 块内部地址不是合法块起始。
	if err := a.Free(unsafe.Pointer(uintptr(p) + 8)); !errors.Is(err, ErrInvalidPointer) {
		t.Fatalf("Free(interior) = %v, want ErrInvalidPointer", err)
	}
	// 完全无关的指针。
	var x int
	if err := a.Free(unsafe.Pointer(&x)); !errors.Is(err, ErrInvalidPointer) {
		t.Fatalf("Free(foreign) = %v, want ErrInvalidPointer", err)
	}
	if err := a.Free(nil); !errors.Is(err, ErrInvalidPointer) {
		t.Fatalf("Free(nil) = %v, want ErrInvalidPointer", err)
	}
	if err := a.Free(p); err != nil {
		t.Fatalf("Free: %v", err)
	}
	// 重复释放必须被明确拒绝。
	if err := a.Free(p); !errors.Is(err, ErrDoubleFree) {
		t.Fatalf("Free(double) = %v, want ErrDoubleFree", err)
	}
}

func TestReuseOrderIsDeterministicLIFO(t *testing.T) {
	a := mustNew(t, testConfig())
	// 用 p2、p4 把 p1、p3 与两侧空闲空间隔开，避免释放时触发合并，
	// 使两个空闲块同处一个分级空闲表。
	p1, _ := a.Alloc(64)
	p2, _ := a.Alloc(64)
	p3, _ := a.Alloc(64)
	p4, _ := a.Alloc(64)
	if err := a.Free(p1); err != nil {
		t.Fatal(err)
	}
	if err := a.Free(p3); err != nil {
		t.Fatal(err)
	}
	// 同一分级内部按 LIFO 确定顺序复用：先复用最后释放的 p3。
	q1, _ := a.Alloc(64)
	if q1 != p3 {
		t.Fatalf("first reuse got %v, want %v (LIFO)", q1, p3)
	}
	q2, _ := a.Alloc(64)
	if q2 != p1 {
		t.Fatalf("second reuse got %v, want %v (LIFO)", q2, p1)
	}
	if got := a.Stats().ReuseCount; got != 2 {
		t.Fatalf("ReuseCount = %d, want 2", got)
	}
	a.Free(p2)
	a.Free(p4)
	a.Free(q1)
	a.Free(q2)
}

func TestCoalescedBlockReusedFromFront(t *testing.T) {
	a := mustNew(t, testConfig())
	p1, _ := a.Alloc(64)
	p2, _ := a.Alloc(64)
	a.Free(p1)
	a.Free(p2)
	// 相邻释放块合并后，再分配应从合并块起始（即 p1）处切出。
	q, _ := a.Alloc(64)
	if q != p1 {
		t.Fatalf("reuse after coalescing got %v, want %v", q, p1)
	}
}

func TestCoalescingSatisfiesMediumAlloc(t *testing.T) {
	cfg := testConfig()
	cfg.MaxTotalBytes = 4096 // 仅允许一个区间，逼出碎片场景
	a := mustNew(t, cfg)

	// 用 64 字节小块填满整个区间。
	var ptrs []unsafe.Pointer
	for {
		p, err := a.Alloc(64)
		if err != nil {
			if !errors.Is(err, ErrOutOfMemory) {
				t.Fatalf("Alloc: %v", err)
			}
			break
		}
		ptrs = append(ptrs, p)
	}
	if len(ptrs) != 64 {
		t.Fatalf("filled %d blocks of 64B in 4096B region, want 64", len(ptrs))
	}
	// 区间被 64B 块占满时，1024B 申请必须失败（容量上限，可区分）。
	if _, err := a.Alloc(1024); !errors.Is(err, ErrOutOfMemory) {
		t.Fatalf("Alloc(1024) on full region = %v, want ErrOutOfMemory", err)
	}
	// 全部释放后邻近空闲块合并为完整区间，中等尺寸申请必须成功。
	for _, p := range ptrs {
		if err := a.Free(p); err != nil {
			t.Fatalf("Free: %v", err)
		}
	}
	if _, err := a.Alloc(1024); err != nil {
		t.Fatalf("Alloc(1024) after coalescing: %v", err)
	}
}

func TestCapacityLimitAndRecovery(t *testing.T) {
	cfg := testConfig()
	cfg.MaxTotalBytes = 4096
	a := mustNew(t, cfg)

	p1, err := a.Alloc(1024)
	if err != nil {
		t.Fatalf("Alloc: %v", err)
	}
	// 占满剩余空间。
	var rest []unsafe.Pointer
	for {
		p, err := a.Alloc(512)
		if err != nil {
			if !errors.Is(err, ErrOutOfMemory) {
				t.Fatalf("Alloc: %v", err)
			}
			break
		}
		rest = append(rest, p)
	}
	// 超限失败不得吞掉后续无关申请：释放一块后立即可再分配。
	if err := a.Free(p1); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Alloc(1024); err != nil {
		t.Fatalf("Alloc after freeing space: %v", err)
	}
	for _, p := range rest {
		if err := a.Free(p); err != nil {
			t.Fatal(err)
		}
	}
}

func TestResetInvalidatesLiveBlocks(t *testing.T) {
	a := mustNew(t, testConfig())
	p, err := a.Alloc(128)
	if err != nil {
		t.Fatal(err)
	}
	a.Reset()
	// 重置前的指针必须被识别为失效。
	if err := a.Free(p); !errors.Is(err, ErrStalePointer) {
		t.Fatalf("Free after Reset = %v, want ErrStalePointer", err)
	}
	// 统计清零。
	st := a.Stats()
	if st.TotalAllocs != 0 || st.TotalFrees != 0 || st.CurrentBytes != 0 || st.PeakBytes != 0 {
		t.Fatalf("stats not cleared after Reset: %+v", st)
	}
	// 重置后可继续正常使用。
	if _, err := a.Alloc(128); err != nil {
		t.Fatalf("Alloc after Reset: %v", err)
	}
}

func TestStatsDeterministicForSameSequence(t *testing.T) {
	run := func() Stats {
		a := mustNew(t, testConfig())
		rng := rand.New(rand.NewSource(42))
		var live []unsafe.Pointer
		for i := 0; i < 2000; i++ {
			op := rng.Intn(2)
			if op == 0 || len(live) == 0 {
				size := 1 << (6 + rng.Intn(5)) // 64..1024
				p, err := a.Alloc(size)
				if err != nil {
					t.Fatalf("Alloc: %v", err)
				}
				live = append(live, p)
			} else {
				j := rng.Intn(len(live))
				if err := a.Free(live[j]); err != nil {
					t.Fatalf("Free: %v", err)
				}
				live = append(live[:j], live[j+1:]...)
			}
		}
		return a.Stats()
	}
	s1, s2 := run(), run()
	if s1 != s2 {
		t.Fatalf("same sequence produced different stats:\n%+v\n%+v", s1, s2)
	}
}

func TestStatsFields(t *testing.T) {
	a := mustNew(t, testConfig())
	p1, _ := a.Alloc(64)
	p2, _ := a.Alloc(128)
	st := a.Stats()
	if st.TotalAllocs != 2 || st.CurrentBlocks != 2 || st.CurrentBytes != 64+128 {
		t.Fatalf("unexpected stats: %+v", st)
	}
	if st.PeakBytes != 64+128 {
		t.Fatalf("PeakBytes = %d, want %d", st.PeakBytes, 64+128)
	}
	if st.ReservedBytes != 4096 {
		t.Fatalf("ReservedBytes = %d, want 4096", st.ReservedBytes)
	}
	a.Free(p1)
	st = a.Stats()
	if st.TotalFrees != 1 || st.CurrentBytes != 128 || st.PeakBytes != 192 {
		t.Fatalf("unexpected stats after free: %+v", st)
	}
	// 复用：再次申请 64 应命中空闲块。
	a.Alloc(64)
	if got := a.Stats().ReuseCount; got != 1 {
		t.Fatalf("ReuseCount = %d, want 1", got)
	}
	a.Free(p2)
}

func TestReleaseIdleReturnsMemory(t *testing.T) {
	cfg := testConfig()
	cfg.MaxTotalBytes = 4096 * 4
	a := mustNew(t, cfg)
	var ptrs []unsafe.Pointer
	for i := 0; i < 100; i++ {
		p, err := a.Alloc(128)
		if err != nil {
			t.Fatal(err)
		}
		ptrs = append(ptrs, p)
	}
	for _, p := range ptrs {
		if err := a.Free(p); err != nil {
			t.Fatal(err)
		}
	}
	if got := a.Stats().ReservedBytes; got <= 0 {
		t.Fatalf("ReservedBytes = %d, want > 0", got)
	}
	n := a.ReleaseIdle(0)
	if n == 0 {
		t.Fatal("ReleaseIdle(0) released nothing")
	}
	if got := a.Stats().ReservedBytes; got != 0 {
		t.Fatalf("ReservedBytes = %d after ReleaseIdle, want 0", got)
	}
	// 归还后按需重新增长。
	if _, err := a.Alloc(128); err != nil {
		t.Fatalf("Alloc after ReleaseIdle: %v", err)
	}
}

func TestConfigValidation(t *testing.T) {
	base := testConfig()
	cases := []struct {
		name   string
		mutate func(*Config)
	}{
		{"empty classes", func(c *Config) { c.SizeClasses = nil }},
		{"zero class", func(c *Config) { c.SizeClasses = []int{0, 64} }},
		{"unsorted classes", func(c *Config) { c.SizeClasses = []int{128, 64} }},
		{"duplicate classes", func(c *Config) { c.SizeClasses = []int{64, 64} }},
		{"class not aligned", func(c *Config) { c.SizeClasses = []int{70} }},
		{"bad alignment", func(c *Config) { c.Alignment = 7 }},
		{"alignment too small", func(c *Config) { c.Alignment = 4 }},
		{"zero region", func(c *Config) { c.RegionSize = 0 }},
		{"region smaller than max class", func(c *Config) { c.RegionSize = 512 }},
		{"negative max total", func(c *Config) { c.MaxTotalBytes = -1 }},
		{"max total smaller than region", func(c *Config) { c.MaxTotalBytes = 2048 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base
			tc.mutate(&cfg)
			if _, err := NewAllocator(cfg); !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("NewAllocator = %v, want ErrInvalidConfig", err)
			}
		})
	}
}

func TestConcurrentAllocFree(t *testing.T) {
	cfg := Config{
		SizeClasses: []int{16, 32, 64, 128, 256},
		Alignment:   8,
		RegionSize:  64 * 1024,
		// 默认分片数，覆盖跨分级不互相阻塞的路径。
	}
	a := mustNew(t, cfg)

	const goroutines = 8
	const iters = 2000

	var wg sync.WaitGroup
	lives := make([][]unsafe.Pointer, goroutines)
	errs := make([]error, goroutines)
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(int64(g) + 1))
			var live []unsafe.Pointer
			for i := 0; i < iters; i++ {
				if len(live) == 0 || rng.Intn(2) == 0 {
					size := 1 << (4 + rng.Intn(5)) // 16..256
					p, err := a.Alloc(size)
					if err != nil {
						errs[g] = err
						return
					}
					// 写入本块，验证单块读写不撕裂。
					b := unsafe.Slice((*byte)(p), size)
					for j := range b {
						b[j] = byte(g)
					}
					live = append(live, p)
				} else {
					j := rng.Intn(len(live))
					if err := a.Free(live[j]); err != nil {
						errs[g] = err
						return
					}
					live = append(live[:j], live[j+1:]...)
				}
			}
			lives[g] = live
		}(g)
	}
	wg.Wait()
	for g, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: %v", g, err)
		}
	}

	// 汇总所有存活块，验证互不重叠。
	type span struct{ lo, hi uintptr }
	var spans []span
	for _, live := range lives {
		for _, p := range live {
			u := uintptr(p)
			spans = append(spans, span{u, u + 256}) // 上界取最大分级
		}
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i].lo < spans[j].lo })
	for i := 1; i < len(spans); i++ {
		if spans[i].lo == spans[i-1].lo {
			t.Fatal("duplicate allocation detected")
		}
	}

	// 全部释放后统计归零。
	for _, live := range lives {
		for _, p := range live {
			if err := a.Free(p); err != nil {
				t.Fatalf("Free: %v", err)
			}
		}
	}
	st := a.Stats()
	if st.CurrentBytes != 0 || st.CurrentBlocks != 0 {
		t.Fatalf("leaked after freeing all: %+v", st)
	}
	if st.TotalAllocs != st.TotalFrees {
		t.Fatalf("TotalAllocs=%d != TotalFrees=%d", st.TotalAllocs, st.TotalFrees)
	}
}
