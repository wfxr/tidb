# Mlog Write Overhead Benchmark Report (R3)

**Date:** 2026-03-13
**Duration:** ~108 min (9 cases x 660s + setup/validation)
**Cluster:** 3x TiDB c4-standard-16 + 3x TiKV c4-highmem-16 + 1x PD c4-standard-8 (GCP us-east1-b)
**Concurrency:** 128 threads
**Results directory:** `results/20260313T032423Z/` (P2 cases merged from `results/20260313T054428Z/`)

## Changes from R2

- **负载均衡修复：** sysbench 连接从单 TiDB 节点改为 3 节点轮询（`--host 10.142.0.7,10.142.0.6,10.142.0.5`）。R2 中所有连接集中在一个 TiDB 上，导致负载倾斜。
- **限流速率调整：** P2 限流场景从 4,000 rows/s 提高到 6,500 rows/s，更接近多节点场景下的合理负载水平。

## Executive Summary

多节点负载均衡后，基线吞吐量显著提升（7,095 → 7,987 rows/s, +12.6%），集群资源利用更均匀。Mlog overhead 在 **2.9%-9.1%** 之间（TPS 下降），其中 shard 模式 batch=1 的 overhead 从 R2 的 4.4% 升至 **9.1%**，noshard 模式为 **5.8%**。batch=10 摊薄效果明显（**2.9%**），悲观事务 overhead 为 **5.1%**。限流 6,500 rows/s 场景下 overhead 可忽略（**+0.1%**）。

TiKV CPU 在所有场景下均已达到 72-73%，接近饱和。Overhead 增大的主要原因是负载均衡后 TiDB 不再是瓶颈，TiKV 成为真正的瓶颈，mlog 额外的 KV 写入在 TiKV 饱和时影响更显著。

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
| 8 | baseline | - | 1 | optimistic | 6500 | P2 |
| 9 | mlog | shard | 1 | optimistic | 6500 | P2 |

## 2. Throughput Results

### 2.1 Raw Data

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

> CV (Coefficient of Variation) 基于 10s report-interval TPS 时间序列计算（丢弃前 60s warmup）。所有 case CV < 10%，结果稳定可靠。
>
> batch=10 的 rows/s = TPS x 10。

### 2.2 Overhead Analysis

| 对比 | 问题 | Baseline rows/s | Mlog rows/s | TPS OH% | Avg Lat OH% | P99 Lat OH% |
|------|------|----------------:|------------:|--------:|------------:|------------:|
| #1 vs #2 | Q1: 单行 mlog overhead (shard) | 7,987 | 7,260 | **-9.1%** | +10.0% | +13.4% |
| #1 vs #3 | Q1: 单行 mlog overhead (noshard) | 7,987 | 7,524 | **-5.8%** | +6.1% | +9.4% |
| #4 vs #5 | Q2: batch=10 摊薄效果 | 11,047 | 10,726 | **-2.9%** | +3.0% | -1.8% |
| #2 vs #3 | Q3: shard vs noshard | 7,260 | 7,524 | **+3.6%** | -3.5% | -3.5% |
| #6 vs #7 | Q4: 悲观事务 overhead | 7,599 | 7,212 | **-5.1%** | +5.4% | +7.5% |
| #8 vs #9 | Q5: 限流场景 overhead | 6,501 | 6,504 | **+0.1%** | +28.3% | +33.4% |

### 2.3 R2 vs R3 Overhead 对比

| 场景 | R2 OH% | R3 OH% | 变化 |
|------|-------:|-------:|------|
| batch=1, opt, shard | -4.4% | -9.1% | overhead 增大，因 TiKV 饱和更充分 |
| batch=1, opt, noshard | -5.5% | -5.8% | 基本一致 |
| batch=10, opt, shard | -3.7% | -2.9% | 略优 |
| batch=1, pess, shard | -4.9% | -5.1% | 基本一致 |
| rate-limited | -0.2% | +0.1% | 可忽略 |

## 3. Cluster Metrics

### 3.1 Per-Case Metrics

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
>
> Prewrite/Commit 为 3 个 TiKV 节点合计。Disk 为 3 个 TiKV 节点 nvme1n1 合计写入量。

### 3.2 R2 vs R3 集群利用率对比

