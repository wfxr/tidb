# Mlog Write Overhead Benchmark — Final Report

**Date:** 2026-03-12 ~ 2026-03-13
**Cluster:** 3x TiDB c4-standard-16 + 3x TiKV c4-highmem-16 + 1x PD c4-standard-8 (GCP us-east1-b)
**Concurrency:** 128 threads
**Test rounds:** R2 (single TiDB endpoint) + R3 (3-node round-robin), 9 cases x 660s each

## Executive Summary

两轮测试（R2、R3）系统性地测量了 mlog 对基表 INSERT 的写入开销。R2 发现单节点 sysbench 连接导致 TiDB 热点、TiKV 未充分利用（CPU 64%），overhead 在 3.7%-5.5% 之间。R3 修复负载均衡后，基线吞吐量提升 12.6%，TiKV CPU 升至 72% 接近饱和，overhead 范围扩大至 2.9%-9.1%。

**核心结论：**

| 场景 | Overhead (TPS) | 置信度 |
|------|:--------------:|:------:|
| batch=1, optimistic, unlim | **5.8%-9.1%** | TiKV 饱和度决定上下界 |
| batch=10, optimistic, unlim | **2.9%-3.7%** | 摊薄效果稳定 |
| batch=1, pessimistic, unlim | **4.9%-5.1%** | 两轮一致 |
| 限流（远低于瓶颈） | **≈0%** | 两轮一致 |

Overhead 的直接原因是 **每笔单行事务精确增加 1 次 TiKV prewrite RPC**（21.92 → 22.93 RPCs/event），与 KV 写放大理论完全一致。

## 1. Test Matrix

| # | Scenario | Mlog Shard | Batch | TxnMode | RowRate | Priority |
|---|----------|------------|-------|---------|---------|----------|
| 1 | baseline | - | 1 | optimistic | unlim | P0 |
| 2 | mlog | shard | 1 | optimistic | unlim | P0 |
| 3 | mlog | noshard | 1 | optimistic | unlim | P0 |
| 4 | baseline | - | 10 | optimistic | unlim | P0 |
| 5 | mlog | shard | 10 | optimistic | unlim | P0 |
| 6 | baseline | - | 1 | pessimistic | unlim | P1 |
| 7 | mlog | shard | 1 | pessimistic | unlim | P1 |
| 8 | baseline | - | 1 | optimistic | rate-limited | P2 |
| 9 | mlog | shard | 1 | optimistic | rate-limited | P2 |

## 2. Methodology & Round Differences

| | R2 | R3 |
|---|---|---|
| sysbench 连接 | 单 TiDB 节点 (10.142.0.7) | 3 节点轮询 (10.142.0.{7,6,5}) |
| Baseline TiDB CPU | 30.6% | 41.1% |
| Baseline TiKV CPU | 64.3% | 72.3% |
| Baseline rows/s | 7,095 | 7,987 (+12.6%) |
| P2 限流速率 | 4,000 rows/s | 6,500 rows/s |
| 瓶颈位置 | TiDB 单节点 | TiKV |

**R3 为主要参考数据**：修复了 R2 的负载倾斜问题，集群资源利用更均匀，TiKV 作为真正瓶颈时的 overhead 更能反映生产环境表现。R2 数据用于对比分析 TiKV 饱和度对 overhead 的放大效应。

## 3. Throughput Results

### 3.1 Raw Data (R3, 3-node LB)

| # | Scenario | rows/s | Avg(ms) | P99(ms) | Max(ms) | CV |
|---|----------|-------:|--------:|--------:|--------:|---:|
| 1 | baseline / batch=1 / optimistic | 7,987 | 16.03 | 24.83 | 215.02 | 4.0% |
| 2 | mlog-shard / batch=1 / optimistic | 7,260 | 17.63 | 28.16 | 652.54 | 4.9% |
| 3 | mlog-noshard / batch=1 / optimistic | 7,524 | 17.01 | 27.17 | 512.49 | 5.0% |
| 4 | baseline / batch=10 / optimistic | 11,047 | 115.86 | 179.94 | 520.00 | 2.9% |
| 5 | mlog-shard / batch=10 / optimistic | 10,726 | 119.33 | 176.73 | 327.54 | 2.6% |
| 6 | baseline / batch=1 / pessimistic | 7,599 | 16.84 | 26.20 | 498.09 | 4.8% |
| 7 | mlog-shard / batch=1 / pessimistic | 7,212 | 17.75 | 28.16 | 453.34 | 4.7% |
| 8 | baseline / batch=1 / rate=6500 | 6,501 | 8.14 | 47.47 | 449.31 | 0.4% |
| 9 | mlog-shard / batch=1 / rate=6500 | 6,504 | 10.44 | 63.32 | 571.05 | 0.4% |

