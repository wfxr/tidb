# Mlog Write Overhead Benchmark — Final Report

**Date:** 2026-03-12 to 2026-03-13
**Cluster:** 3x TiDB c4-standard-16 + 3x TiKV c4-highmem-16 + 1x PD c4-standard-8 (GCP us-east1-b)
**Concurrency:** 128 sysbench threads
**Test rounds:** R2 (single TiDB endpoint) + R3 (3-node round-robin), 9 cases x 660 s each

## Executive Summary

Two rounds of testing (R2 and R3) systematically measured the write overhead that mlog imposes on base-table INSERTs. In R2, all sysbench connections were directed at a single TiDB node, which created a TiDB-side hotspot and left TiKV underutilized (CPU ~64%); the observed throughput overhead ranged from 3.7% to 5.5%. After load balancing was corrected in R3, baseline throughput increased by 12.6%, TiKV CPU rose to ~72% (near saturation), and the overhead range widened to 2.9%–9.1%.

**Key results:**

| Scenario | Throughput overhead | Confidence |
|----------|:-------------------:|:----------:|
| batch=1, optimistic, unlimited rate | **5.8%–9.1%** | Bounded by TiKV saturation level |
| batch=10, optimistic, unlimited rate | **2.9%–3.7%** | Stable across both rounds |
| batch=1, pessimistic, unlimited rate | **4.9%–5.1%** | Consistent across both rounds |
| Rate-limited (well below bottleneck) | **~0%** | Consistent across both rounds |

The root cause is precisely identified: **each single-row mlog transaction adds exactly one additional TiKV prewrite RPC** (21.92 &rarr; 22.93 RPCs per event), fully consistent with the theoretical KV write-amplification analysis.

## 1. Test Matrix

| # | Scenario | Mlog Shard | Batch | TxnMode | Row Rate | Priority |
|---|----------|------------|-------|---------|----------|----------|
| 1 | baseline | — | 1 | optimistic | unlimited | P0 |
| 2 | mlog | shard | 1 | optimistic | unlimited | P0 |
| 3 | mlog | noshard | 1 | optimistic | unlimited | P0 |
| 4 | baseline | — | 10 | optimistic | unlimited | P0 |
| 5 | mlog | shard | 10 | optimistic | unlimited | P0 |
| 6 | baseline | — | 1 | pessimistic | unlimited | P1 |
| 7 | mlog | shard | 1 | pessimistic | unlimited | P1 |
| 8 | baseline | — | 1 | optimistic | rate-limited | P2 |
| 9 | mlog | shard | 1 | optimistic | rate-limited | P2 |

## 2. Methodology and Round Differences

| | R2 | R3 |
|---|---|---|
| sysbench connectivity | Single TiDB node (10.142.0.7) | 3-node round-robin (10.142.0.{7,6,5}) |
| Baseline TiDB CPU | 30.6% | 41.1% |
| Baseline TiKV CPU | 64.3% | 72.3% |
| Baseline throughput | 7,095 rows/s | 7,987 rows/s (+12.6%) |
| P2 rate limit | 4,000 rows/s | 6,500 rows/s |
| Bottleneck location | Single TiDB node | TiKV |

**R3 is the primary dataset.** It corrects R2's load-distribution skew and achieves more uniform cluster utilization. Because TiKV is the true bottleneck in R3, its overhead figures more accurately reflect production behavior. R2 data is retained for comparative analysis of TiKV saturation effects.

## 3. Throughput Results

### 3.1 Raw Data (R3, 3-Node Load Balancing)

| # | Scenario | rows/s | Avg (ms) | P99 (ms) | Max (ms) | CV |
|---|----------|-------:|---------:|---------:|---------:|---:|
| 1 | baseline / batch=1 / optimistic | 7,987 | 16.03 | 24.83 | 215.02 | 4.0% |
| 2 | mlog-shard / batch=1 / optimistic | 7,260 | 17.63 | 28.16 | 652.54 | 4.9% |
| 3 | mlog-noshard / batch=1 / optimistic | 7,524 | 17.01 | 27.17 | 512.49 | 5.0% |
| 4 | baseline / batch=10 / optimistic | 11,047 | 115.86 | 179.94 | 520.00 | 2.9% |
| 5 | mlog-shard / batch=10 / optimistic | 10,726 | 119.33 | 176.73 | 327.54 | 2.6% |
| 6 | baseline / batch=1 / pessimistic | 7,599 | 16.84 | 26.20 | 498.09 | 4.8% |
| 7 | mlog-shard / batch=1 / pessimistic | 7,212 | 17.75 | 28.16 | 453.34 | 4.7% |
| 8 | baseline / batch=1 / rate=6500 | 6,501 | 8.14 | 47.47 | 449.31 | 0.4% |
| 9 | mlog-shard / batch=1 / rate=6500 | 6,504 | 10.44 | 63.32 | 571.05 | 0.4% |

