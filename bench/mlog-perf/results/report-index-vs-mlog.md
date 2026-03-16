# Non-Unique Index vs Mlog: Write Overhead Comparison

**Date**: 2026-03-13 | **Results**: `results/20260313T071427Z` | **Duration**: 360s/case

## TL;DR

在裸基表（仅 PK）上，添加一个 11 列非 unique 索引的吞吐量下降 **-13.8%**，添加 mlog 下降 **-17.6%**。
两者开销在同一量级，mlog 略高约 4 个百分点。开销特征不同：索引的磁盘写放大更大（+56%），mlog 的 TiKV CPU 开销更高（+39%）。

## Test Setup

- **基表**: `bc_bet_records`，42 列，仅 NONCLUSTERED PK，SHARD_ROW_ID_BITS=4
- **索引**: `KEY idx_mlog_cols (site_code, account, category_id, platform_id, game_id, currency, settle_day, settle_status, all_bet, valid_bet, net_profit)` — 与 mlog 追踪的 11 列完全一致
- **Mlog**: `CREATE MATERIALIZED VIEW LOG ON bc_bet_records (...)` with SHARD_ROW_ID_BITS=4
- **事务模式**: pessimistic (auto-commit)
- **并发**: 128 threads, 3 TiDB nodes round-robin
- **集群**: 3×TiDB (c4-standard-16) + 3×TiKV (c4-highmem-16) + 1×PD

| Case | 场景 | 限流 |
|------|------|------|
| 1 | baseline (裸表) | 无 |
| 2 | + non-unique index | 无 |
| 3 | + mlog (shard) | 无 |
| 4 | baseline (裸表) | 18,000 rows/s |
| 5 | + non-unique index | 18,000 rows/s |
| 6 | + mlog (shard) | 18,000 rows/s |

## Results

### 1. Throughput (Unlimited Rate)

| | Baseline | Index | Mlog |
|--|----------|-------|------|
| rows/s | **29,829** | **25,709** | **24,589** |
| overhead | — | **-13.8%** | **-17.6%** |
| avg latency | 4.29 ms | 4.98 ms | 5.20 ms |
| P99 latency | 6.91 ms | 7.98 ms | 8.43 ms |

Mlog 比 index 多约 4 个百分点的吞吐下降。

### 2. Resource Cost (Rate-Limited @ 18,000 rows/s)

限流到相同吞吐量，观察资源消耗差异：

| | Baseline | Index | vs BL | Mlog | vs BL |
|--|----------|-------|-------|------|-------|
| TiDB CPU | 36.6% | 40.6% | +10.9% | 44.7% | **+22.1%** |
| TiKV CPU | 35.0% | 41.0% | +17.1% | 48.6% | **+38.9%** |
| Prewrite/s | 34,854 | 47,894 | +37.4% | 52,802 | **+51.5%** |
| Commit/s | 33,725 | 47,516 | +40.9% | 52,801 | **+56.6%** |
| Disk MB/s | 197.7 | 308.8 | **+56.2%** | 267.2 | +35.1% |
| Disk KB/row | 11.25 | 17.57 | **+56.2%** | 15.20 | +35.1% |
| Avg latency | 3.29 ms | 4.17 ms | +26.7% | 4.12 ms | +25.2% |

### 3. Key Observations

**索引开销特征 — 磁盘写放大大**
- 每行磁盘写入 17.57 KB（baseline 11.25 KB），增加 56%
- 11 列组合索引的 KV entry 较大，写放大显著
- Prewrite/Commit RPC 增幅约 +37~41%（索引 KV 和数据 KV 在同一事务内）

**Mlog 开销特征 — CPU 开销高**
- Mlog 和基表写入在**同一个用户事务**内（同一轮 2PC），非独立事务
- Prewrite/Commit 增幅 +52~57%，高于索引的 +37~41%
- 每行磁盘写入 15.20 KB，增幅 35%，**低于索引的 56%**
- TiKV CPU 开销最高（+39%）

**直接对比: Index vs Mlog**

| 维度 | Index 更高 | Mlog 更高 |
|------|-----------|-----------|
| 磁盘写放大 | **+56% vs +35%** | |
| RPC 数量 | | **+52% vs +37%** |
| TiKV CPU | | **+39% vs +17%** |
| 吞吐下降 | | **-17.6% vs -13.8%** |
| 限流延迟 | 4.17 ms | 4.12 ms (接近) |

## Conclusion

1. **Mlog 开销略高于单个 11 列非 unique 索引**：压满场景下多约 4pp 的吞吐损失（-17.6% vs -13.8%）
2. **开销特征不同**：索引的磁盘写放大更大（+56% KB/row），mlog 的 TiKV CPU 开销更高（+39%）。两者都在同一用户事务内完成写入。
3. **限流场景下延迟接近**：两者的 avg latency 几乎相同（4.17 vs 4.12 ms），但 mlog 消耗更多 CPU
4. 两者开销在**同一量级**，mlog 并没有比一个宽索引贵太多
