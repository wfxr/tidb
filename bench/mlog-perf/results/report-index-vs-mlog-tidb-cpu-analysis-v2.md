# Mlog vs Non-Unique Index: TiDB CPU 差异根因分析

## TL;DR

Mlog 相比 non-unique index 多一些 TiDB CPU 消耗（44.4% vs 41.7%），分析其主因不是 mlog 缺少 reserve rowid 带来的额外分配开销，而是 **mlog 写入流程本身的固有结构性开销**——mlog 的写入不是 index-style 的轻量 KV 追加，而是对 `$mlog$...` 表执行一次完整的 `Table.AddRecord`，包括 handle 分配、行编码、mem-buffer staging、record key 生成等全套通用表插入流水线。`ReservedRowIDAlloc` 的 reset/restore 虽然存在，但底层 allocator 自带本地 cache，实际开销可忽略。

## Benchmark 关键数据

测试条件：`bc_bet_records` 基表，11 列 non-unique index vs 追踪相同 11 列的 mlog，均为 no-shard，pessimistic auto-commit，`batch_size=1`，128 threads / 18k rows/s。

| Metric | Baseline | + Index | + Mlog |
| --- | ---: | ---: | ---: |
| TiDB CPU | 35.8% | 41.7% (+16.5%) | 44.4% (+24.0%) |
| TiKV CPU | 26.3% | 39.4% | 38.4% |
| Disk KB/row | 9.57 | 16.73 | 13.71 |

两个关键信号：

1. Mlog TiDB CPU 更高，但 TiKV CPU 持平甚至略低
2. Mlog 磁盘写入反而少于 index

不过从绝对值看，两者的 TiDB CPU 差距仅 2.7 个百分点（44.4% vs 41.7%），整体开销都不算大。值得关注的不是差距本身的大小，而是它反映出的执行路径差异——这排除了差异由存储层 payload 大小或 TiKV 侧编码成本导致的可能性，指向 **TiDB 端执行路径本身的结构性差异**。

## 路径对比

### Non-unique index：index-specific fast path

```
base TableCommon.AddRecord
  └─ addIndices
       └─ FetchValues        // r[ic.Offset] 直接投影
       └─ GenIndexKey/Value  // 编码 index KV
       └─ MemBuffer.Set      // 写入
```

Index 维护不进入通用表插入路径，只是对已有 row 做投影 + 编码一个 index entry（[`index.go`][index-create]）。

### Mlog：second full table insert

```
mlogTable.AddRecord
  ├─ base Table.AddRecord    // 基表正常插入
  ├─ writeMLogRow            // 构造 mlog 行（datum 投影）
  └─ mlog Table.AddRecord    // 第二次通用表插入
```

Mlog 的 DDL 会创建一张独立物理表（tracked columns + `_MLOG$_DML_TYPE` / `_MLOG$_OLD_NEW`）。写入时，executor 层将基表包装为 [`mlogTable`][builder-wrap]，其 [`AddRecord`][mlog-addrecord] 在基表插入完成后，再对 mlog 表调用一次 [`Table.AddRecord`][mlog-write-addrecord]。

两次写入在同一事务内完成，不会多一轮 2PC。额外 CPU 主要来自 table layer 的通用写流程。

### Mlog 比 index 多出的环节

Index 的附加工作主要集中在 `FetchValues → GenIndexKey/Value → 写 mem-buffer` 这条 index-specific 路径，不进入通用表写路径；实际实现中还会带上 assertion 等 index 侧逻辑，但整体仍明显轻于一次完整的 `Table.AddRecord`。Mlog 的 `Table.AddRecord` 会进入 `TableCommon.addRecord` 通用流水线，各环节参与情况如下：

| 环节 | 说明 | 源码 |
| --- | --- | --- |
| Handle / RowID 分配 | `AllocHandle` → `AllocHandleIDs(1)` | [`tables.go:763`][tables-reserve] |
| 初始化 `encodeRowBuffer` | 为行编码分配 buffer | [`tables.go:787`][tables-encode-buf] |
| 建立 mem-buffer staging | `txn.GetMemBuffer().Staging()` | [`tables.go:789`][tables-staging] |
| 逐列遍历 writable columns | `checkDataForModifyColumn`、列状态判断、`canSkip`、`AddColVal` | [`tables.go:792-846`][tables-col-iter] |
| Row-level constraint check | 行级约束校验（空操作，mlog 无约束定义） | [`tables.go:848`][tables-constraint] |
| Dup key check | 检查 record key 是否重复（跳过，mlogOpts 设置了 `DupKeyCheckSkip`） | [`tables.go:853`][tables-dup-check] |
| 生成 record key | `t.RecordKey(recordID)` | [`tables.go:851`][tables-record-key] |
| 整行编码并写入 mem-buffer | `WriteMemBufferEncoded` | [`tables.go:887`][tables-encode-write] |
| 设置 assertion | `SetAssertNotExist` | [`tables.go:903`][tables-assertion] |
| 维护索引 | `addIndices`（空操作，mlog 无索引定义） | [`tables.go:913`][tables-addindices] |
| 更新统计信息 | `UpdatePhysicalTableDelta` | [`tables.go:929`][tables-stats] |

