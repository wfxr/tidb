# 乐观 vs 悲观事务：验证 PessLock 对 Mlog TiDB CPU 的影响

**Date**: 2026-03-18 | **Pessimistic**: `results/20260318T080530Z` | **Optimistic**: `results/20260318T091052Z` | **Duration**: 660s/case

## TL;DR

通过对比悲观/乐观事务下同一测试矩阵的结果，验证了**悲观锁 RPC 是悲观模式下 mlog TiDB CPU 高于 index 的唯一显著原因**：
- 悲观模式：mlog TiDB CPU OH% = **+26.1%**，index = +15.9%，mlog 高 10.2pp
- 乐观模式：mlog TiDB CPU OH% = **+15.9%**，index = +17.9%，mlog 反而**低 2.0pp**
- 切换到乐观事务后 CPU 差距完全消失并反转，证实悲观锁是唯一变量

## Test Setup

两轮测试配置完全相同，仅事务模式不同：

- **基表**: `bc_bet_records`，42 列，NONCLUSTERED PK，无 SHARD（单 region）
- **索引**: `KEY idx_mlog_cols (...)` — 11 列非唯一索引
- **Mlog**: `CREATE MATERIALIZED VIEW LOG ON bc_bet_records (...)` 无 SHARD（单 region）
- **限流**: 18,000 rows/s（sysbench --rate）
- **并发**: 128 threads, 3 TiDB nodes round-robin
- **集群**: 3×TiDB (c4-standard-16) + 3×TiKV (c4-highmem-16) + 1×PD

| Case | 场景 | 悲观轮 txn_mode | 乐观轮 txn_mode |
|------|------|----------------|----------------|
| 1 | baseline | pessimistic | optimistic |
| 2 | index | pessimistic | optimistic |
| 3 | mlog-noshard | pessimistic | optimistic |

## Results

### 1. TiDB CPU 对比（核心验证）

| | 悲观 Baseline | 悲观 Index | 悲观 Mlog | 乐观 Baseline | 乐观 Index | 乐观 Mlog |
|--|--------------|-----------|----------|--------------|-----------|----------|
| TiDB CPU | 34.5% | 40.0% | 43.5% | 30.1% | 35.5% | 34.9% |
| vs Baseline | — | +15.9% | **+26.1%** | — | +17.9% | **+15.9%** |
| Mlog - Index 差距 | | | **+3.5pp** | | | **-0.6pp** |

悲观模式下 mlog 比 index 高 3.5pp；乐观模式下 mlog 比 index **低** 0.6pp。差距从 +3.5pp → -0.6pp，完全消失。

### 2. PessLock/s 对比

| | 悲观 Baseline | 悲观 Index | 悲观 Mlog | 乐观 Baseline | 乐观 Index | 乐观 Mlog |
|--|--------------|-----------|----------|--------------|-----------|----------|
| PessLock/s | 34,914 | 35,056 | **52,847** | 0 | 1 | 0 |
| vs Baseline | — | +0.4% | **+51.4%** | — | — | — |

乐观事务下三个 case 的 PessLock/s 均为 ~0，消除了悲观锁这一变量。

### 3. 完整指标对比

#### 悲观事务 (`20260318T080530Z`)

| | Baseline | Index | vs BL | Mlog | vs BL |
|--|----------|-------|-------|------|-------|
| TiDB CPU | 34.5% | 40.0% | +15.9% | 43.5% | **+26.1%** |
| TiKV CPU | 25.5% | 37.8% | +48.2% | 37.4% | +46.7% |
| PessLock/s | 34,914 | 35,056 | +0.4% | 52,847 | **+51.4%** |
| Prewrite/s | 34,919 | 49,921 | +43.0% | 52,864 | +51.4% |
| Commit/s | 33,863 | 48,979 | +44.6% | 52,860 | +56.1% |
| Disk KB/row | 6.55 | 11.08 | +69.2% | 9.15 | +39.8% |
| Avg latency | 3.35 ms | 3.84 ms | +14.6% | 3.65 ms | +9.0% |
| P99 latency | 5.00 ms | 5.57 ms | +11.4% | 5.28 ms | +5.6% |