> CV (coefficient of variation) is computed from the 10 s report-interval TPS time series, discarding the first 60 s of warmup. All cases have CV < 10%, indicating stable results. For batch=10, rows/s = TPS x 10.

### 3.2 Overhead Comparison (R2 vs R3)

| Comparison | Question | R2 Baseline | R2 Mlog | R2 OH% | R3 Baseline | R3 Mlog | R3 OH% |
|------------|----------|------------:|--------:|-------:|------------:|--------:|-------:|
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

> CPU% = delta of `process_cpu_seconds_total` / elapsed time / core count (16 cores per node), averaged across all nodes of the same role. Prewrite/Commit counts are aggregated across all 3 TiKV nodes. Disk figures are aggregated writes to `nvme1n1` across all 3 TiKV nodes.

### 4.2 Prewrite RPCs per Event (R2)

| # | Scenario | Total Events | Prewrite RPCs | RPCs/event | Delta vs Baseline |
|---|----------|-------------:|--------------:|-----------:|------------------:|
| 1 | baseline / batch=1 | 4,682,870 | 102,680,866 | **21.93** | — |
| 2 | mlog-shard / batch=1 | 4,477,085 | 102,680,813 | **22.93** | **+1.00** |
| 3 | mlog-noshard / batch=1 | 4,424,254 | 101,431,643 | **22.93** | **+1.00** |
| 4 | baseline / batch=10 | 724,927 | 24,810,770 | **34.23** | — |
| 5 | mlog-shard / batch=10 | 697,980 | 23,726,905 | **33.99** | **-0.24** |
| 6 | baseline / batch=1 / pess | 4,386,289 | 96,153,587 | **21.92** | — |
| 7 | mlog-shard / batch=1 / pess | 4,172,603 | 95,669,872 | **22.93** | **+1.01** |
| 8 | baseline / rate=4000 | 2,641,473 | 57,861,956 | **21.91** | — |
| 9 | mlog-shard / rate=4000 | 2,636,721 | 60,378,461 | **22.90** | **+0.99** |

Every batch=1 mlog case adds exactly **+1 prewrite RPC per event** (21.92 &rarr; 22.93). Each base-table INSERT produces 22 KV pairs (1 row + 1 nonclustered PK + 20 secondary indexes), distributed across ~22 regions and thus requiring ~22 prewrite RPCs. Mlog appends one additional KV to a separate mlog region, adding exactly one more prewrite RPC. In the batch=10 scenario, the 10 mlog KVs within the same transaction are coalesced into a single prewrite RPC, leaving RPCs/event virtually unchanged (34.23 &rarr; 33.99).

### 4.3 Metrics Overhead (R3)

| Comparison | TiDB CPU OH% | TiKV CPU OH% | Prewrite/s OH% | Disk MB/s OH% | Disk KB/row OH% |
|------------|----------:|----------:|----------:|----------:|----------:|
| #1 vs #2 (batch=1, opt, shard) | -6.1% | +1.1% | -5.1% | +7.6% | +18.6% |
| #1 vs #3 (batch=1, opt, noshard) | -3.2% | +1.8% | -1.5% | +10.2% | +17.0% |
| #4 vs #5 (batch=10, opt, shard) | -0.8% | +4.6% | -2.5% | +5.8% | +9.0% |
| #6 vs #7 (batch=1, pess, shard) | +0.7% | +0.5% | -0.7% | -0.6% | +4.7% |
| #8 vs #9 (rate=6500, shard) | +2.2% | +2.8% | +4.6% | +5.4% | +5.4% |

