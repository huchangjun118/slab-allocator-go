package slab

import (
	"errors"
	"sync"
	"testing"
	"unsafe"
)

// 在 Reset 高频交错下持续申请，校验每一批存活块的物理区间互不重叠。
// 若“空间授予”与“句柄登记”之间存在可被 Reset 插入的窗口，rs.reset 会把
// 已授予区间重新记为空闲并再次切给别人，本测试必能观测到重叠。
func TestNoDoubleAllocAcrossReset(t *testing.T) {
	cfg := Config{
		Alignment:   8,
		Classes:     []int{64, 256, 1024},
		RegionSize:  1 << 14,
		MaxCapacity: 1 << 18,
	}
	p, _ := New(cfg)
	var wg sync.WaitGroup
	stop := make(chan struct{})

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
				var bs []*Block
				for i := 0; i < 24; i++ {
					size := []int{64, 256, 1024}[i%3]
					b, err := p.Alloc(size)
					if err != nil {
						if errors.Is(err, ErrPoolExhausted) || errors.Is(err, ErrOversized) {
							continue
						}
						t.Errorf("alloc: %v", err)
						return
					}
					bs = append(bs, b)
				}
				// 在释放前快照并校验本批“仍存活”块区间互不重叠。
				type iv struct {
					base   uintptr
					lo, hi int
				}
				var ivs []iv
				for _, b := range bs {
					if !b.Valid() || b.gen != p.gen.Load() {
						continue
					}
					base := uintptr(unsafe.Pointer(&b.region.mem[0]))
					ivs = append(ivs, iv{base, b.off, b.off + b.span})
				}
				for i := 0; i < len(ivs); i++ {
					for j := i + 1; j < len(ivs); j++ {
						if ivs[i].base == ivs[j].base &&
							ivs[i].lo < ivs[j].hi && ivs[j].lo < ivs[i].hi {
							t.Errorf("overlapping live blocks across reset: %+v %+v",
								ivs[i], ivs[j])
							return
						}
					}
				}
				for _, b := range bs {
					_ = p.Free(b)
				}
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 500; i++ {
			p.Reset()
		}
		close(stop)
	}()
	wg.Wait()
}