#### 乐观事务 (`20260318T091052Z`)

| | Baseline | Index | vs BL | Mlog | vs BL |
|--|----------|-------|-------|------|-------|
| TiDB CPU | 30.1% | 35.5% | +17.9% | 34.9% | **+15.9%** |
| TiKV CPU | 20.9% | 33.6% | +60.8% | 28.6% | +36.8% |
| PessLock/s | 0 | 1 | — | 0 | — |
| Prewrite/s | 34,894 | 49,809 | +42.7% | 52,882 | +51.6% |
| Commit/s | 33,800 | 48,786 | +44.3% | 52,877 | +56.4% |
| Disk KB/row | 9.45 | 17.38 | +83.8% | 13.53 | +43.1% |
| Avg latency | 2.91 ms | 3.29 ms | +13.1% | 3.06 ms | +5.2% |
| P99 latency | 4.82 ms | 4.82 ms | +0.0% | 4.41 ms | -8.5% |

### 4. 悲观 → 乐观切换的变化量

| 指标 | Index 变化 | Mlog 变化 | 说明 |
|------|-----------|----------|------|
| TiDB CPU OH% | +15.9% → +17.9% | **+26.1% → +15.9%** | mlog 下降 10pp，index 不变 |
| TiKV CPU OH% | +48.2% → +60.8% | +46.7% → +36.8% | mlog 下降，index 上升 |
| Prewrite/s OH% | +43.0% → +42.7% | +51.4% → +51.6% | 不变（预期内） |
| Commit/s OH% | +44.6% → +44.3% | +56.1% → +56.4% | 不变（预期内） |
| Disk KB/row OH% | +69.2% → +83.8% | +39.8% → +43.1% | 磁盘比例稳定 |

- **TiDB CPU**：mlog OH% 下降了 10.2pp（从 +26.1% 到 +15.9%），index 基本不变（+15.9% → +17.9%）。这与"悲观锁 RPC 只影响 mlog、不影响 index"完全一致。
- **Prewrite/Commit**：两轮测试的 RPC 增幅几乎相同，说明乐观/悲观切换不影响 prewrite/commit 路径。
- **TiKV CPU**：乐观模式下 index 的 TiKV CPU OH% 从 +48.2% 上升到 +60.8%，而 mlog 从 +46.7% 下降到 +36.8%。可能与乐观模式下 TiKV 侧的 latch 竞争模式变化有关。

### 5. 延迟对比

| | 悲观 Index | 悲观 Mlog | 乐观 Index | 乐观 Mlog |
|--|-----------|----------|-----------|----------|
| Avg latency | 3.84 ms | 3.65 ms | 3.29 ms | 3.06 ms |
| P99 latency | 5.57 ms | 5.28 ms | 4.82 ms | 4.41 ms |

两种事务模式下 mlog 延迟均低于 index。乐观模式整体延迟更低（少了一个 pessimistic lock 网络往返）。

## Conclusion

1. **悲观锁 RPC 是 mlog TiDB CPU 高于 index 的唯一显著原因**。消除悲观锁后（乐观模式），mlog TiDB CPU OH% 从 +26.1% 降至 +15.9%，与 index (+17.9%) 持平甚至略低。
2. **优化 mlog 跳过悲观锁的预期收益得到实测验证**：TiDB CPU OH% 预期从 +26% 降至 ~+16%，与乐观模式的实测值 +15.9% 吻合。
3. **Mlog 在所有其他维度均优于或持平 index**：磁盘写入少 17–22%，延迟更低，TiKV CPU 更低。悲观锁是 mlog 唯一的"劣势项"，且可通过代码优化消除。
4. **优化建议**：在 mlog mutation 构造时标记 `skipPessimisticLock`，使其在悲观事务中跳过 `kv_pessimistic_lock` RPC。mlog 行的 key 为内部自动生成，不存在并发冲突，跳过悲观锁不影响正确性。