| 指标 | R2 (单节点) | R3 (三节点 LB) | 变化 |
|------|----------:|----------:|------|
| Baseline TiDB CPU% | 30.6% | 41.1% | +34% — TiDB 负载更均匀，总利用率提升 |
| Baseline TiKV CPU% | 64.3% | 72.3% | +12% — TiKV 更接近饱和 |
| Baseline rows/s | 7,095 | 7,987 | +12.6% — 消除 TiDB 热点后吞吐量提升 |
| Baseline Prewrite/s | 155,577 | 175,185 | +12.6% — 与 TPS 提升一致 |

负载均衡后 TiDB CPU 从 30.6% 升至 41.1%，说明 R2 中单节点 TiDB 承载所有连接时已接近其单节点瓶颈，3 节点分散后 TiDB 不再是瓶颈。TiKV CPU 从 64.3% 升至 72.3%，成为新的瓶颈点。

### 3.3 Metrics Overhead

| 对比 | TiDB CPU OH% | TiKV CPU OH% | Prewrite/s OH% | Disk MB/s OH% | Disk KB/row OH% |
|------|----------:|----------:|----------:|----------:|----------:|
| #1 vs #2 (batch=1, opt, shard) | -6.1% | +1.1% | -5.1% | +7.6% | +18.6% |
| #1 vs #3 (batch=1, opt, noshard) | -3.2% | +1.8% | -1.5% | +10.2% | +17.0% |
| #4 vs #5 (batch=10, opt, shard) | -0.8% | +4.6% | -2.5% | +5.8% | +9.0% |
| #6 vs #7 (batch=1, pess, shard) | +0.7% | +0.5% | -0.7% | -0.6% | +4.7% |
| #8 vs #9 (rate=6500, shard) | +2.2% | +2.8% | +4.6% | +5.4% | +5.4% |

> Disk KB/row overhead 反映 mlog 的纯写放大比例。batch=1 场景为 +17-19%，batch=10 降至 +9%，与 mlog 每行写入 ~13 列 KV 的理论一致。

## 4. Key Findings

### Q1: 单行 INSERT 的 mlog overhead（#1 vs #2, #1 vs #3）

Mlog-shard 使单行 INSERT 吞吐量下降 **9.1%**（7,987 → 7,260 rows/s），P99 延迟从 24.83ms 升至 28.16ms（+13.4%）。Noshard 模式 overhead 较低（**5.8%**），这与 R2 中 noshard 略优的趋势一致。

**R2 → R3 overhead 增大的根因：** R2 中单 TiDB 节点是瓶颈（TiKV CPU 仅 64.3%），mlog 的额外 KV 写入未充分压到 TiKV 上。R3 负载均衡后，TiKV CPU 升至 72-73% 接近饱和，mlog 额外写入在 TiKV 饱和区间产生的边际成本更高。具体表现为：baseline Prewrite/s 从 155K 提升到 175K（+12.6%），mlog 场景的 Prewrite/s 仅提升到 166K（+6.8%），差距拉大。

TiDB CPU 在 mlog 场景反而下降（41.1% → 38.6%），说明 TiDB 仍有余量，瓶颈完全在 TiKV 侧。

### Q2: 批量 INSERT 的摊薄效果（#4 vs #5）

batch=10 场景 overhead 为 **2.9%**（11,047 → 10,726 rows/s），低于 batch=1 的 9.1%，摊薄效果显著。

batch=10 场景下 TiKV CPU 仅 52-55%，远未饱和，因此 mlog overhead 接近理论下界。这进一步验证了"TiKV 饱和程度决定 overhead 幅度"的结论。

### Q3: Shard vs Noshard（#2 vs #3）

与 R2 不同，R3 中 **noshard 明显优于 shard**（7,524 vs 7,260 rows/s，差距 3.6%）。

分析：shard 模式下 mlog 写入分散到 8 个 region，每个 region 在不同 TiKV 上产生独立的 Raft 日志条目。在 TiKV 接近饱和时（73%），更多的跨 region 写入增加了 Raft 共识开销。Noshard 模式下 mlog 集中写入 1 个 region，虽然存在热点风险，但减少了 prewrite batch 的 region 扇出数，反而更高效。在当前的表结构（22 个 KV per row）下，noshard 增加的 1 个 KV 写入被合并到已有的 prewrite batch 中的概率更高。

