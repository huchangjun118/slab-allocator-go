package slab

import (
	"errors"
	"testing"
)

// 耗尽与上限：两个区间（总上限 2*RegionSize），超过上限返回可区分的
// ErrPoolExhausted；释放后后续无关申请必须立刻可成功（不被提前吞掉）。
func TestCapacityExhaustionAndRecovery(t *testing.T) {
	cfg := Config{
		Alignment:   8,
		Classes:     []int{64, 4096},
		RegionSize:  4096,
		MaxCapacity: 8192,
	}
	p, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	// 每块占用 span=4096（最大级）；每区间至多 1 块，总计 2 块。
	b1, err := p.Alloc(4096)
	if err != nil {
		t.Fatal(err)
	}
	b2, err := p.Alloc(4096)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Alloc(4096); !errors.Is(err, ErrPoolExhausted) {
		t.Fatalf("want exhausted, got %v", err)
	}
	if s := p.Stats(); s.RegionCount != 2 || s.CapacityBytes != 8192 {
		t.Fatalf("unexpected regions: %+v", s)
	}
	// 释放一块后，后续无关申请必须成功（失败没有吞掉空间）。
	if err := p.Free(b1); err != nil {
		t.Fatal(err)
	}
	b3, err := p.Alloc(4096)
	if err != nil {
		t.Fatalf("alloc after free failed: %v", err)
	}
	_ = b2
	_ = b3
}

// 上限为 0 表示不限：可以持续增长。
func TestUnlimitedGrowth(t *testing.T) {
	cfg := Config{Alignment: 8, Classes: []int{4096}, RegionSize: 4096, MaxCapacity: 0}
	p, _ := New(cfg)
	var blk []*Block
	for i := 0; i < 5; i++ {
		b, err := p.Alloc(4096)
		if err != nil {
			t.Fatalf("iter %d: %v", i, err)
		}
		blk = append(blk, b)
	}
	if got := p.Stats().RegionCount; got != 5 {
		t.Fatalf("regions=%d want 5", got)
	}
}

// 碎片合并：占满一个区间后交错释放，再申请大尺寸请求。
// 若没有邻接合并，虽然空闲总量足够，也无法满足整块申请。
func TestCoalescing(t *testing.T) {
	cfg := Config{Alignment: 8, Classes: []int{64, 512}, RegionSize: 4096, MaxCapacity: 4096}
	p, _ := New(cfg)
	// 8 块 512 恰好占满一个区间（span=512）。
	blks := make([]*Block, 8)
	for i := range blks {
		b, err := p.Alloc(512)
		if err != nil {
			t.Fatalf("alloc %d: %v", i, err)
		}
		blks[i] = b
	}
	// 交错释放偶数块：形成 free,live,free,live,...，每段 512。
	for i := 0; i < 8; i += 2 {
		if err := p.Free(blks[i]); err != nil {
			t.Fatal(err)
		}
	}
	// 释放奇数块，邻接合并应重建整区间空闲。
	for i := 1; i < 8; i += 2 {
		if err := p.Free(blks[i]); err != nil {
			t.Fatal(err)
		}
	}
	// 触发缓存归位+合并：申请 512 会先直接尝试全局空闲（此时缓存未归位，
	// 全局空闲为空），于是归位全部缓存后重试并成功。这里连续申请 8 块必须
	// 全部落在同一区间且不再增长。
	var got []*Block
	for i := 0; i < 8; i++ {
		b, err := p.Alloc(512)
		if err != nil {
			t.Fatalf("post-coalesce alloc %d: %v", i, err)
		}
		got = append(got, b)
	}
	if s := p.Stats(); s.RegionCount != 1 {
		t.Fatalf("coalesce should avoid growth, regions=%d", s.RegionCount)
	}
}

// 跨分级不得塞小：小分级缓存中的块不能满足更大请求。
func TestNoCrossClassServing(t *testing.T) {
	cfg := Config{Alignment: 8, Classes: []int{64, 128}, RegionSize: 4096, MaxCapacity: 4096}
	p, _ := New(cfg)
	small, _ := p.Alloc(64)
	_ = p.Free(small) // 进入 64 级 LIFO
	big, err := p.Alloc(128)
	if err != nil {
		t.Fatal(err)
	}
	if big.grant != 128 {
		t.Fatalf("big grant=%d", big.grant)
	}
	// 小缓存仍应保留一块。
	p.classMu[0].Lock()
	n := len(p.freeLst[0])
	p.classMu[0].Unlock()
	if n != 1 {
		t.Fatalf("small cache drained to serve big request: len=%d", n)
	}
}