> CV (Coefficient of Variation) 基于 10s report-interval TPS 时间序列计算（丢弃前 60s warmup）。所有 case CV < 10%，结果稳定可靠。batch=10 的 rows/s = TPS x 10。

### 3.2 Overhead Analysis (R2 & R3 对比)

| 对比 | 问题 | R2 Baseline | R2 Mlog | R2 OH% | R3 Baseline | R3 Mlog | R3 OH% |
|------|------|------------:|--------:|-------:|------------:|--------:|-------:|
| #1 vs #2 | Q1: batch=1, opt, shard | 7,095 | 6,783 | **-4.4%** | 7,987 | 7,260 | **-9.1%** |
| #1 vs #3 | Q1: batch=1, opt, noshard | 7,095 | 6,703 | **-5.5%** | 7,987 | 7,524 | **-5.8%** |
| #4 vs #5 | Q2: batch=10, opt | 10,982 | 10,574 | **-3.7%** | 11,047 | 10,726 | **-2.9%** |
| #2 vs #3 | Q3: shard vs noshard | 6,783 | 6,703 | **-1.2%** | 7,260 | 7,524 | **+3.6%** |
| #6 vs #7 | Q4: pessimistic | 6,646 | 6,322 | **-4.9%** | 7,599 | 7,212 | **-5.1%** |
| #8 vs #9 | Q5: rate-limited | 4,002 | 3,995 | **-0.2%** | 6,501 | 6,504 | **+0.1%** |

## 4. Cluster Metrics (R3)

### 4.1 Per-Case Metrics

| # | Scenario | TiDB CPU% | TiKV CPU% | Prewrite/s | Commit/s | Disk MB/s | Disk KB/row |
|---|----------|----------:|----------:|-----------:|---------:|----------:|------------:|
| 1 | baseline / batch=1 / opt | 41.1% | 72.3% | 175,185 | 175,180 | 486.6 | 62.39 |
| 2 | mlog-shard / batch=1 / opt | 38.6% | 73.1% | 166,202 | 166,197 | 523.8 | 73.99 |
| 3 | mlog-noshard / batch=1 / opt | 39.8% | 73.6% | 172,517 | 172,513 | 536.1 | 72.97 |
| 4 | baseline / batch=10 / opt | 13.2% | 52.2% | 38,069 | 38,067 | 683.5 | 63.35 |
| 5 | mlog-shard / batch=10 / opt | 13.1% | 54.6% | 37,127 | 37,128 | 723.1 | 69.03 |
| 6 | baseline / batch=1 / pess | 41.1% | 72.9% | 166,626 | 166,621 | 540.4 | 72.82 |
| 7 | mlog-shard / batch=1 / pess | 41.4% | 73.3% | 165,389 | 165,385 | 537.1 | 76.26 |
| 8 | baseline / batch=1 / rate=6500 | 36.6% | 71.1% | 142,558 | 142,556 | 442.5 | 69.70 |
| 9 | mlog-shard / batch=1 / rate=6500 | 37.4% | 73.1% | 149,096 | 149,091 | 466.6 | 73.45 |

> CPU% = process_cpu_seconds_total delta / elapsed / cores (16 cores per node), averaged across nodes.
> Prewrite/Commit 为 3 个 TiKV 节点合计。Disk 为 3 个 TiKV 节点 nvme1n1 合计写入量。

### 4.2 Prewrite RPCs per Event (R2)

| # | Scenario | Total Events | Prewrite RPCs | RPCs/event | Delta vs Baseline |
|---|----------|-------------:|--------------:|-----------:|------------------:|
| 1 | baseline / batch=1 | 4,682,870 | 102,680,866 | **21.93** | - |
| 2 | mlog-shard / batch=1 | 4,477,085 | 102,680,813 | **22.93** | **+1.00** |
| 3 | mlog-noshard / batch=1 | 4,424,254 | 101,431,643 | **22.93** | **+1.00** |
| 4 | baseline / batch=10 | 724,927 | 24,810,770 | **34.23** | - |
| 5 | mlog-shard / batch=10 | 697,980 | 23,726,905 | **33.99** | **-0.24** |
| 6 | baseline / batch=1 / pess | 4,386,289 | 96,153,587 | **21.92** | - |
| 7 | mlog-shard / batch=1 / pess | 4,172,603 | 95,669,872 | **22.93** | **+1.01** |
| 8 | baseline / rate=4000 | 2,641,473 | 57,861,956 | **21.91** | - |
| 9 | mlog-shard / rate=4000 | 2,636,721 | 60,378,461 | **22.90** | **+0.99** |

