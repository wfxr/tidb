# Mlog vs Non-Unique Index: TiDB CPU Gap Investigation Report

**Date**: 2026-03-17  
**Scope**: current branch source analysis + benchmark `results/report-index-vs-mlog-noshard.md`  
**Audience**: TiDB / Executor / Table Layer reviewers

## Executive Summary

在相同基表、相同 11 列追踪范围、相同 `no-shard` 条件下，benchmark 显示：

- non-unique index 的 TiDB CPU 为 `41.7%`，相对 baseline 为 `+16.5%`
- mlog 的 TiDB CPU 为 `44.4%`，相对 baseline 为 `+24.0%`
- 两者 TiKV CPU 接近，且 mlog 的磁盘写入反而更低

这说明问题不在于 mlog 写到了更多字节，也不主要来自 TiKV 侧编码成本，而是在 TiDB 侧走了一条更重的 DML 执行路径。

**结论摘要：**

1. `mlog` 当前不是按“维护一个附加索引”的方式实现，而是按“在基表写入成功后，再对 `$mlog$...` 表执行一次普通 `Table.AddRecord`”的方式实现。
2. 因此，`mlog` 的额外开销本质上是 **第二次通用表插入**，而不是一次 index-style 的轻量 KV 增量维护。
3. 这条第二次 `AddRecord` 路径会重复执行 row ID / handle、行编码、约束检查、record key 生成、mem-buffer staging / assertion / statistics 等通用表写流程，其 TiDB CPU 成本显著高于 non-unique index 的 `FetchValues + GenIndexKey/Value + MemBuffer.Set`。
4. `ReservedRowIDAlloc` 相关逻辑在 mlog 路径中确实存在，但其直接开销只是少量整数读写；在本次 `batch=1` benchmark 下，它不是能够解释 TiDB CPU gap 的主导因素。
5. `writeMLogRow()` 中的 datum 深拷贝确实有额外成本，但它只占整条 mlog 写路径的前置小段，不是本次 TiDB CPU gap 的主因。

## 1. Background and Scope

本文聚焦于以下现象：

> 对同一张基表，添加一个非 unique 二级索引与添加一个 mlog 后，`mlog` 在 TiDB 侧 CPU 更高。

分析目标是解释 **当前实现下 `mlog` 为什么在 TiDB CPU 维度高于 non-unique index**，而不是对两种机制做抽象层面的优劣比较。

需要明确一个边界：

- 本报告直接解释的是本次 benchmark 所覆盖的 **INSERT 写路径**
- `UPDATE` / `DELETE` 场景下，`mlogTable.UpdateRecord()` / `RemoveRecord()` 的行为会不同，应单独分析

## 2. Benchmark Observation

参考报告：`bench/mlog-perf/results/report-index-vs-mlog-noshard.md`

测试条件：

- 基表：`bc_bet_records`
- 比较对象一：11 列 non-unique index
- 比较对象二：追踪相同 11 列的 `CREATE MATERIALIZED VIEW LOG`
- 基表、索引、mlog 均为 `no-shard`
- 事务模式：pessimistic auto-commit
- INSERT 形态：`batch_size=1` 的单行 insert
- 负载：128 threads，18,000 rows/s 限流

关键结果如下：

| Metric | Baseline | + Index | + Mlog |
| --- | ---: | ---: | ---: |
| TiDB CPU | 35.8% | 41.7% | 44.4% |
| TiDB CPU vs baseline | - | +16.5% | +24.0% |
| TiKV CPU | 26.3% | 39.4% | 38.4% |
| Disk KB/row | 9.57 | 16.73 | 13.71 |
| Prewrite/s | 34,893 | 50,142 | 52,861 |

从结果看有两个关键信号：

1. `mlog` 的 TiDB CPU 高于 index，但 TiKV CPU 并没有更高。
2. `mlog` 的磁盘写入少于 index，但 TiDB CPU 反而更高。

这两个信号共同指向：**差异主要来自 TiDB 端执行路径，而不是存储层 payload 大小。**

## 3. Code Paths Reviewed

本次分析主要落在以下源码路径：

