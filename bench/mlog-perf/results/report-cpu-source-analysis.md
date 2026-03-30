# 源码级分析：Index vs Mlog 的 TiDB CPU 开销差异

**Date**: 2026-03-18 | **Benchmark**: `results/20260318T123425Z` | **Profile**: 120s × 3 nodes

## TL;DR

结合 6 组 CPU profile（悲观/乐观 × baseline/index/mlog）和源码分析，定位了 mlog 在悲观模式下 TiDB CPU 高于 index 的**根因**：

1. **Mlog 行被悲观锁的原因是缺少 `SetNeedConstraintCheckInPrewrite` flag**。mlog 的 `AddRecord` 使用 `DupKeyCheckSkip`（跳过重复检查），导致 `setPresume=false`，record key 写入 memBuffer 时不带任何 flag。在 `KeyNeedToLock()` 中，无 flag 的 record key 直接走 `!IsIndexKey(k)` 分支返回 true，被加锁。
2. **非唯一索引跳过悲观锁**是因为在 `KeyNeedToLock()` 中走 `!IndexKVIsUnique(v)` 分支，返回 false。
3. **修复方案**：在 mlog 的 `AddRecord` 调用时设置 `SetNeedConstraintCheckInPrewrite` flag，使其在 `KeyNeedToLock()` 第 619 行被跳过。

## 1. 写入路径对比

### Index 写入路径（每行 INSERT）

```
InsertExec.Next() → insertRows() → addRecord()
  → Table.AddRecord()  [tables.go:697]
    → addRecord()  [tables.go:703]
      ├─ 生成 recordID（_tidb_rowid）
      ├─ 编码行数据 → WriteMemBufferEncoded(key, row, flags)  [line 887]
      │   flags = [SetPresumeKeyNotExists]  （auto-commit pessimistic 下）
      │   → 写入 memBuffer，key = "t{tableID}_r{rowID}"
      │
      └─ addIndices()  [line 913]
          └─ 对每个 writable index:
              index.create()  [index.go:194]
              ├─ GenIndexKey(11列 + handle)  → key = "t{tableID}_i{idxID}{col_values}{handle}"
              ├─ GenIndexValue()  → compact value
              └─ memBuffer.Set(key, val)  [line 299]  ← 无 flag
```

**产生的 KV mutations**：
- 1 个 record key（基表行）— 带 `SetPresumeKeyNotExists` flag
- 1 个 unique index key（NONCLUSTERED PK）— 带 `SetAssertNotExist`
- 1 个 non-unique index key（11 列索引）— **无 flag**

### Mlog 写入路径（每行 INSERT）

```
InsertExec.Next() → insertRows() → addRecord()
  → mlogTable.AddRecord()  [mview_log.go:192]
    ├─ t.Table.AddRecord()  — 与 baseline 相同（基表行 + PK 索引）
    │
    └─ writeMLogRow()  [mview_log.go:337]
        ├─ 复制 11 个 tracked 列的 Datum  [line 354-356]
        ├─ 追加 DML type + old/new marker  [line 358]
        └─ t.mlog.AddRecord(ctx, txn, mlogRow, DupKeyCheckSkip, ...)  [line 369]
            → TableCommon.addRecord()  [tables.go:703]
              ├─ opt.DupKeyCheck() == DupKeyCheckSkip → 跳过整个 dup check 块
              │   → setPresume = false
              │   → flags = nil  ← 关键：无任何 flag
              ├─ WriteMemBufferEncoded(key, row, 无flag)  [line 887]
              │   key = "t{mlogTableID}_r{rowID}"
              └─ addIndices()  — mlog 表无索引，跳过
```

**产生的 KV mutations**：
- 1 个 record key（基表行）— 带 `SetPresumeKeyNotExists` flag
- 1 个 unique index key（NONCLUSTERED PK）— 带 `SetAssertNotExist`
- 1 个 record key（mlog 行）— **无 flag** ← 这就是问题所在

### 关键区别

| 额外 mutation | 类型 | Flag | `KeyNeedToLock()` 判定 |
|---------------|------|------|----------------------|
| **Index key**（非唯一索引） | index key (`_i`) | 无 | line 650: `!IndexKVIsUnique(v)` → false → **不锁** |
| **Mlog key**（mlog 行） | record key (`_r`) | 无 | line 636: `!IsIndexKey(k)` → true → **锁** |

## 2. `KeyNeedToLock()` 决策路径详解

源码位于 `pkg/session/txn.go:610-663`。以下是各 mutation 的判定路径：