Index 主要是在既有 row 上做投影并编码 index KV；mlog 则需要走完上面整条通用表插入流水线。这就是 TiDB CPU 差异的结构性来源。

## 次要因素

### Datum 深拷贝

[`writeMLogRow()`][mlog-writemlogrow] 中对每个 tracked datum 调用 [`Datum.Copy()`][mlog-deep-copy]，对 string/bytes、`MyDecimal`、`MysqlTime` 类型会产生真实的内存分配和拷贝。本 benchmark 的 11 个 tracked 列中有 3 个字符串列、3 个 decimal 列、1 个日期列，均受影响。

但这只影响 mlog 行构造阶段，不改变后续更重的 [`Table.AddRecord`][mlog-write-addrecord] 流水线，因此即使优化掉深拷贝，也无法把 mlog 拉平到和 non-unique index 同一条成本曲线。

### ReservedRowIDAlloc

Mlog 写入前会临时 reset 基表的 [`ReservedRowIDAlloc`][mlog-rowid-reset]，避免误用基表预留的 row ID：

```go
base, maxv := alloc.Current()
defer alloc.Reset(base, maxv)
alloc.Reset(0, 0)
```

这段逻辑看起来像每行都多了一次状态切换，但它不是影响性能的主要因素，原因如下：

**直接开销极小。** [`ReservedRowIDAlloc`][stmtctx-alloc] 只有 `base`/`max` 两个 `int64` 字段，`Current()` 是两次读，`Reset()` 是两次写，`Consume()` 是比较加自增——全是常数级整数操作。

**间接影响也有限。** Reset 之后 mlog 的 `AllocHandle` 无法从基表的 statement-level reservation 中 `Consume()`，只能走底层 rowid allocator 的 [`Alloc()`][autoid-alloc]。但底层 allocator 自带本地 cache（[`autoid.go:893`][autoid-cache]），`Alloc(..., 1)` 优先从本地区间 `[base, end]` 取，只有区间耗尽时才会 `kv.RunInNewTxn()` 扩新段。每次扩段的批大小由 [`NextStep`][autoid-nextstep] 动态调整——目标是让每批 ID 大约够用 10 秒（`defaultConsumeTime`），结果 clamp 到 `[30000, 2000000]`：

```go
// autoid.go:560
func NextStep(curStep int64, consumeDur time.Duration) int64 {
	consumeRate := defaultConsumeTime.Seconds() / consumeDur.Seconds() // 10s / 实际消耗时间
	res := int64(float64(curStep) * consumeRate)
	if res < minStep { return minStep }  // 30000
	if res > maxStep { return maxStep }  // 2000000
	return res
}
```

以本次 benchmark 的 18k rows/s 为例，初始 `minStep=30000` 约 1.7 秒用完，下一批就会扩大到 ~176k，几轮后稳定在每批撑约 10 秒的水平。因此绝大多数行的 `Alloc` 只是一次本地整数自增，refill 频率极低。

综合来看，`ReservedRowIDAlloc` 的 reset/restore 是正确性必须的状态隔离逻辑，但在当前 benchmark 条件下不构成可观测的性能差异。

## 结论

Benchmark 中 TiDB CPU `+24.0%`（mlog）vs `+16.5%`（index）的差距，与当前实现结构高度一致：index 走的是 index-specific fast path（投影 + 编码 index KV），mlog 走的是 second generic table insert（完整的 [`TableCommon.addRecord`][tables-addrecord] 流水线）。局部优化（去掉深拷贝、优化 rowid alloc）能带来边际改善，但很难改变这一结构性差异。

## 关键源码入口

| 位置 | 说明 |
| --- | --- |
| [`pkg/executor/builder.go:2936`][builder-wrap] | `wrapTableWithMLogIfExists` |
| [`pkg/table/tables/mview_log.go:101`][mlog-wrap] | `WrapTableWithMaterializedViewLog` |
| [`pkg/table/tables/mview_log.go:192`][mlog-addrecord] | `mlogTable.AddRecord` |
| [`pkg/table/tables/mview_log.go:337`][mlog-writemlogrow] | `writeMLogRow` + 再次插表 |
| [`pkg/table/tables/tables.go:697`][tables-AddRecord] | `TableCommon.AddRecord`（public） |
| [`pkg/table/tables/tables.go:703`][tables-addrecord] | `TableCommon.addRecord`（private，实际流水线） |
| [`pkg/table/tables/tables.go:913`][tables-addindices] | `addIndices` |
| [`pkg/table/tables/index.go:194`][index-create] | `index.create` |
| [`pkg/ddl/executor.go:1047`][ddl-mlog] | `CreateMaterializedViewLog` |