- `pkg/executor/builder.go`
- `pkg/table/tables/mview_log.go`
- `pkg/table/tables/tables.go`
- `pkg/table/tables/index.go`
- `pkg/ddl/executor.go`

分析主要围绕两个问题展开：

1. mlog 的插入链路与普通 secondary index 维护链路，在 TiDB 内部分别落到哪些函数。
2. 这些函数分别承担哪些工作，其中哪些工作更可能构成 TiDB CPU 主开销。

## 4. How Mlog Is Implemented in Current Branch

### 4.1 Executor Layer: base table is wrapped by an mlog-aware table

在 executor 构建 DML 执行器时，如果基表存在 mlog，`executorBuilder.wrapTableWithMLogIfExists()` 会把原始 `table.Table` 包装成一个 `mlogTable`：

- `pkg/executor/builder.go:2936`
- `pkg/table/tables/mview_log.go:101`

`WrapTableWithMaterializedViewLog()` 的返回对象仍然实现 `table.Table` 接口，但它重写了 DML 相关方法，使基表写入后会同步追加 mlog 行。

### 4.2 Mlog AddRecord path is "base insert + mlog insert"

`mlogTable.AddRecord()` 的核心逻辑非常直接：

1. 先调用基表的 `t.Table.AddRecord(...)`
2. 再根据 tracked columns 构造 `mlogRow`
3. 最后对 mlog 表调用 `t.mlog.AddRecord(...)`

对应源码：

- `pkg/table/tables/mview_log.go:192`
- `pkg/table/tables/mview_log.go:204`
- `pkg/table/tables/mview_log.go:217`
- `pkg/table/tables/mview_log.go:367`

这意味着 mlog 的额外写入不是 index maintenance，而是 **对另一张表的第二次普通表写入**。

需要强调的是，这次 mlog 写入仍然发生在**同一个用户事务**里，不是单独起一个新事务；额外 CPU 来自同一事务内多执行了一次表写流程，而不是额外多做一轮独立 2PC。

### 4.3 Mlog table is a regular table with tracked columns plus meta columns

`CREATE MATERIALIZED VIEW LOG` 在 DDL 层会为 mlog 构造一张独立物理表，其列包括：

- 用户声明的 tracked columns
- `_MLOG$_DML_TYPE`
- `_MLOG$_OLD_NEW`

对应源码：

- `pkg/ddl/executor.go:1047`
- `pkg/ddl/executor.go:1088`
- `pkg/ddl/executor.go:1105`
- `pkg/ddl/executor.go:1124`

因此，从 table layer 的视角看，写 mlog 并不是“给当前表多写一个 index entry”，而是“给另一张表插入一行”。

## 5. Non-Unique Index Maintenance Path

普通 non-unique index 的附加开销发生在基表 `AddRecord()` 的尾部：

- `pkg/table/tables/tables.go:697`
- `pkg/table/tables/tables.go:912`
- `pkg/table/tables/tables.go:954`

执行流程如下：

1. 基表先正常完成一次 `TableCommon.addRecord()`
2. 在 `addIndices()` 中遍历 writable indexes
3. 对每个 index 调用 `FetchValues()`，从原始 `r []Datum` 中投影出索引列
4. 调用 `index.create()`
5. 在 `index.create()` 中执行 `GenIndexKey()` / `GenIndexValue()`
6. 最后 `txn.GetMemBuffer().Set(key, val)`

对应源码：

- `pkg/table/tables/index.go:722`
- `pkg/table/tables/index.go:194`
- `pkg/table/tables/index.go:221`
- `pkg/table/tables/index.go:273`
- `pkg/table/tables/index.go:280`

这里的关键点是：**index 维护没有第二次进入通用表插入路径。**

它只是在已有 base row 上投影出 index columns，然后生成 index key/value 写入 mem-buffer。

## 6. Side-by-Side Path Comparison

可以把两条链路简化成下面的对比：

### 6.1 Non-unique index

```text
base TableCommon.AddRecord
  -> addIndices
    -> FetchValues
    -> index.create
      -> GenIndexKey / GenIndexValue
      -> MemBuffer.Set
```

### 6.2 Mlog