```
KeyNeedToLock(k, v, flags):

  [L619] flags.HasNeedConstraintCheckInPrewrite?  ──── 基表 record key (auto-commit pessimistic,
         → true → return false (不锁)                   ConstraintCheckInPlacePessimistic=true, InTxn=false)
                                                        实际走 L623: HasPresumeKeyNotExists → true → 锁
                                                        (因为 auto-commit 不算 InTxn，不设 ConstraintCheckInPrewrite)

  [L623] flags.HasPresumeKeyNotExists?
         → true → return true (锁)  ──────────── 基表 record key (DupKeyCheckLazy, setPresume=true)

  [L628] len(v) == 0?  (删除操作)
         → 不相关

  [L636] !tablecodec.IsIndexKey(k)?
         → true → return true (锁)  ──────────── mlog record key (无 flag, 是 record key)

  [L650] !tablecodec.IndexKVIsUnique(v)?
         → true → return flags.HasNeedLocked()
                   → false → return false (不锁) ─── 非唯一索引 key (无 NeedLocked flag)

  [L661] 唯一索引 → return true (锁) ─────────── PK 唯一索引 key
```

### 每行的悲观锁次数推导

| Mutation | Baseline | Index | Mlog |
|----------|----------|-------|------|
| 基表 `_tidb_rowid` record key | **锁**（L623） | **锁**（L623） | **锁**（L623） |
| PK 唯一索引 key | **锁**（L661） | **锁**（L661） | **锁**（L661） |
| 非唯一索引 key | — | **不锁**（L650） | — |
| mlog record key | — | — | **锁**（L636） |
| **合计 locks/row** | **2** | **2** | **3** |

与实测数据吻合：Baseline 1.94、Index 1.95、Mlog 2.94 locks/row。

## 3. CPU 开销差异的精确归因

### 3.1 每行额外操作对比

| 操作 | Index | Mlog | CPU 差异来源 |
|------|-------|------|------------|
| KV key 编码 | `GenIndexKey()`: 11 列 codec 编码 → index key | `EncodeRow()`: 11+2 列行编码 → record key | 不同编码格式 |
| KV value 编码 | `GenIndexValue()`: 紧凑 value | 行编码已包含在 key 编码中 | |
| Datum 复制 | `FetchValues()`: 从行提取 11 列 | `Copy()` × 11 次 [mview_log.go:355] | 类似 |
| MemBuffer 写入 | `Set(key, val)` — 无 flag | `WriteMemBufferEncoded(key, row)` — 无 flag | |
| 悲观锁 RPC | 无（非唯一索引跳过） | **1 次额外 RPC** | **核心差异** |
| RowID 分配 | 无（index 无 rowID） | `AllocHandle()` + alloc reset [mview_log.go:360-367] | 小开销 |

### 3.2 pprof 数据归因

从 3 节点 120s 合并 profile（累积时间，相对同组 baseline 的增量）：

| 函数 | Idx-pess 增量 | Mlog-pess 增量 | Idx-opt 增量 | Mlog-opt 增量 | 分析 |
|------|-------------|--------------|------------|-------------|------|
| `SendReqCtx` | +49s | **+95s** | +49s | +54s | 悲观下 mlog 多 46s = pesslock RPC |
| `mallocgc` | +49s | **+72s** | +40s | +35s | 悲观下 mlog 多 23s = pesslock 请求对象分配 |
| `insertRows` | **+56s** | +28s | **+48s** | +25s | index 更高：index key 编码更贵 |
| `Prewrite.handleSingle` | +26s | +30s | +33s | +35s | 相近，与事务模式无关 |
| `Commit.handleSingle` | +29s | +30s | +24s | +27s | 相近，与事务模式无关 |
| `buildInsert` | +13s | +11s | +8s | +9s | 相近，planner 层开销 |

**悲观模式下 mlog 独有的开销（切换到乐观后消失）**：
- `SendReqCtx` 多 41s（95s - 54s），占总增量的 ~43%
- `mallocgc` 多 37s（72s - 35s），占总增量的 ~39%
- 合计 **~78s**，完全来自 `kv_pessimistic_lock` gRPC 发送和请求对象构造

**Index 独有的开销（mlog 没有的）**：
- `insertRows` 比 mlog 多 ~28s（悲观）/ ~23s（乐观）
- 原因：11 列 index key 的 codec 编码（`GenIndexKey` → `codec.EncodeKey`）比 mlog 的标准行编码更贵
- Index key 需要逐列 datum 编码到 key 中（带类型前缀），而 mlog row 使用紧凑的行编码格式

### 3.3 `pessimisticLockMutations` cum 为何 mlog 反而更低

pprof 数据显示 mlog 发送了 51% 更多的悲观锁 RPC，但 `pessimisticLockMutations` 的 cum 时间反而低 31%（58s vs 84s）。这不是因为 mlog 的悲观锁更轻量，而是 client-go 的 batch 分发机制和 Go pprof 的 goroutine 归属规则共同导致的测量差异。

**client-go 悲观锁分发流程**（`tikv/client-go` `2pc.go`）：

1. `doActionOnGroupMutations` L998-1001：**先同步**发送 primary batch
   - `doActionOnBatches(primaryBatch())` → 只有 1 batch → `noNeedFork=true` → 同步调用 `handleSingleBatch`
2. L1009：`forgetPrimary()` 移除 primary
3. L1054：发送剩余 batches
   - `doActionOnBatches(allBatches())` → 如果剩 1 batch → `noNeedFork=true` → **同步**
   - 如果剩 >1 batch → `batchExecutor.process()` → `go startWorker()` → **异步 goroutine**

