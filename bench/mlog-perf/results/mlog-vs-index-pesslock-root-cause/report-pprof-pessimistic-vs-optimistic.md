# CPU Profile 分析：悲观 vs 乐观事务下 Mlog 的 TiDB CPU 开销

**Date**: 2026-03-18 | **Results**: `results/20260318T123425Z` | **Profile**: 120s × 3 nodes (merged)

## TL;DR

通过 6 组对照实验（悲观/乐观 × baseline/index/mlog）的 CPU profile 对比，定位了悲观模式下 mlog TiDB CPU 高于 index 的具体来源：
- **`SendReqCtx`（gRPC 发送层）是核心差异点**：悲观模式下 mlog +51.4% vs index +26.5%（差 25pp）；乐观模式下 mlog +39.9% ≈ index +36.3%（差仅 4pp）
- 这 25pp 的 `SendReqCtx` 差距里，约 41s 的绝对增量在乐观对照中消失，与 mlog 额外的 `kv_pessimistic_lock` gRPC 调用最一致，是最强的归因信号
- `pessimisticLockMutations` 的 cum 时间反而更低（-30.9%），主要是因为额外 lock batches 被分流到异步 worker goroutine；不能据此推出锁逻辑本身更省 CPU

## Test Setup

6 个 case，悲观和乐观各一组：

| Case | 场景 | 事务模式 |
|------|------|----------|
| 1 | baseline | pessimistic |
| 2 | index | pessimistic |
| 3 | mlog-noshard | pessimistic |
| 4 | baseline | optimistic |
| 5 | index | optimistic |
| 6 | mlog-noshard | optimistic |

- **基表**: `bc_bet_records`，42 列，NONCLUSTERED PK，无 SHARD；base record 数据为单 region，但写路径仍会触达 PK index region
- **限流**: 18,000 rows/s，128 threads，3 TiDB nodes round-robin
- **Profile**: 每 case 的 warmup 60s + 稳定 30s 后，采集 120s CPU profile，3 个 TiDB 节点并行采集后合并

## Results

### 1. Cluster Metrics

#### 悲观事务

| | Baseline | Index | vs BL | Mlog | vs BL |
|--|----------|-------|-------|------|-------|
| TiDB CPU | 36.0% | 41.5% | +15.3% | 43.6% | +21.1% |
| TiKV CPU | 24.9% | 37.8% | +51.8% | 34.5% | +38.6% |
| PessLock/s | 34,913 | 35,022 | +0.3% | 52,834 | **+51.3%** |
| Prewrite/s | 34,917 | 49,853 | +42.8% | 52,840 | +51.3% |
| Commit/s | 33,863 | 48,914 | +44.4% | 52,837 | +56.0% |
| Disk KB/row | 9.47 | 17.74 | +87.3% | 13.42 | +41.8% |

#### 乐观事务

| | Baseline | Index | vs BL | Mlog | vs BL |
|--|----------|-------|-------|------|-------|
| TiDB CPU | 30.3% | 35.7% | +17.8% | 35.8% | +18.2% |
| TiKV CPU | 19.3% | 31.3% | +62.2% | 27.0% | +39.9% |
| PessLock/s | 16 | 1 | — | 0 | — |
| Prewrite/s | 34,864 | 50,116 | +43.7% | 52,899 | +51.7% |
| Commit/s | 33,766 | 49,416 | +46.3% | 52,896 | +56.7% |
| Disk KB/row | 9.71 | 16.94 | +74.5% | 13.28 | +36.8% |

#### TiDB CPU 差距变化

| | 悲观 | 乐观 |
|--|------|------|
| Mlog TiDB CPU OH% | +21.1% | +18.2% |
| Index TiDB CPU OH% | +15.3% | +17.8% |
| **Mlog - Index 差距** | **+5.8pp** | **+0.4pp** |

切换到乐观事务后，mlog 与 index 的 TiDB CPU 差距从 5.8pp 缩小到 0.4pp。

### 2. CPU Profile 对比（3 节点合并；函数为 cum time）

| 函数 | BL-pess | Idx-pess | Mlog-pess | BL-opt | Idx-opt | Mlog-opt |
|------|---------|----------|-----------|--------|---------|----------|
| **Profile duration** | **355s** | **354s** | **355s** | **356s** | **356s** | **355s** |
| **Total samples** | **1731s** | **2038s (+17.8%)** | **2076s (+19.9%)** | **1494s** | **1743s (+16.7%)** | **1696s (+13.5%)** |
| `handlePessimisticDML` | 255s | 315s (+23.5%) | 260s (+2.0%) | — | — | — |
| `pessimisticLockMutations` | 84s | 87s (+3.6%) | 58s (-30.9%) | — | — | — |
| `SendReqCtx` | 185s | 234s (+26.5%) | 280s (+51.4%) | 135s | 184s (+36.3%) | 189s (+39.9%) |
| `Prewrite.handleSingle` | 81s | 107s (+32.1%) | 111s (+37.0%) | 89s | 122s (+37.1%) | 124s (+39.3%) |
| `Commit.handleSingle` | 74s | 103s (+39.2%) | 104s (+40.5%) | 73s | 97s (+32.9%) | 100s (+37.0%) |
| `mallocgc` | 288s | 337s (+17.0%) | 360s (+25.0%) | 250s | 290s (+16.0%) | 285s (+14.0%) |
| `yyParse` | 140s | 148s (+5.7%) | 147s (+5.0%) | 133s | 135s (+1.5%) | 138s (+3.8%) |
| `insertRows` | 139s | 195s (+40.3%) | 167s (+20.1%) | 135s | 183s (+35.6%) | 160s (+18.5%) |
| `buildInsert` | 175s | 188s (+7.4%) | 186s (+6.3%) | 165s | 173s (+4.8%) | 174s (+5.5%) |