```text
wrapped table AddRecord
  -> base Table.AddRecord
  -> writeMLogRow
  -> mlog Table.AddRecord
```

两者的根本差异不在于“都多写一个附加结构”，而在于：

- index 额外执行的是一条 **index-specific** fast path
- mlog 额外执行的是一条 **generic table insert** path

这就是本次 TiDB CPU 差异的结构性来源。

## 7. Why "Second Full Table Insert" Dominates TiDB CPU

这里的 “second full table insert” 需要精确定义：

- 不是说把基表 42 列重新插入一遍
- 而是说对 mlog 表再次调用了通用 `Table.AddRecord`
- 这次调用虽然只处理 11 个 tracked 列加 2 个 meta 列，但它会重复走一整套 **通用表写流程**

### 7.1 `TableCommon.addRecord()` is a broad, generic pipeline

`pkg/table/tables/tables.go:703` 开始的 `TableCommon.addRecord()` 不是一个窄函数，而是一条完整的表行写入流水线。其主要工作包括：

1. 构造 `AddRecordOpt`，进入 tracing region
2. 计算 handle / row ID；必要时调用 `AllocHandle()` / `AllocHandleIDs()`
3. 初始化 `encodeRowBuffer`
4. 建立 mem-buffer staging
5. 按列遍历 writable columns，执行：
   - `checkDataForModifyColumn`
   - 列状态判断
   - 可能的 cast / default value 处理
   - `canSkip` 判断
   - `encodeRowBuffer.AddColVal`
6. 进行 row-level constraint check
7. 生成 record key
8. 将整行编码并写入 mem-buffer
9. 设置 assertion
10. 更新统计信息
11. 视表结构情况继续维护其自身索引

这些逻辑集中在：

- `pkg/table/tables/tables.go:713`
- `pkg/table/tables/tables.go:725`
- `pkg/table/tables/tables.go:783`
- `pkg/table/tables/tables.go:792`
- `pkg/table/tables/tables.go:847`
- `pkg/table/tables/tables.go:851`
- `pkg/table/tables/tables.go:887`
- `pkg/table/tables/tables.go:903`
- `pkg/table/tables/tables.go:929`

结论是：**一次 `Table.AddRecord` 的 CPU 开销远不只是“拼一个 KV 并写出去”。**

### 7.2 Mlog repeats this pipeline once more

`mlogTable.AddRecord()` 在基表插入完成后，又执行了一次：

```go
_, err := t.mlog.AddRecord(ctx, txn, mlogRow, opts...)
```

对应：

- `pkg/table/tables/mview_log.go:367`

这意味着 mlog 的附加成本包括：

1. 再次进入 `TableCommon.addRecord`
2. 再次经历 row ID / handle 相关逻辑
3. 再次进行逐列行编码
4. 再次执行 record key 写入
5. 再次设置 assertion / staging / statistics

另外，`writeMLogRow()` 中还专门处理了 `ReservedRowIDAlloc` 的 reset / restore：

- `pkg/table/tables/mview_log.go:358`

这进一步说明 mlog 表写入在实现上就是一条独立的表写路径，而不是复用基表插入时已经准备好的 row ID 状态。

### 7.3 `ReservedRowIDAlloc` is a secondary factor, not the main driver

`ReservedRowIDAlloc` 是 reviewer 容易关注的点，因为 mlog 写入前会执行：

```go
base, maxv := alloc.Current()
defer alloc.Reset(base, maxv)
alloc.Reset(0, 0)
```

看起来像是在每行额外做一次状态切换。但从实现上看，这个对象本身非常轻：

- `ReservedRowIDAlloc` 只有 `base` / `max` 两个字段
- `Current()` 只是读两个 `int64`
- `Reset()` 只是写两个 `int64`
- `Consume()` 只是比较加自增

对应源码：

- `pkg/sessionctx/stmtctx/stmtctx.go:110`
- `pkg/sessionctx/stmtctx/stmtctx.go:117`
- `pkg/sessionctx/stmtctx/stmtctx.go:123`
- `pkg/sessionctx/stmtctx/stmtctx.go:131`

