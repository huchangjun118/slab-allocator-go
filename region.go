package slab

import (
	"sort"
	"sync"
	"unsafe"
)

// freeSeg 描述某个底层区间内部的一段空闲字节区间 [off, off+size)。
//
// reclaimed 标记该段（或其切分余段）是否来自曾经授予过用户、后又归还的
// 空间。合并后的段只要任一来源是回收空间即为 true；从未使用过的初始段与
// 纯切分余段为 false。该标记为“复用次数”统计提供逐条确定的口径。
type freeSeg struct {
	off       int
	size      int
	reclaimed bool
}

// region 是一块由运行时分配、首地址对齐到内部对齐粒度的连续底层内存。
type region struct {
	id    int
	mem   []byte    // len == cap == RegionSize
	raw   []byte    // make 出来的原始切片，仅用于保活
	frees []freeSeg // 按 off 升序排列，互不交叠且已合并
	live  int       // 当前由用户持有的存活块数（不含分级缓存中的块）
}

// regionSet 管理全部底层区间与跨分级的空闲结构，以一把锁串行化。
// 这把锁与各分级自己的锁相互独立：命中分级缓存的快路径完全不触碰它。
type regionSet struct {
	mu      sync.Mutex
	regions []*region
	cap     int64
	nextID  int
}

// newRegionSet 创建空区间集合。
func newRegionSet() *regionSet {
	return &regionSet{}
}

// allocate 在全部区间的空闲结构中为 span 字节的请求取一段确定位置。
//
// 确定性复用规则：在所有区间、所有空闲段中选择“最佳适配”——长度最小的
// 可容纳段；长度相同时取区间 id 更小者；再相同取段内偏移更小者。段内从
// 低偏移切分，余段保留并继承 reclaimed 标记。返回 reused 表示命中的是否
// 为回收空间。调用方必须持有 rs.mu。
func (rs *regionSet) allocate(span int) (r *region, off int, reused bool) {
	var bestR *region
	bestIdx, bestSize := -1, 0
	for _, cand := range rs.regions {
		for i := range cand.frees {
			s := cand.frees[i].size
			if s < span {
				continue
			}
			if bestR == nil || s < bestSize ||
				(s == bestSize && (cand.id < bestR.id ||
					(cand.id == bestR.id && cand.frees[i].off < cand.frees[bestIdx].off))) {
				bestR, bestIdx, bestSize = cand, i, s
			}
		}
	}
	if bestR == nil {
		return nil, 0, false
	}
	seg := bestR.frees[bestIdx]
	off, reused = seg.off, seg.reclaimed
	if seg.size == span {
		bestR.frees = append(bestR.frees[:bestIdx], bestR.frees[bestIdx+1:]...)
	} else {
		bestR.frees[bestIdx].off += span
		bestR.frees[bestIdx].size -= span
		// 余段继承回收标记。
		bestR.frees[bestIdx].reclaimed = seg.reclaimed
	}
	bestR.live++
	return bestR, off, reused
}

// insertFree 把一段空闲区间并入 r 的空闲表（含前后邻接合并），不改动 live
// 计数。适用于分级缓存归位——这些块进入缓存时 live 已减过。新并入的段来自
// 已归还块，因此标记为回收空间；与纯新余段合并后同样视为回收空间。
// 调用方必须持有 rs.mu。
func (rs *regionSet) insertFree(r *region, off, size int) {
	seg := freeSeg{off: off, size: size, reclaimed: true}
	i := sort.Search(len(r.frees), func(i int) bool { return r.frees[i].off >= off })
	if i < len(r.frees) && off+size == r.frees[i].off {
		seg.size += r.frees[i].size
		seg.reclaimed = seg.reclaimed || r.frees[i].reclaimed
		r.frees = append(r.frees[:i], r.frees[i+1:]...)
	}
	if i > 0 && r.frees[i-1].off+r.frees[i-1].size == off {
		r.frees[i-1].size += seg.size
		r.frees[i-1].reclaimed = r.frees[i-1].reclaimed || seg.reclaimed
	} else {
		r.frees = append(r.frees, freeSeg{})
		copy(r.frees[i+1:], r.frees[i:])
		r.frees[i] = seg
	}
}

// grow 追加一个容量为 regionSize、首地址对齐到 align 的新区间；超过 maxCap
// 时返回 ErrPoolExhausted。maxCap 为 0 表示不限。调用方必须持有 rs.mu。
func (rs *regionSet) grow(regionSize, align, maxCap int) (*region, error) {
	if maxCap > 0 && rs.cap+int64(regionSize) > int64(maxCap) {
		return nil, ErrPoolExhausted
	}
	raw := make([]byte, regionSize+align-1)
	addr := uintptr(unsafe.Pointer(&raw[0]))
	pad := int((align - int(addr%uintptr(align))) % align)
	r := &region{
		id:    rs.nextID,
		raw:   raw,
		mem:   raw[pad : pad+regionSize : pad+regionSize],
		frees: []freeSeg{{off: 0, size: regionSize, reclaimed: false}},
	}
	rs.nextID++
	rs.regions = append(rs.regions, r)
	rs.cap += int64(regionSize)
	return r, nil
}

// reset 作废全部存活块与空闲记录。调用方必须已清空全部分级缓存并持有
// rs.mu。区间本身保留（容量保留），全部容量重新记为单段、未回收空闲。
func (rs *regionSet) reset() {
	for _, r := range rs.regions {
		r.live = 0
		r.frees = []freeSeg{{off: 0, size: len(r.mem), reclaimed: false}}
	}
}

// releaseIdle 释放完全空闲（live==0 且空闲表恰为覆盖整个区间的单段）的底层
// 区间；若调用将导致一个区间不剩，则保留最后一个符合条件的区间。返回释放
// 字节数。调用前必须已把全部分级缓存归位，且持有 rs.mu。
func (rs *regionSet) releaseIdle() int64 {
	var released int64
	kept := rs.regions[:0]
	// 先统计可释放数量，保证至少保留一个区间。
	fullyFree := 0
	for _, r := range rs.regions {
		if r.live == 0 && len(r.frees) == 1 &&
			r.frees[0].off == 0 && r.frees[0].size == len(r.mem) {
			fullyFree++
		}
	}
	allow := fullyFree - 1
	for _, r := range rs.regions {
		if allow > 0 && r.live == 0 && len(r.frees) == 1 &&
			r.frees[0].off == 0 && r.frees[0].size == len(r.mem) {
			allow--
			released += int64(len(r.mem))
			rs.cap -= int64(len(r.mem))
			continue
		}
		kept = append(kept, r)
	}
	rs.regions = kept
	return released
}