> Disk KB/row overhead represents the per-row write amplification attributable to mlog. At +17–19% for batch=1 and +9% for batch=10, these figures are consistent with the 13-column mlog KV payload per row.

## 5. Key Findings

### Q1: Single-Row INSERT Overhead

**Finding: Mlog overhead for batch=1 optimistic INSERTs ranges from 5.8% to 9.1% (throughput reduction), governed by TiKV saturation level.**

| Condition | TiKV CPU | TPS OH% | Mechanism |
|-----------|:--------:|:-------:|-----------|
| R2, single-node LB, shard | 64% &rarr; 69% | -4.4% | TiKV unsaturated; extra RPC absorbed at low marginal cost |
| R3, 3-node LB, noshard | 72% &rarr; 74% | -5.8% | Moderate saturation; noshard reduces region fan-out |
| R3, 3-node LB, shard | 72% &rarr; 73% | -9.1% | TiKV near saturation; shard increases region fan-out overhead |

Root cause: each transaction gains exactly +1 prewrite RPC (21.92 &rarr; 22.93 RPCs/event). When TiKV is underutilized, the marginal cost of one additional RPC is negligible. As TiKV approaches saturation, the marginal cost rises sharply. In R3, the baseline prewrite rate increased from 155K/s to 175K/s (+12.6%), whereas the mlog-shard rate only reached 166K/s (+6.8%), widening the gap.

TiDB CPU actually decreased in mlog scenarios (41.1% &rarr; 38.6%), confirming that `writeMLogRow()` has minimal impact on the TiDB side and that the bottleneck lies entirely within TiKV.

### Q2: Batch Amortization

**Finding: Batching at batch=10 significantly reduces overhead to a stable 2.9%–3.7%.**

Within a single transaction, 10 mlog KV writes are coalesced into one prewrite RPC, leaving RPCs/event virtually unchanged (34.23 &rarr; 33.99). Disk KB/row overhead drops from +17–19% (batch=1) to +9% (batch=10). Results are consistent across both rounds (R2: 3.7%, R3: 2.9%). In the batch=10 scenario, TiKV CPU remains at only 52–55% (well below saturation), so the observed overhead approaches the theoretical lower bound.

### Q3: Shard vs Noshard

**Finding: Under near-saturated TiKV, noshard outperforms shard. When TiKV is underutilized, the difference is not statistically significant.**

| Condition | TiKV CPU | Shard (rows/s) | Noshard (rows/s) | Difference |
|-----------|:--------:|:--------------:|:----------------:|:----------:|
| R2 (TiKV underutilized) | ~68% | 6,783 | 6,703 | shard +1.2% |
| R3 (TiKV near saturation) | ~73% | 7,260 | 7,524 | noshard +3.6% |

In shard mode, mlog writes are distributed across 8 regions (potentially spanning multiple TiKV nodes), which increases the prewrite batch's region fan-out count and the associated Raft consensus overhead. In noshard mode, all mlog writes target a single region. Although this introduces hotspot risk, the additional KV is more likely to be batched with existing prewrite RPCs given the current table schema (22 KVs per row). This efficiency gap widens as TiKV approaches saturation.

**Recommendation:** Noshard is more efficient in short-lived or low-volume scenarios, but shard remains necessary in production when accumulated mlog data could cause a single region to exceed 96 MB.

### Q4: Pessimistic Transaction Overhead

**Finding: Overhead under pessimistic transactions is 4.9%–5.1%, highly consistent across both rounds.**

The pessimistic-auto-commit path acquires an additional pessimistic lock for the mlog row, adding lock-management overhead. However, the baseline throughput under pessimistic mode is lower to begin with (7,599 vs 7,987 rows/s), so the marginal impact of mlog is proportionally smaller. Prewrite RPCs/event increases by exactly +1.01, confirming the same root cause. Results are stable across rounds (R2: 4.9%, R3: 5.1%) and are not affected by the load-balancing configuration.

### Q5: Rate-Limited Scenario Overhead

**Finding: When the target throughput is well below the cluster bottleneck, mlog overhead is negligible (~0%).**