*百分比为相对同组 baseline 的变化。`Profile duration` 是采样墙钟时长；真正反映 CPU 总量的是 `Total samples`。*

### 3. 分析

#### 3.1 `SendReqCtx` — 核心差异点

`SendReqCtx` 是所有 TiKV gRPC 调用（pessimistic lock / prewrite / commit）的统一发送入口。

悲观模式下：
- Index: 234s (+26.5%) — 额外的 prewrite + commit RPCs
- Mlog: 280s (+51.4%) — 额外的 pessimistic lock + prewrite + commit RPCs
- **差值: 46s** — 其中约 41s 在乐观对照中消失，与 mlog 额外的 ~18K/s pessimistic lock RPCs 最一致；剩余约 5s 更像 prewrite/commit 共享路径差异或噪声

乐观模式下：
- Index: 184s (+36.3%) — 额外的 prewrite + commit RPCs
- Mlog: 189s (+39.9%) — 额外的 prewrite + commit RPCs
- **差值: 5s** — 无 pessimistic lock，差距几乎消失

**结论**：悲观模式下 mlog 比 index 多出的 SendReqCtx 开销中，大头可归到额外的 `kv_pessimistic_lock` gRPC 调用。严格说 `SendReqCtx` 仍覆盖 prewrite/commit 等共享路径，因此这里更准确的表述是“主要来自”，而不是“完全来自”或某个可精确切分的固定占比。

#### 3.2 `pessimisticLockMutations` — 为何 mlog 反而更低

| | BL-pess | Idx-pess | Mlog-pess |
|--|---------|----------|-----------|
| `pessimisticLockMutations` | 84s | 87s | **58s** |
| PessLock/s | 34,913 | 35,022 | **52,834** |

Mlog 发送了 51% 更多的 pessimistic lock RPC，但 `pessimisticLockMutations` 的 cum 时间反而低 31%。这不能说明 mlog 的悲观锁更轻量；更直接的原因是 client-go 的 **batch 分发机制**改变了 CPU 样本的归属。CPU pprof 记录的是实际消耗到 CPU 上的样本，不应把这里解释成 “I/O 等待更多”。

**client-go 悲观锁分发流程**（`2pc.go`）：

```
doActionOnGroupMutations():
  L998-1001: 先同步发送 primary batch
             → doActionOnBatches(primaryBatch()) → 1 batch → noNeedFork=true
             → 同步调用 handleSingleBatch
  L1009:     forgetPrimary() 移除 primary
  L1054:     再发送剩余 batches
             → doActionOnBatches(allBatches())
             → 如果剩 1 batch → noNeedFork=true → 同步
             → 如果剩 >1 batch → batchExecutor.process() → go startWorker() → 异步
```

**各 case 的执行路径差异**：

这里的 `Regions` 指 pessimistic lock batch 实际触达的 region 数，而不是仅指 base table record 所在的 region 数。

| Case | Lock keys | Regions | Primary (同步) | 剩余 batches | 剩余路径 | `pessimisticLockMutations` cum 包含 |
|------|-----------|---------|---------------|-------------|---------|-----------------------------------|
| Baseline | 2 | 2 | 1 batch 同步 | 1 batch | `noNeedFork=true` → **同步** | **2 个** handleSingleBatch |
| Index | 2 | 2 | 1 batch 同步 | 1 batch | `noNeedFork=true` → **同步** | **2 个** handleSingleBatch |
| Mlog | 3 | 3 | 1 batch 同步 | 2 batches | `noNeedFork=false` → **异步 goroutine** | **1 个** handleSingleBatch |

Baseline/Index 的 base record 本身是单 region，但 2 个 lock key 会分别落到 record region 和 PK index region。forgetPrimary 后剩 1 个 batch，走 `noNeedFork=true` 同步路径（`2pc.go:1092`）。两个 `handleSingleBatch`（含 `SendReqCtx`）的 CPU 时间都归到 `pessimisticLockMutations` 的 cum 里。