所有 batch=1 mlog 场景精确增加 **+1 prewrite RPC/event**（21.92 → 22.93）。基表每行 INSERT 产生 22 个 KV 对（1 行数据 + 1 NONCLUSTERED PK + 20 索引），mlog 额外增加 1 个 KV 到独立的 mlog region。batch=10 场景中 10 行 mlog KV 被合并到同一个 prewrite RPC 中，RPCs/event 几乎不变。

### 4.3 Metrics Overhead (R3)

| 对比 | TiDB CPU OH% | TiKV CPU OH% | Prewrite/s OH% | Disk MB/s OH% | Disk KB/row OH% |
|------|----------:|----------:|----------:|----------:|----------:|
| #1 vs #2 (batch=1, opt, shard) | -6.1% | +1.1% | -5.1% | +7.6% | +18.6% |
| #1 vs #3 (batch=1, opt, noshard) | -3.2% | +1.8% | -1.5% | +10.2% | +17.0% |
| #4 vs #5 (batch=10, opt, shard) | -0.8% | +4.6% | -2.5% | +5.8% | +9.0% |
| #6 vs #7 (batch=1, pess, shard) | +0.7% | +0.5% | -0.7% | -0.6% | +4.7% |
| #8 vs #9 (rate=6500, shard) | +2.2% | +2.8% | +4.6% | +5.4% | +5.4% |

> Disk KB/row overhead 反映 mlog 的纯写放大比例。batch=1 场景为 +17-19%，batch=10 降至 +9%，与 mlog 每行 13 列 KV 写入的理论一致。

## 5. Key Findings

### Q1: 单行 INSERT 的 mlog overhead

**结论：batch=1 optimistic 场景下 mlog overhead 为 5.8%-9.1%（TPS 下降），取决于 TiKV 饱和度。**

| 条件 | TiKV CPU | TPS OH% | 机制 |
|------|:--------:|:-------:|------|
| R2 单节点 LB, shard | 64% → 69% | -4.4% | TiKV 未饱和，额外 RPC 被吸收 |
| R3 三节点 LB, noshard | 72% → 74% | -5.8% | 中等饱和，noshard 减少 region 扇出 |
| R3 三节点 LB, shard | 72% → 73% | -9.1% | TiKV 饱和，shard 增加 region 扇出开销 |

根因：每笔事务精确增加 +1 prewrite RPC（21.92 → 22.93 RPCs/event）。在 TiKV 未饱和时，额外 RPC 几乎免费；TiKV 接近饱和时，边际成本急剧上升。R3 中 baseline Prewrite/s 从 155K 提升到 175K（+12.6%），但 mlog-shard 场景仅提升到 166K（+6.8%），差距拉大。

TiDB CPU 在 mlog 场景反而下降（41.1% → 38.6%），说明 `writeMLogRow()` 对 TiDB 侧 CPU 影响极小，瓶颈完全在 TiKV 侧。

### Q2: 批量 INSERT 的摊薄效果

**结论：batch=10 摊薄效果显著，overhead 稳定在 2.9%-3.7%。**

同一事务内 10 行 mlog KV 被合并到同一个 prewrite RPC 中（RPCs/event：34.23 → 33.99，几乎不变），有效摊薄了 RPC 开销。Disk KB/row overhead 从 batch=1 的 +17-19% 降至 +9%。两轮结果一致（R2: 3.7%, R3: 2.9%），batch=10 场景下 TiKV 仅 52-55% 未饱和，overhead 接近理论下界。

### Q3: Shard vs Noshard

**结论：在 TiKV 接近饱和时，noshard 优于 shard；未饱和时差异不显著。**

| 条件 | TiKV CPU | Shard rows/s | Noshard rows/s | 差异 |
|------|:--------:|:------------:|:--------------:|:----:|
| R2 (TiKV 未饱和) | ~68% | 6,783 | 6,703 | shard +1.2% |
| R3 (TiKV 接近饱和) | ~73% | 7,260 | 7,524 | noshard +3.6% |

Shard 将 mlog 写入分散到 8 个 region（跨多个 TiKV 节点），增加了 prewrite batch 的 region 扇出数和 Raft 共识开销。Noshard 集中写入 1 个 region，虽有热点风险，但在当前表结构（22 KV/row）下新增的 1 个 KV 更容易被合并到已有的 prewrite batch 中。在 TiKV 饱和时这一差异被放大。

**建议：** 短期测试中 noshard 更优，但生产环境如果 mlog 数据量积累导致单 region 过大（> 96 MB），shard 仍然是必要的。

### Q4: 悲观事务 overhead

**结论：悲观事务 overhead 为 4.9%-5.1%，两轮结果高度一致。**

