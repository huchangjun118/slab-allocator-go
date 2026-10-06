package slab

import (
	"time"
	"unsafe"
)

// block 描述区间内一个连续块。块元数据保存在 Go 堆中，
// 不占用用户数据区。所有字段仅在所属 heap 的锁内访问。
type block struct {
	region   *region // 所属区间
	offset   int     // 相对区间数据区起始的偏移（对齐粒度的整数倍）
	size     int     // 块总大小（含内部碎片）
	free     bool    // 是否空闲
	recycled bool    // 是否来自释放路径（用于复用统计，区别于新区间的首次切分）
	gen      uint64  // 最近一次分配时的代际，用于识别 Reset 后的失效指针
	prev     *block  // 地址序前驱
	next     *block  // 地址序后继
}

// region 为底层连续区间，内部以地址序双向链表 + 偏移索引管理块，
// 支持切分与邻近空闲块合并。
type region struct {
	data      []byte         // 底层存储（保留引用防止 GC）
	off       int            // 数据区起始在 data 中的下标（对齐后）
	base      uintptr        // 数据区对齐后的起始地址
	size      int            // 可用字节数（对齐向下取整后）
	head      *block         // 按地址序的块链表头
	byOffset  map[int]*block // 块起始偏移 -> 块
	idleSince time.Time      // 整个区间变为全空闲的时刻；非全空闲时为零值
}

// newRegion 创建容量至少为 size 字节的区间，数据区起始地址按 alignment 对齐。
func newRegion(size, alignment int) *region {
	buf := make([]byte, size+alignment)
	start := uintptr(unsafe.Pointer(&buf[0]))
	aligned := (start + uintptr(alignment) - 1) &^ (uintptr(alignment) - 1)
	// 可用大小向下取整到对齐粒度；缓冲区预留了 alignment 字节，
	// 因此 aligned-start+usable <= len(buf) 恒成立。
	usable := size &^ (alignment - 1)
	r := &region{
		data:     buf,
		off:      int(aligned - start),
		base:     aligned,
		size:     usable,
		byOffset: make(map[int]*block),
	}
	b := &block{region: r, offset: 0, size: usable, free: true}
	r.head = b
	r.byOffset[0] = b
	r.idleSince = time.Now()
	return r
}

// contains 报告 ptr 是否落在本区间数据范围内。
func (r *region) contains(ptr uintptr) bool {
	return ptr >= r.base && ptr < r.base+uintptr(r.size)
}

// addr 返回块起始地址。
func (r *region) addr(b *block) uintptr {
	return r.base + uintptr(b.offset)
}

// ptr 返回块起始地址的 unsafe.Pointer 形式。
func (r *region) ptr(b *block) unsafe.Pointer {
	return unsafe.Pointer(&r.data[r.off+b.offset])
}

// insertAfter 在链表中 at 之后插入 b，并登记索引。
func (r *region) insertAfter(at, b *block) {
	b.prev = at
	b.next = at.next
	if at.next != nil {
		at.next.prev = b
	}
	at.next = b
	r.byOffset[b.offset] = b
}

// unlink 从链表与索引中移除 b。
func (r *region) unlink(b *block) {
	if b.prev != nil {
		b.prev.next = b.next
	} else {
		r.head = b.next
	}
	if b.next != nil {
		b.next.prev = b.prev
	}
	delete(r.byOffset, b.offset)
}

// carve 把空闲块 b 的前 size 字节切出作为已分配块；
// 剩余部分若 >= minRemainder 则切分为新的空闲块并作为第二个返回值返回，
// 否则剩余空间整体并入已分配块（第二个返回值为 nil）。
// 调用前提：b.free 且 b.size >= size。
func (r *region) carve(b *block, size, minRemainder int, gen uint64) (allocated, remainder *block) {
	rem := b.size - size
	if rem >= minRemainder {
		nb := &block{region: r, offset: b.offset + size, size: rem, free: true}
		r.insertAfter(b, nb)
		b.size = size
		remainder = nb
	}
	b.free = false
	b.gen = gen
	r.idleSince = time.Time{}
	return b, remainder
}

// release 释放块 b 并与前后邻近空闲块合并，返回合并后的空闲块。
func (r *region) release(b *block) *block {
	b.free = true
	// 与后继合并。
	if n := b.next; n != nil && n.free {
		b.size += n.size
		r.unlink(n)
	}
	// 与前驱合并。
	if p := b.prev; p != nil && p.free {
		p.size += b.size
		r.unlink(b)
		b = p
	}
	// 区间是否已整体空闲。
	if b.prev == nil && b.next == nil {
		r.idleSince = time.Now()
	}
	return b
}

// fullyFree 报告区间是否没有任何存活块。
func (r *region) fullyFree() bool {
	return r.head != nil && r.head.next == nil && r.head.free
}