Mlog 的 3 个 lock key 会触达 3 个 region（base record + PK index + mlog record）。forgetPrimary 后剩 2 个 batch，走异步路径（`2pc.go:1114` → `batchExecutor.process` → `go startWorker`）。只有 primary 的 1 个 `handleSingleBatch` 算在 `pessimisticLockMutations` 的 cum 里；另外 2 个在 `startWorker.func1` 的独立 goroutine 栈上。

从 pprof 也能看到这一点：`startWorker.func1` 在 mlog 中悲观/乐观分别是 332s / 248s，差了 **84s**；而 baseline 则是 171s / 178s，基本不变。说明 mlog 在悲观模式下新增的大量工作，确实主要落在异步 worker 栈上。

**结论**：`pessimisticLockMutations` cum 值低不代表悲观锁 CPU 更少，而是 2/3 的 RPC 工作被异步 goroutine 分流，不在此函数的调用栈上。悲观锁相关的真实 CPU 成本需要结合 `pessimisticLockMutations`、`actionPessimisticLock.handleSingleBatch` 和 `startWorker.func1` 一起看；这些信号与 `SendReqCtx` 的 +51.4% 增量是一致的。

#### 3.3 `mallocgc` — 内存分配开销

悲观模式下 mlog `mallocgc` +25.0% vs index +17.0%（差 8pp）。乐观模式下 mlog +14.0%，回落到与 index（+16.0%）同量级。

从悲观/乐观对照看，mlog 在悲观模式下的额外内存分配最可能主要来自 pessimistic lock 请求的构造和序列化。乐观模式下这部分基本消失，因此 `mallocgc` 也回落到与 index 同量级。

#### 3.4 与事务模式无关的稳定开销

以下函数在悲观/乐观模式下表现一致，说明它们与事务模式无关：

| 函数 | 悲观 Mlog vs BL | 乐观 Mlog vs BL | 说明 |
|------|----------------|----------------|------|
| `Prewrite.handleSingle` | +37.0% | +39.3% | prewrite 开销稳定 |
| `Commit.handleSingle` | +40.5% | +37.0% | commit 开销稳定 |
| `buildInsert` | +6.3% | +5.5% | mlog 行构造的 planner 开销 |
| `insertRows` | +20.1% | +18.5% | mlog 行的执行层开销 |
| `yyParse` | +5.0% | +3.8% | 与 mlog 无关的噪声 |

### 4. 主要热点增量（cum time，非可加总分解）

悲观模式下 mlog vs baseline 的主要热点增量如下。注意 cum time 存在调用栈重叠，**不能**按行相加理解为严格分摊：

| 来源 | 增量 (cum) | 说明 |
|------|-----------|------|
| `SendReqCtx` | +95s | 最大热点，覆盖 pesslock + prewrite + commit 的 gRPC 发送 |
| `mallocgc` | +72s | 请求对象分配和序列化相关开销 |
| `Prewrite.handleSingle` | +30s | 更多 KV 对的 prewrite |
| `Commit.handleSingle` | +30s | 更多 KV 对的 commit |
| `insertRows` | +28s | mlog 行的构造和编码 |
| `buildInsert` | +11s | planner 中 mlog 行的构建 |

其中，切换到乐观模式后明显回落、可较强关联到悲观锁路径的热点主要是：
- `SendReqCtx`: 95s → 54s，消失 **41s**（约 43%）
- `mallocgc`: 72s → 35s，消失 **37s**（约 51%）

## Notes

- Case 1 (pessimistic baseline) 和 Case 6 (optimistic mlog) 的延迟异常偏高（avg 10.38ms / 11.85ms），可能受建表后初始 compaction 等背景因素影响；这是工作假设，不影响本文对 CPU 和 RPC 指标的归因主线。
- pprof 的 `Profile duration` 在各 case 基本相同（~355s），因为采样墙钟时长一致；真正反映 CPU 总量的 `Total samples` 并不相同，范围约 1494s–2076s。

## Conclusion

1. **悲观锁相关的 CPU 热点主要体现在 gRPC 发送层和请求构造（`SendReqCtx` / `mallocgc`），不能只看 `pessimisticLockMutations` 这个 caller 的 cum 值**。后者会受到 async batch 路径影响，样本会转移到 `startWorker.func1`。
2. **悲观模式下 mlog 比 index 多出的 TiDB CPU 开销，主导因素是额外的 `kv_pessimistic_lock` gRPC 调用**。切换到乐观模式后，mlog 与 index 的 TiDB CPU OH 差距从 5.8pp 缩小到 0.4pp；结合 RPC 指标和 pprof 热点变化，这个归因是成立的，但不宜表述为过于精确的固定占比。
3. **工程含义**：如果后续能够避免 mlog 进入 pessimistic lock 路径，按本文对照结果，这部分额外 TiDB CPU 应会明显回落；但具体实现方案和精确收益不在本文讨论范围内。
4. **Prewrite/Commit 开销与事务模式无关**，因此即使悲观锁路径被消掉，这部分仍然是 mlog 写入的固有成本。