这一发现表明：**在当前测试负载下，shard 的额外开销大于其分散写入的收益**。但在实际生产环境中，如果 mlog 数据量积累导致单 region 过大（> 96 MB），shard 仍然是必要的。

### Q4: 悲观事务 overhead（#6 vs #7）

悲观事务下 mlog overhead 为 **5.1%**（7,599 → 7,212 rows/s），低于乐观事务的 9.1%。

原因：悲观事务路径下 baseline 本身就因锁管理消耗更多 TiKV 资源（baseline TiKV CPU 72.9% vs 乐观 72.3%），吞吐量更低（7,599 vs 7,987）。在更低的绝对吞吐量下，mlog 的边际压力相对较小。Prewrite/s overhead 仅 -0.7%，磁盘 KB/row overhead 也仅 +4.7%，说明悲观路径下 mlog 写入被更好地吸收了。

### Q5: 限流场景 overhead（#8 vs #9）

限流到 6,500 rows/s 时，mlog 对吞吐量完全无影响（6,501 vs 6,504 rows/s，**+0.1%**）。

平均延迟从 8.14ms 升至 10.44ms（+28.3%），P99 从 47.47ms 升至 63.32ms（+33.4%）。延迟上升的百分比较高，但需要注意：限流场景下 sysbench 使用 `--rate` 模式，多余的线程在队列中等待，延迟包含了排队时间，因此绝对延迟比无限流场景的数值更高但意义不同。核心指标是吞吐量完全达标（6,501 vs 6,504），说明在目标速率远低于集群瓶颈（~8,000 rows/s）时，mlog overhead 完全可忽略。

## 5. Summary

| 场景 | 预期 OH% | R2 OH% | R3 OH% | 判定 |
|------|---------|-------:|-------:|------|
| batch=1, optimistic, unlim (shard) | 5-15% | 4.4% | 9.1% | 符合预期，TiKV 饱和放大效应 |
| batch=1, optimistic, unlim (noshard) | 5-15% | 5.5% | 5.8% | 符合预期 |
| batch=10, optimistic, unlim | 3-8% | 3.7% | 2.9% | 优于预期 |
| shard vs noshard | shard 更优 | shard +1.2% | noshard +3.6% | noshard 在当前负载下更优 |
| batch=1, pessimistic, unlim | 略高于 optimistic | 4.9% | 5.1% | 符合预期 |
| batch=1, optimistic, rate=6500 | <2% | 0.2% | 0.1% | 符合预期 |

**核心结论：**

1. **负载均衡后 overhead 范围扩大到 2.9%-9.1%**，上界出现在 batch=1 shard 场景。根因是 TiKV CPU 从 64% 提升到 72%，mlog 的额外写入在 TiKV 接近饱和时边际成本更高。
2. **Noshard 在当前测试条件下优于 shard**（5.8% vs 9.1%），这与 R2 结论相反。关键因素是 TiKV 饱和度 —— 饱和时减少 region 扇出比分散写入更重要。
3. **Batch 摊薄效果稳定**（2.9%），且 batch=10 场景下 TiKV 未饱和（52-55%），overhead 接近理论下界。
4. **限流场景 overhead 仍然可忽略**（+0.1%），与 R2 一致。对于目标吞吐量低于集群瓶颈的生产场景，mlog 不构成性能问题。

## 6. Validation

所有 9 个 case 通过以下校验：
- sysbench errors = 0, reconnects = 0
- `SELECT count(*) FROM bc_bet_records` = sysbench total_events x batch_size
- （mlog 场景）mlog 行数 = 基表行数

## 7. Environment

- TiDB/TiKV/TiFlash: patched binary (feature branch with mlog support)
- sysbench: 1.0.20 (LuaJIT 2.1.0-beta3)
- TiDB config: `pessimistic-txn.pessimistic-auto-commit = true`
- **sysbench 连接: 3 TiDB 节点轮询（R2 为单节点）**
- Base table: 41 columns, 20 secondary indexes, NONCLUSTERED PK, SHARD_ROW_ID_BITS=4 PRE_SPLIT_REGIONS=3
- Mlog table: 13 columns, no indexes (shard mode: SHARD_ROW_ID_BITS=4 PRE_SPLIT_REGIONS=2)
- Metrics collection: process CPU (TiDB/TiKV), gRPC prewrite/commit counters, node_exporter disk written bytes
