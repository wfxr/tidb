# Index vs Mlog (No-Shard): Write Overhead Comparison

**Date**: 2026-03-16 | **Results**: `results/20260316T072638Z` | **Duration**: 660s/case

## TL;DR

在统一 no-shard 条件下（基表、mlog 均为单 region），限流 18,000 rows/s 场景：
- **Mlog 磁盘写入比 index 少 18%**（13.71 vs 16.73 KB/row）
- **延迟几乎相同**（avg 3.82 vs 3.86 ms，P99 5.88 vs 5.67 ms）
- **TiKV CPU 接近**（+46.0% vs +49.8%，mlog 略低）
- **TiDB CPU mlog 更高**（+24.0% vs +16.5%）

两者开销在同一量级，mlog 在磁盘 I/O 方面优于 11 列宽索引。

## Test Setup

- **基表**: `bc_bet_records`，42 列，NONCLUSTERED PK，**无 SHARD_ROW_ID_BITS**（单 region）
- **索引**: `KEY idx_mlog_cols (site_code, account, ..., net_profit)` — 与 mlog 追踪的 11 列完全一致
- **Mlog**: `CREATE MATERIALIZED VIEW LOG ON bc_bet_records (...)` **无 SHARD**（单 region）
- **事务模式**: pessimistic (auto-commit)
- **限流**: 18,000 rows/s（sysbench --rate）
- **并发**: 128 threads, 3 TiDB nodes round-robin
- **集群**: 3×TiDB (c4-standard-16) + 3×TiKV (c4-highmem-16) + 1×PD

| Case | 场景 | 基表 Region | 附加结构 Region |
|------|------|-------------|-----------------|
| 1 | baseline (裸表) | 1 | — |
| 2 | + non-unique index | 1 | (index region) |
| 3 | + mlog (noshard) | 1 | 1 |

## Results

### 1. Latency

| | Baseline | Index | Mlog (noshard) |
|--|----------|-------|----------------|
| avg latency | 3.25 ms | 3.86 ms (+18.8%) | 3.82 ms (+17.5%) |
| P99 latency | 4.49 ms | 5.67 ms (+26.3%) | 5.88 ms (+30.9%) |
| max latency | 490.66 ms | 479.14 ms | 491.28 ms |

限流下平均延迟几乎相同，P99 mlog 略高约 4%。

### 2. Resource Cost (Rate-Limited @ 18,000 rows/s)

| | Baseline | Index | vs BL | Mlog | vs BL |
|--|----------|-------|-------|------|-------|
| TiDB CPU | 35.8% | 41.7% | +16.5% | 44.4% | **+24.0%** |
| TiKV CPU | 26.3% | 39.4% | **+49.8%** | 38.4% | +46.0% |
| Prewrite/s | 34,893 | 50,142 | +43.7% | 52,861 | **+51.5%** |
| Commit/s | 33,800 | 49,385 | +46.1% | 52,858 | **+56.4%** |
| Disk MB/s | 168.3 | 294.1 | **+74.8%** | 241.0 | +43.2% |
| Disk KB/row | 9.57 | 16.73 | **+74.8%** | 13.71 | +43.3% |

### 3. Index vs Mlog 直接对比

| 维度 | Index 更高 | Mlog 更高 | 差距 |
|------|-----------|-----------|------|
| 磁盘 KB/row | **16.73** | 13.71 | index 多写 22% |
| Disk MB/s OH% | **+74.8%** | +43.2% | index 写放大更严重 |
| TiKV CPU OH% | **+49.8%** | +46.0% | 接近 |
| TiDB CPU OH% | +16.5% | **+24.0%** | mlog SQL 层开销更大 |
| Prewrite/s OH% | +43.7% | **+51.5%** | mlog 多约 5% RPC |
| Avg latency | 3.86 ms | **3.82 ms** | 接近 (mlog 略低) |
| P99 latency | **5.67 ms** | 5.88 ms | mlog 略高 3.7% |

### 4. 与基表有 Shard 的对比 (Run 20260316T063855Z)

基表有 `SHARD_ROW_ID_BITS=4`（8 regions）时的同一测试：

| 指标 | 基表 Shard | 基表 No-Shard | 变化 |
|------|-----------|--------------|------|
| Baseline TiKV CPU | 37.1% | 26.3% | -29% |
| Baseline Disk KB/row | 10.70 | 9.57 | -11% |
| Index Disk KB/row | 18.80 | 16.73 | -11% |
| Mlog Disk KB/row | 15.21 | 13.71 | -10% |

去掉基表 shard 后所有场景的 TiKV CPU 和磁盘写入都有下降，因为 shard 模式下 PD 的 region 调度和 Raft 复制路径更分散。

## Conclusion

1. **Mlog (noshard) 磁盘写入优于等宽索引**：每行 13.71 KB vs 16.73 KB，少写约 18%。mlog 的 KV 编码比 11 列组合索引更紧凑。
2. **TiKV CPU 开销接近**：mlog +46.0% vs index +49.8%，mlog 反而略低。
3. **TiDB CPU mlog 更高**：+24.0% vs +16.5%，mlog 在 SQL 层有额外的行构造和编码开销。
4. **延迟几乎相同**：avg 差 1%，P99 mlog 高约 4%，实际无显著差异。
5. **整体结论**：在相同 no-shard 条件下，mlog 的写入开销与一个 11 列宽索引在同一量级，且磁盘 I/O 方面 mlog 更优。