因此，`Current + Reset + defer Reset` 这一小段的**直接 CPU 开销基本可以视为常数级**，不可能单独解释 benchmark 中 `+24.0% vs +16.5%` 的 TiDB CPU 差距。

更值得关注的是它带来的**间接影响**：

1. 基表 `AddRecord` 在带 reserve hint 时，会先通过 `AllocHandleIDs(..., reserveAutoID)` 预留一段 row ID，再写回 `StmtCtx.ReservedRowIDAlloc`
2. `AllocHandle()` 优先从这段预留范围里 `Consume()`
3. mlog 写入前必须把这段预留范围临时清空，避免误把基表保留的 row ID 用到 mlog 表上
4. mlog 当前也没有自己的 `WithReserveAutoIDHint`，因此无法共享这套 statement-level reservation

对应源码：

- `pkg/table/tables/tables.go:763`
- `pkg/table/tables/tables.go:1390`
- `pkg/table/tables/mview_log.go:358`

不过，这个间接影响在本次 benchmark 里仍然不是主导项，原因有两个：

1. 底层 rowid allocator 本身带本地 cache。`Alloc(..., 1)` 先走 allocator 本地状态，只有本地区间耗尽时才会进入 `kv.RunInNewTxn(...)` 扩新段，因此并不是每行都触发一次 metadata txn
2. 本次 benchmark 是 `batch=1` 的 `INSERT` 负载，statement-level reserve 对基表自身也几乎没有摊薄空间。即使 mlog 不能复用 reservation，边际损失也有限

对应源码：

- `pkg/meta/autoid/autoid.go:712`
- `pkg/meta/autoid/autoid.go:878`
- `pkg/executor/insert.go:105`

综合来看，`ReservedRowIDAlloc` 在当前现象中的定位更准确地应当是：

- 为正确性必须存在的状态隔离逻辑
- 可能带来少量次级开销
- 但优先级明显低于第二次 `Table.AddRecord` 整体流水线本身

### 7.4 By contrast, non-unique index only projects and encodes index KV

普通 index 路径的附加工作窄得多：

1. `FetchValues()` 从 `r []Datum` 中直接按 offset 拿出索引列
2. `GenIndexKey()` 生成 index key
3. `GenIndexValue()` 生成 index value
4. `MemBuffer.Set()` 写入 KV

其中 `FetchValues()` 的主体几乎只是：

```go
vals[i] = r[ic.Offset]
```

对应：

- `pkg/table/tables/index.go:744`
- `pkg/table/tables/index.go:748`

也就是说，index 额外工作更接近：

> 对已有 row 做投影，然后编码一个 index entry

而 mlog 更接近：

> 为另一张表重新执行一次通用插入

这两者的 TiDB CPU 成本不在同一层级上。

## 8. Why This Matches the Benchmark Data

如果 `mlog` 更高的 CPU 主要来自 payload 更大、TiKV 侧编码更重，通常应该看到：

- TiKV CPU 明显更高，或者
- 磁盘写入明显更高

但 benchmark 结果恰好不是这样：

1. `mlog` 的 TiKV CPU 与 index 接近，甚至略低
2. `mlog` 的 `Disk KB/row` 低于 index
3. `mlog` 只有 TiDB CPU 更高

这与源码分析高度一致：

- **TiKV 侧**：mlog 写出的字节并不比宽索引更重，甚至更紧凑
- **TiDB 侧**：mlog 走的是第二次通用表插入，因此 CPU 更高

因此，benchmark 中的 `TiDB CPU: +24.0% vs +16.5%`，更合理的解释是：

> mlog 多出来的是 SQL / table layer 的执行路径成本，而不是存储字节数成本。

## 9. Deep Copy Is Real but Not the Primary Cause

### 9.1 What deep copy used to do

在 `writeMLogRow()` 中，旧实现会对每个 tracked datum 调用 `Datum.Copy()`，这会对以下类型执行真实 deep copy：

- string / bytes backing
- `MyDecimal`
- `MysqlTime`

在本 benchmark 的 11 个 tracked 列里，这类对象主要包括：

- 3 个字符串列：`site_code`、`account`、`currency`
- 1 个日期列：`settle_day`
- 3 个 decimal 列：`all_bet`、`valid_bet`、`net_profit`