<!-- GitHub permanent links (commit 85f88a3ff5) -->
[builder-wrap]: https://github.com/pingcap/tidb/blob/85f88a3ff5b33c566c40027f60bf455aee583159/pkg/executor/builder.go#L2936
[mlog-wrap]: https://github.com/pingcap/tidb/blob/85f88a3ff5b33c566c40027f60bf455aee583159/pkg/table/tables/mview_log.go#L101
[mlog-addrecord]: https://github.com/pingcap/tidb/blob/85f88a3ff5b33c566c40027f60bf455aee583159/pkg/table/tables/mview_log.go#L192
[mlog-writemlogrow]: https://github.com/pingcap/tidb/blob/85f88a3ff5b33c566c40027f60bf455aee583159/pkg/table/tables/mview_log.go#L337
[mlog-deep-copy]: https://github.com/pingcap/tidb/blob/85f88a3ff5b33c566c40027f60bf455aee583159/pkg/table/tables/mview_log.go#L354-L356
[mlog-rowid-reset]: https://github.com/pingcap/tidb/blob/85f88a3ff5b33c566c40027f60bf455aee583159/pkg/table/tables/mview_log.go#L360-L367
[mlog-write-addrecord]: https://github.com/pingcap/tidb/blob/85f88a3ff5b33c566c40027f60bf455aee583159/pkg/table/tables/mview_log.go#L369
[tables-AddRecord]: https://github.com/pingcap/tidb/blob/85f88a3ff5b33c566c40027f60bf455aee583159/pkg/table/tables/tables.go#L697
[tables-addrecord]: https://github.com/pingcap/tidb/blob/85f88a3ff5b33c566c40027f60bf455aee583159/pkg/table/tables/tables.go#L703
[tables-reserve]: https://github.com/pingcap/tidb/blob/85f88a3ff5b33c566c40027f60bf455aee583159/pkg/table/tables/tables.go#L763
[tables-encode-buf]: https://github.com/pingcap/tidb/blob/85f88a3ff5b33c566c40027f60bf455aee583159/pkg/table/tables/tables.go#L787
[tables-staging]: https://github.com/pingcap/tidb/blob/85f88a3ff5b33c566c40027f60bf455aee583159/pkg/table/tables/tables.go#L789
[tables-col-iter]: https://github.com/pingcap/tidb/blob/85f88a3ff5b33c566c40027f60bf455aee583159/pkg/table/tables/tables.go#L792-L846
[tables-constraint]: https://github.com/pingcap/tidb/blob/85f88a3ff5b33c566c40027f60bf455aee583159/pkg/table/tables/tables.go#L848
[tables-record-key]: https://github.com/pingcap/tidb/blob/85f88a3ff5b33c566c40027f60bf455aee583159/pkg/table/tables/tables.go#L851
[tables-encode-write]: https://github.com/pingcap/tidb/blob/85f88a3ff5b33c566c40027f60bf455aee583159/pkg/table/tables/tables.go#L887
[tables-dup-check]: https://github.com/pingcap/tidb/blob/85f88a3ff5b33c566c40027f60bf455aee583159/pkg/table/tables/tables.go#L853-L877
[tables-assertion]: https://github.com/pingcap/tidb/blob/85f88a3ff5b33c566c40027f60bf455aee583159/pkg/table/tables/tables.go#L903-L910
[tables-stats]: https://github.com/pingcap/tidb/blob/85f88a3ff5b33c566c40027f60bf455aee583159/pkg/table/tables/tables.go#L929-L931
[tables-addindices]: https://github.com/pingcap/tidb/blob/85f88a3ff5b33c566c40027f60bf455aee583159/pkg/table/tables/tables.go#L913
[index-create]: https://github.com/pingcap/tidb/blob/85f88a3ff5b33c566c40027f60bf455aee583159/pkg/table/tables/index.go#L194
[ddl-mlog]: https://github.com/pingcap/tidb/blob/85f88a3ff5b33c566c40027f60bf455aee583159/pkg/ddl/executor.go#L1047
[stmtctx-alloc]: https://github.com/pingcap/tidb/blob/85f88a3ff5b33c566c40027f60bf455aee583159/pkg/sessionctx/stmtctx/stmtctx.go#L110-L142
[autoid-alloc]: https://github.com/pingcap/tidb/blob/85f88a3ff5b33c566c40027f60bf455aee583159/pkg/meta/autoid/autoid.go#L712
[autoid-cache]: https://github.com/pingcap/tidb/blob/85f88a3ff5b33c566c40027f60bf455aee583159/pkg/meta/autoid/autoid.go#L893
[autoid-nextstep]: https://github.com/pingcap/tidb/blob/85f88a3ff5b33c566c40027f60bf455aee583159/pkg/meta/autoid/autoid.go#L560-L579