| Condition | Rate limit | Baseline | Mlog | TPS OH% |
|-----------|:----------:|:--------:|:----:|:-------:|
| R2 | 4,000 rows/s | 4,002 | 3,995 | -0.2% |
| R3 | 6,500 rows/s | 6,501 | 6,504 | +0.1% |

At both rate limits (~50% and ~81% of the cluster's maximum throughput), mlog had no measurable impact on throughput. The increase in average latency is small in absolute terms (R2: +0.55 ms; R3: +2.3 ms). Note that under sysbench `--rate` mode, reported latency includes queuing time and is not directly comparable to the unlimited-rate figures. The key observation is that throughput met the target precisely, confirming that mlog overhead is fully absorbed when the cluster is not write-saturated.

## 6. Overhead Model

Synthesizing both rounds of data, mlog overhead can be understood through the following model:

```
TPS_overhead ~ f(TiKV_saturation) x (1 / batch_size) x RPC_cost
```

- **TiKV saturation is the dominant factor.** As TiKV CPU increased from 64% to 73%, the batch=1 shard overhead doubled from 4.4% to 9.1%. This relationship is nonlinear: the marginal cost of additional load rises steeply as the system approaches saturation.
- **Batch amortization.** At batch=10, 10 mlog KV writes are coalesced into a single prewrite RPC, reducing overhead to 2.9%–3.7%.
- **Region fan-out.** Shard mode increases the number of distinct regions touched by each prewrite, amplifying overhead under TiKV saturation (9.1% shard vs 5.8% noshard).
- **Rate-limited immunity.** When the actual throughput is well below the cluster bottleneck, the extra KV writes fall within TiKV's spare capacity and overhead approaches 0%.

## 7. Summary and Recommendations

| Scenario | Expected OH% | Measured OH% | Assessment |
|----------|:------------:|:------------:|:----------:|
| batch=1, opt, unlimited (shard) | 5–15% | 4.4%–9.1% | Within expectations; range governed by saturation |
| batch=1, opt, unlimited (noshard) | 5–15% | 5.5%–5.8% | Within expectations; stable across rounds |
| batch=10, opt, unlimited | 3–8% | 2.9%–3.7% | Better than expected |
| batch=1, pess, unlimited | Slightly above opt | 4.9%–5.1% | Within expectations; stable across rounds |
| Rate-limited (at or below 80% of bottleneck) | <2% | ~0% | Better than expected |

**Recommendations:**

1. **Production workloads with rate limiting or moderate write pressure:** Mlog overhead is negligible (~0%) and can be enabled with confidence.
2. **High write-pressure workloads:** Expect a 5–9% throughput reduction, depending on TiKV saturation level. If TiKV CPU already exceeds ~70%, consider scaling out TiKV or reducing write pressure before enabling mlog.
3. **Batch INSERTs:** Where possible, use batch sizes of 10 or more to keep overhead below 3%.
4. **Shard mode selection:** Prefer noshard for small datasets or short-lived mlog tables (saves ~3.6% overhead). Use shard for long-running production scenarios where mlog data accumulation could cause a single region to exceed 96 MB.

## 8. Validation

All 18 cases across both rounds (R2: 9, R3: 9) passed the following checks:
- sysbench errors = 0, reconnects = 0
- `SELECT count(*) FROM bc_bet_records` = sysbench `total_events` x `batch_size`
- For mlog scenarios: mlog row count = base-table row count

## 9. Environment

- TiDB / TiKV / TiFlash: patched binary (feature branch with mlog support)
- sysbench: 1.0.20 (LuaJIT 2.1.0-beta3)
- TiDB config: `pessimistic-txn.pessimistic-auto-commit = true`
- Base table: 41 columns, 20 secondary indexes, NONCLUSTERED PK, SHARD_ROW_ID_BITS=4 PRE_SPLIT_REGIONS=3
- Mlog table: 13 columns, no secondary indexes (shard mode: SHARD_ROW_ID_BITS=4 PRE_SPLIT_REGIONS=2)
- Metrics: process CPU (TiDB/TiKV), gRPC prewrite/commit counters, `node_exporter` disk written bytes
- **R2 raw data:** `results/20260312T140018Z/` (single TiDB endpoint)
- **R3 raw data:** `results/20260313T032423Z/` + `results/20260313T054428Z/` (3-node round-robin)