因此，这部分确实会引入分配和拷贝成本。

### 9.2 Why it still is not the dominant term

但它的代码位置决定了它只是前置小段：

1. 先构造 `mlogRow`
2. 再调用 `t.mlog.AddRecord(...)`

对应：

- `pkg/table/tables/mview_log.go:345`
- `pkg/table/tables/mview_log.go:367`

也就是说，deep copy 只影响 “构造 mlog 行” 这一步，不改变后续那条更重的 `TableCommon.addRecord()` 通用流水线。

因此，去掉 deep copy 可以优化 CPU，但无法把 mlog 拉平到和 non-unique index 同一条成本曲线。

## 10. Optimization Status and Expected Gain

当前分支已经完成一项局部优化：

- 在 `writeMLogRow()` 中去掉 tracked datum 的 deep copy，改为直接复用原 datum 引用

这项优化的预期效果是：

1. 减少 `writeMLogRow()` 阶段的对象分配和内存拷贝
2. 对字符串 / decimal / time 类列收益更明显
3. 对整数类列收益很小，因为原本就是值拷贝

但是从结构上看，这项优化只影响：

```text
base AddRecord
  -> writeMLogRow   <-- 只优化这里
  -> mlog AddRecord <-- 主体成本仍在这里
```

因此更准确的判断应是：

- **可以优化**
- **预计收益有限**
- **无法改变 mlog 比 index 更像“第二次表插入”的事实**

如果后续目标是显著缩小 mlog 与 non-unique index 的 TiDB CPU gap，真正需要优先评估的不是继续抠 `writeMLogRow()` 或 `ReservedRowIDAlloc`，而是是否存在：

1. mlog 专用更窄的写路径
2. 避免再次进入完整 `Table.AddRecord` 的可能性
3. 在 statement 层提前为 mlog 预留 row ID / buffer 的可能性
4. 在 bulk insert / load data 场景下，为 mlog 设计独立 reservation 机制的必要性

这些方向都属于后续设计议题，不是当前分支已有机制。

## 11. Conclusion

结合 benchmark 结果与当前分支源码，实现层面的结论如下：

1. `mlog` TiDB CPU 高于 non-unique index 的主因，不是 mlog 写得更大，也不是 TiKV 更忙，而是 **TiDB 端多走了一次通用表插入路径**。
2. non-unique index 的附加工作是 index-specific fast path；mlog 的附加工作是 second `Table.AddRecord`。这是两者 CPU 模型不同的根本原因。
3. `ReservedRowIDAlloc` 在 mlog 路径中的 reset / restore 逻辑本身开销很小，其影响更多体现在“mlog 不能复用基表 statement-level rowid reservation”，但在本次 `batch=1` benchmark 下仍然只是次级因素。
4. `writeMLogRow()` 的深拷贝只是这条路径中的局部额外成本，值得优化，但不是主导项。
5. 因此，benchmark 中观察到的 TiDB CPU 差异是 **由当前实现结构决定的合理结果**，不是偶发抖动，也不是单纯由 payload 大小解释的现象。

## 12. Source Pointers

为便于评审复核，关键源码入口如下：

- mlog wrapper 入口：`pkg/executor/builder.go:2936`
- mlog wrapper 构造：`pkg/table/tables/mview_log.go:101`
- mlog AddRecord：`pkg/table/tables/mview_log.go:192`
- mlog 写行并再次插表：`pkg/table/tables/mview_log.go:337`
- 通用表插入主路径：`pkg/table/tables/tables.go:697`
- 基表插入后维护索引：`pkg/table/tables/tables.go:954`
- rowid reservation 设置：`pkg/table/tables/tables.go:763`
- rowid reservation 消费：`pkg/table/tables/tables.go:1390`
- index value 投影：`pkg/table/tables/index.go:722`
- index key/value 编码与写入：`pkg/table/tables/index.go:194`
- reserved allocator 定义：`pkg/sessionctx/stmtctx/stmtctx.go:110`
- rowid allocator fast path / refill：`pkg/meta/autoid/autoid.go:712`
- mlog DDL 列构造：`pkg/ddl/executor.go:1047`
