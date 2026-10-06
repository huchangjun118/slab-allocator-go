# slab-allocator-go

一个仅依赖 Go 运行时与标准库的内存池分配器，面向对分配开销与碎片敏感的
小块内存场景。按尺寸分级管理内存块，支持并发、容量上限、按需增长、
空闲块合并复用、整体重置与统计查询。

## 特性

- **尺寸分级**：请求被路由到最小可容纳的分级，块按分级尺寸计费与复用。
- **对齐保证**：返回地址满足配置的对齐粒度（2 的幂，>= 8）。
- **块内切分与邻近合并**：大空闲块可切分出小块；相邻空闲块在释放时
  立即合并为更大的可复用区间，避免"总量够但无法满足中等尺寸申请"。
- **并发安全**：内部分为多个独立分片（heap），尺寸分级按哈希路由到
  不同分片，某一分级争用严重时不会阻塞无关分级；同一分片内操作串行化，
  行为确定。
- **容量上限与按需增长**：底层区间（region）按需创建，达到总量上限后
  以 `ErrOutOfMemory` 可区分地失败，不静默超发；长期空闲的区间可通过
  `ReleaseIdle` 交还底层。
- **整体重置与统计**：`Reset` 使全部存活块失效（之后释放旧指针返回
  `ErrStalePointer`），统计计数清零；`Stats` 返回累计申请、累计释放、
  当前占用、峰值占用与复用次数等指标。

## 使用示例

```go
a, err := slab.NewAllocator(slab.Config{
    SizeClasses:   []int{64, 128, 256, 512, 1024},
    Alignment:     8,
    RegionSize:    1 << 20,       // 单区间 1 MiB
    MaxTotalBytes: 64 << 20,      // 总量上限 64 MiB，0 表示不限
    NumHeaps:      0,             // 0 = 默认（>=4，随 CPU 数）
})
if err != nil { log.Fatal(err) }

p, err := a.Alloc(100) // 路由到 128 字节分级
if err != nil { /* ErrInvalidSize / ErrSizeTooLarge / ErrOutOfMemory */ }
// ... 使用 p ...
if err := a.Free(p); err != nil { /* ErrInvalidPointer / ErrDoubleFree / ErrStalePointer */ }

buf, err := a.AllocBytes(200) // 便捷切片形式
_ = buf

stats := a.Stats()       // 统计快照
a.ReleaseIdle(time.Minute) // 归还空闲超过 1 分钟的区间
a.Reset()                // 整体重置
```

## 复用语义

- **分级路由**：尺寸 `n` 路由到满足 `class >= n` 的最小分级；`n` 大于最大
  分级时 `Alloc` 返回 `ErrSizeTooLarge`，绝不会把请求塞入更小的块。
  恰好等于分级尺寸的请求使用该分级（边界行为确定）。
- **空闲块来源**：每个分级维护一个 LIFO 空闲块栈。申请时从恰好满足的
  分级向上查找（最佳适配），找到的第一个空闲块被复用；过大的空闲块会被
  切分，剩余部分重新进入对应分级的空闲表。
- **复用顺序**：同一分级内部按 LIFO（后释放先复用）的确定顺序复用；
  相同操作序列下，分配结果与全部统计指标逐条一致。
- **合并**：释放块会立即与前后相邻的空闲块合并，合并后的块按其实际大小
  进入相应分级，第三个特性保证长期运行后中等尺寸申请仍可被满足。
- **复用计数**：`Stats.ReuseCount` 只统计由"释放路径产生的空闲块"满足的
  分配；新区间首次切分不计入。

## 配置项

| 字段 | 含义 | 约束 |
| --- | --- | --- |
| `SizeClasses` | 尺寸分级（字节） | 非空、严格升序、每项为 `Alignment` 的整数倍 |
| `Alignment` | 对齐粒度（字节） | 2 的幂且 >= 8；0 表示默认 8 |
| `RegionSize` | 单个底层区间容量（字节） | > 0 且 >= 最大分级 |
| `MaxTotalBytes` | 全部区间总量上限（字节） | 0 表示不限；否则 >= `RegionSize` |
| `NumHeaps` | 并发分片数 | <= 0 表示默认值（>= 4，随 CPU 数） |

配置在 `NewAllocator` 时完整校验，任何非法或越界项都返回
`ErrInvalidConfig`（附具体原因），绝不静默降级。

## 错误

| 错误 | 触发场景 |
| --- | --- |
| `ErrInvalidSize` | 申请尺寸 <= 0 |
| `ErrSizeTooLarge` | 申请尺寸超过最大分级 |
| `ErrOutOfMemory` | 达到 `MaxTotalBytes` 上限且无可复用块 |
| `ErrInvalidPointer` | 指针非本分配器分配、或不是块起始地址、或为 nil |
| `ErrDoubleFree` | 对同一块重复释放 |
| `ErrStalePointer` | 释放已被 `Reset` 失效的指针 |
| `ErrInvalidConfig` | 配置非法（`NewAllocator` 返回） |

## 并发模型

- 不同分片（heap）各有独立的锁与空闲结构，互不阻塞。
- 尺寸分级按 `classIndex % NumHeaps` 路由到分片，因此无关分级之间的
  争用被隔离；同一分级的并发操作在分片锁内串行化。
- 任意时刻存活块之间互不重叠，单块之内的读写不会出现撕裂。

## 测试

```sh
go test ./...        # 单元测试
go test -race ./...  # 竞态检测
```

覆盖：并发申请释放、容量耗尽与恢复、碎片合并、非法/重复释放、
重置后失效识别、统计确定性、空闲区间归还与配置校验。