**各 case 的路径差异**：

| Case | Lock keys | Regions | Primary (同步) | 剩余 | 剩余路径 | cum 包含 |
|------|-----------|---------|---------------|------|---------|---------|
| Baseline | 2 | 2 | 1 batch 同步 | 1 batch | `noNeedFork` → **同步** | **2 个** handleSingleBatch |
| Index | 2 | 2 | 1 batch 同步 | 1 batch | `noNeedFork` → **同步** | **2 个** handleSingleBatch |
| Mlog | 3 | 3 | 1 batch 同步 | 2 batches | **异步 goroutine** | **1 个** handleSingleBatch |

Baseline/Index：2 个 lock key 在 2 个 region，forgetPrimary 后剩 1 batch，走同步路径。两个 `handleSingleBatch`（含 `SendReqCtx`）的 CPU 都归到 `pessimisticLockMutations` 的 cum 里。

Mlog：3 个 lock key 在 3 个 region（base record + PK index + mlog record），forgetPrimary 后剩 2 batches，走异步路径。只有 primary 的 1 个 `handleSingleBatch` 算在 `pessimisticLockMutations` 的 cum 里；另外 2 个在 `startWorker.func1` 的独立 goroutine 栈上。

**结论**：`pessimisticLockMutations` cum 值低不代表悲观锁 CPU 更少，而是 2/3 的 RPC 工作被异步 goroutine 分流，不在此函数的调用栈上。悲观锁的真实 CPU 总成本在 mlog 中是更高的，与 `SendReqCtx` 的 +51.4% 增量一致。

## 4. 修复方案

### 问题根因

`mview_log.go:369` 调用 `t.mlog.AddRecord()` 时传了 `DupKeyCheckSkip`，导致 `tables.go:853` 的 dup check 块被完全跳过，`setPresume=false`，record key 写入 memBuffer 时不带任何 flag。

在 `KeyNeedToLock()` 中，无 flag 的 record key 走到 L636 `!IsIndexKey(k)` 返回 true，被加悲观锁。

### 修复方案

在 `mview_log.go:369` 的 `mlog.AddRecord` 调用之后，为 mlog record key 设置 `SetNeedConstraintCheckInPrewrite` flag：

```go
// mview_log.go writeMLogRow() 中
_, err := t.mlog.AddRecord(ctx, txn, mlogRow, opts...)
if err != nil {
    return err
}
// Mlog rows use auto-generated rowIDs with no conflict risk.
// Skip pessimistic locking by marking the key for prewrite-phase check.
// In KeyNeedToLock(), HasNeedConstraintCheckInPrewrite() returns true → skip lock.
mlogKey := t.mlog.RecordKey(recordID)  // 需要拿到 recordID
txn.GetMemBuffer().UpdateFlags(mlogKey, kv.SetNeedConstraintCheckInPrewrite)
```

或者更简洁地，在传给 `AddRecord` 的 opts 中设置适当的 `PessimisticLazyDupKeyCheck` 模式，使得 dup check 路径设置 `SetNeedConstraintCheckInPrewrite` flag。

### 预期效果

- `KeyNeedToLock()` L619: `HasNeedConstraintCheckInPrewrite()` → true → return false → **不锁**
- PessLock/s OH%：+51% → ~0%（与 index 持平）
- TiDB CPU OH%：~+21% → ~+16%（与 index 持平）
- 每行减少 1 次 TiKV gRPC 往返

### 正确性

Mlog record key 使用内部自动生成的 `_tidb_rowid`（auto-increment），不可能与其他事务冲突：
1. 每个 mlog 行的 rowID 由 TiDB 的 RowID allocator 分配，全局唯一
2. Mlog 表没有唯一索引，无约束冲突
3. Prewrite 阶段的 conflict check 仍然会执行（作为兜底），但不会失败

## 5. 其他 CPU 开销观察

### Index 的 `insertRows` 为何更高

Index 的 `insertRows` 增量（+56s）显著高于 mlog（+28s），两者的差异在于：

- **Index key 编码**：`GenIndexKey()` 需要用 `codec.EncodeKey()` 逐列编码 11 个 datum 到 key 字节中，每列需要类型前缀 + 值编码
- **Mlog row 编码**：走标准 `EncodeRow()` 路径，使用紧凑的列式编码格式，11 列 + 2 系统列

这解释了为什么即使在乐观模式下（无 pesslock 干扰），mlog 的 `insertRows` 开销也低于 index。

### `mallocgc` 开销分布

悲观模式下 mlog 的 `mallocgc` 比 index 高 23s（360s vs 337s），但乐观模式下 mlog 反而低 5s（285s vs 290s）。差异来自：
- 悲观模式：mlog 额外的 pessimistic lock 请求需要构造 `PessimisticLockRequest` 对象 + protobuf 序列化，产生额外内存分配
- 乐观模式：mlog 无额外锁请求，且行编码比 index key 编码的内存分配更少