Pessimistic-auto-commit 路径为 mlog 行额外获取悲观锁，增加了锁管理开销。但悲观事务下 baseline 吞吐量更低（7,599 vs 7,987 rows/s），mlog 的边际压力相对较小。Prewrite RPCs/event 同样精确增加 +1.01。两轮结果稳定（R2: 4.9%, R3: 5.1%），不受负载均衡方式影响。

### Q5: 限流场景 overhead

**结论：目标吞吐量低于集群瓶颈时，mlog overhead 可忽略（≈0%）。**

| 条件 | 限流速率 | Baseline | Mlog | TPS OH% |
|------|:--------:|:--------:|:----:|:-------:|
| R2 | 4,000 rows/s | 4,002 | 3,995 | -0.2% |
| R3 | 6,500 rows/s | 6,501 | 6,504 | +0.1% |

两个限流速率（~50% 和 ~81% 集群瓶颈）下，mlog 均未影响吞吐量。延迟上升的绝对值较小（R2: +0.55ms avg；R3: +2.3ms avg），且 `--rate` 模式下延迟包含排队时间，意义与无限流不同。对于目标吞吐量低于集群瓶颈的生产场景，mlog 不构成性能问题。

## 6. Overhead Model

综合两轮数据，mlog overhead 可用以下模型理解：

```
TPS_overhead ≈ f(TiKV_saturation) × (1 / batch_size) × RPC_cost
```

- **TiKV 饱和度是主导因素**：TiKV CPU 从 64% 到 73%，batch=1 shard 的 overhead 从 4.4% 翻倍至 9.1%。这是非线性的 — 接近饱和时额外负载的边际成本急剧上升。
- **Batch 摊薄效果**：batch=10 将 10 次 mlog KV 写入合并到 1 个 prewrite RPC 中，overhead 降至 2.9%-3.7%。
- **Region 扇出影响**：shard 增加 prewrite 的 region 数量，在 TiKV 饱和时加剧开销（9.1% vs noshard 5.8%）。
- **限流场景免疫**：当实际吞吐量远低于瓶颈时，额外的 KV 写入在 TiKV 处理能力范围内，overhead ≈ 0%。

## 7. Summary & Recommendations

| 场景 | 预期 OH% | 实测 OH% | 判定 |
|------|:--------:|:--------:|:----:|
| batch=1, opt, unlim (shard) | 5-15% | 4.4%-9.1% | 符合预期，饱和度决定范围 |
| batch=1, opt, unlim (noshard) | 5-15% | 5.5%-5.8% | 符合预期，两轮稳定 |
| batch=10, opt, unlim | 3-8% | 2.9%-3.7% | 优于预期 |
| batch=1, pess, unlim | 略高于 opt | 4.9%-5.1% | 符合预期，两轮稳定 |
| rate-limited (≤80% 瓶颈) | <2% | ≈0% | 优于预期 |

**建议：**

1. **生产环境（限流/非极端写入压力）：** Mlog overhead 可忽略（≈0%），可放心启用。
2. **高写入压力场景：** 预期 5-9% TPS 下降（取决于 TiKV 饱和度）。如 TiKV CPU 已接近阈值（>70%），建议先扩容 TiKV 或降低写入压力。
3. **批量 INSERT：** 推荐使用 batch 写入（batch≥10），可将 overhead 控制在 3% 以内。
4. **Shard 模式选择：** 小数据量或短生命周期场景优先 noshard（-3.6% 开销差异）；大数据量或长期运行场景使用 shard 避免热点。

## 8. Validation

两轮所有 18 个 case（R2: 9, R3: 9）均通过以下校验：
- sysbench errors = 0, reconnects = 0
- `SELECT count(*) FROM bc_bet_records` = sysbench total_events x batch_size
- （mlog 场景）mlog 行数 = 基表行数

## 9. Environment

- TiDB/TiKV/TiFlash: patched binary (feature branch with mlog support)
- sysbench: 1.0.20 (LuaJIT 2.1.0-beta3)
- TiDB config: `pessimistic-txn.pessimistic-auto-commit = true`
- Base table: 41 columns, 20 secondary indexes, NONCLUSTERED PK, SHARD_ROW_ID_BITS=4 PRE_SPLIT_REGIONS=3
- Mlog table: 13 columns, no indexes (shard mode: SHARD_ROW_ID_BITS=4 PRE_SPLIT_REGIONS=2)
- Metrics collection: process CPU (TiDB/TiKV), gRPC prewrite/commit counters, node_exporter disk written bytes
- **R2 results:** `results/20260312T140018Z/` (single TiDB endpoint)
- **R3 results:** `results/20260313T032423Z/` + `results/20260313T054428Z/` (3-node round-robin)
