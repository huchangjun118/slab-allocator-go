package slab

import (
	"testing"
	"unsafe"
)

// Alignment=1：地址仍 8 对齐（内部下限），任何尺寸均可。
func TestAlignmentOne(t *testing.T) {
	cfg := Config{Alignment: 1, Classes: []int{1, 3, 9, 27}, RegionSize: 4096, MaxCapacity: 4096}
	p, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []int{1, 2, 3, 9, 27} {
		b, err := p.Alloc(n)
		if err != nil {
			t.Fatal(err)
		}
		pv := uintptr(unsafe.Pointer(&b.Bytes()[0]))
		if pv%8 != 0 {
			t.Fatalf("size %d at %#x not 8-aligned", n, pv)
		}
	}
}

// ReleaseIdle 不得回收仍含存活块的区间。
func TestReleaseIdleKeepsLive(t *testing.T) {
	cfg := Config{Alignment: 8, Classes: []int{4096}, RegionSize: 4096, MaxCapacity: 0}
	p, _ := New(cfg)
	var blk []*Block
	for i := 0; i < 3; i++ {
		b, _ := p.Alloc(4096)
		blk = append(blk, b)
	}
	// 只释放两块，保留一块存活在第三个区间。
	_ = p.Free(blk[0])
	_ = p.Free(blk[1])
	released := p.ReleaseIdle()
	// 两个空闲区间可回收（保留一个 + 存活区间占用一个 => 释放 1 个）。
	if released != 4096 {
		t.Fatalf("released=%d want 4096", released)
	}
	if n := p.Stats().RegionCount; n != 2 {
		t.Fatalf("regions=%d want 2", n)
	}
	if err := p.Free(blk[2]); err != nil {
		t.Fatal(err)
	}
}

// 同一分级内空闲块按 LIFO 顺序确定复用；不同尺寸交错也保持各自 LIFO。
func TestLIFOOrder(t *testing.T) {
	p, _ := New(DefaultConfig())
	keys := make(map[*Block][2]int)
	var bs []*Block
	for i := 0; i < 5; i++ {
		b, _ := p.Alloc(64)
		keys[b] = [2]int{int(uintptr(unsafe.Pointer(&b.region.mem[0]))), b.off}
		bs = append(bs, b)
	}
	pos := make(map[[2]int]int)
	for i, b := range bs {
		pos[keys[b]] = i
	}
	for _, b := range bs {
		_ = p.Free(b)
	}
	for i := 4; i >= 0; i-- {
		b, err := p.Alloc(64)
		if err != nil {
			t.Fatal(err)
		}
		k := [2]int{int(uintptr(unsafe.Pointer(&b.region.mem[0]))), b.off}
		if pos[k] != i {
			t.Fatalf("reuse order: got orig idx %d want %d", pos[k], i)
		}
	}
}

// 块内切分余段可继续满足更小请求。
func TestSplittingWithinRegion(t *testing.T) {
	cfg := Config{Alignment: 8, Classes: []int{1024}, RegionSize: 4096, MaxCapacity: 4096}
	p, _ := New(cfg)
	b1, _ := p.Alloc(1024)
	b2, _ := p.Alloc(1024)
	b3, _ := p.Alloc(1024)
	b4, _ := p.Alloc(1024)
	if b1.region != b2.region || b2.region != b3.region || b3.region != b4.region {
		t.Fatal("expected all four in one region")
	}
	if b1.off != 0 || b2.off != 1024 || b3.off != 2048 || b4.off != 3072 {
		t.Fatalf("offsets: %d %d %d %d", b1.off, b2.off, b3.off, b4.off)
	}
}

// 交错释放后，合并只能发生在相邻空闲段之间；存活块内容不得被破坏。
func TestCoalescePreservesLive(t *testing.T) {
	cfg := Config{Alignment: 8, Classes: []int{512}, RegionSize: 4096, MaxCapacity: 4096}
	p, _ := New(cfg)
	blks := make([]*Block, 8)
	for i := range blks {
		blks[i], _ = p.Alloc(512)
		for j := range blks[i].Bytes() {
			blks[i].Bytes()[j] = byte(i*7 + j)
		}
	}
	// 释放 0,2,4,6；1,3,5,7 存活。
	for i := 0; i < 8; i += 2 {
		_ = p.Free(blks[i])
	}
	// 再释放 1（与 0、2 相邻但被存活边界限制：只与相邻空闲合并），
	// 随后重新申请若干块并校验存活块内容。
	_ = p.Free(blks[1])
	for i, b := range []*Block{blks[3], blks[5], blks[7]} {
		idx := []int{3, 5, 7}[i]
		for j := range b.Bytes() {
			if b.Bytes()[j] != byte(idx*7+j) {
				t.Fatalf("live block %d corrupted at %d: %d", idx, j, b.Bytes()[j])
			}
		}
	}
	// 触发归位合并（申请失败路径）。
	extra, err := p.Alloc(512)
	if err != nil {
		t.Fatal(err)
	}
	for i, b := range []*Block{blks[3], blks[5], blks[7]} {
		idx := []int{3, 5, 7}[i]
		for j := range b.Bytes() {
			if b.Bytes()[j] != byte(idx*7+j) {
				t.Fatalf("live block %d corrupted after drain at %d", idx, j)
			}
		}
	}
	_ = extra
}
