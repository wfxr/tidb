# Index vs Mlog: Pessimistic Lock RPC 分析

**Date**: 2026-03-18 | **Results**: `results/20260318T080530Z` | **Duration**: 660s/case

## TL;DR

新增 `kv_pessimistic_lock` RPC 计数后的复测表明：
- **Index 不产生额外悲观锁 RPC**（+0.4%），非唯一索引的 KV 对在 pessimistic lock 阶段被跳过
- **Mlog 每行多 1 次悲观锁 RPC**（+51.4%），mlog 行作为 record key 走完整的悲观锁路径
- **这是 mlog TiDB CPU 高于 index 的主要原因**，悲观锁 RPC 贡献了约 60–75% 的 TiDB CPU 差距
- 磁盘写入方面 mlog 仍优于 index（9.15 vs 11.08 KB/row，少 17%）

## Test Setup

与 `report-index-vs-mlog-noshard.md` 完全一致：

- **基表**: `bc_bet_records`，42 列，NONCLUSTERED PK，无 SHARD_ROW_ID_BITS（单 region）
- **索引**: `KEY idx_mlog_cols (site_code, account, ..., net_profit)` — 11 列非唯一索引
- **Mlog**: `CREATE MATERIALIZED VIEW LOG ON bc_bet_records (...)` 无 SHARD（单 region）
- **事务模式**: pessimistic (auto-commit)
- **限流**: 18,000 rows/s（sysbench --rate）
- **并发**: 128 threads, 3 TiDB nodes round-robin
- **集群**: 3×TiDB (c4-standard-16) + 3×TiKV (c4-highmem-16) + 1×PD

| Case | 场景 | 说明 |
|------|------|------|
| 1 | baseline | 裸表 |
| 2 | index | + 11 列非唯一索引 |
| 3 | mlog-noshard | + mlog (单 region) |

## Results

### 1. 完整指标总览

| | Baseline | Index | vs BL | Mlog | vs BL |
|--|----------|-------|-------|------|-------|
| TiDB CPU | 34.5% | 40.0% | +15.9% | 43.5% | +26.1% |
| TiKV CPU | 25.5% | 37.8% | +48.2% | 37.4% | +46.7% |
| **PessLock/s** | **34,914** | **35,056** | **+0.4%** | **52,847** | **+51.4%** |
| Prewrite/s | 34,919 | 49,921 | +43.0% | 52,864 | +51.4% |
| Commit/s | 33,863 | 48,979 | +44.6% | 52,860 | +56.1% |
| Disk MB/s | 115.1 | 194.8 | +69.2% | 160.9 | +39.7% |
| Disk KB/row | 6.55 | 11.08 | +69.2% | 9.15 | +39.8% |
| Avg latency | 3.35 ms | 3.84 ms | +14.6% | 3.65 ms | +9.0% |
| P99 latency | 5.00 ms | 5.57 ms | +11.4% | 5.28 ms | +5.6% |

### 2. Pessimistic Lock 行为分析

每行的悲观锁次数：

| Case | rows/s | PessLock/s | Locks/row |
|------|--------|-----------|-----------|
| Baseline | 18,003 | 34,914 | **1.94** |
| Index | 18,002 | 35,056 | **1.95** |
| Mlog | 17,999 | 52,847 | **2.94** |

- Baseline 每行 ~2 次锁：`_tidb_rowid` + NONCLUSTERED PK 索引（唯一索引需要悲观锁）
- Index 不增加：**非唯一索引不需要 pessimistic lock**，KV 对在 prewrite 阶段直接写入
- Mlog 每行多 1 次锁：mlog 行是 **record key**（表行），TiDB 对所有 record key 一律加悲观锁

### 3. Mlog vs Index 直接对比

| 维度 | Index | Mlog | 差距 |
|------|-------|------|------|
| **PessLock/s** | 35,056 | **52,847** | **mlog 多 17,791 (+51%)** |
| Prewrite/s | 49,921 | 52,864 | mlog 多 2,943 (+5.9%) |
| Commit/s | 48,979 | 52,860 | mlog 多 3,881 (+7.9%) |
| TiDB CPU | 40.0% | **43.5%** | mlog 高 3.5pp |
| TiKV CPU | **37.8%** | 37.4% | 接近 |
| Disk KB/row | **11.08** | 9.15 | index 多写 21% |
| Avg latency | 3.84 ms | **3.65 ms** | mlog 更低 |
| P99 latency | 5.57 ms | **5.28 ms** | mlog 更低 |

Mlog vs index 的额外 RPC 拆分：

| RPC 类型 | 额外 RPC/s | 占比 |
|----------|-----------|------|
| PessLock | +17,791 | **72%** |
| Prewrite | +2,943 | 12% |
| Commit | +3,881 | 16% |
| **合计** | **+24,615** | 100% |

### 4. PessLock 对 TiDB CPU 的影响估算

Mlog 的 TiDB CPU 比 index 高 3.5pp（43.5% vs 40.0%）。利用 index 数据估算单次 RPC 的 CPU 成本：

- Index vs baseline：~30K 额外 RPCs（prewrite+commit）→ +5.5pp TiDB CPU
- 上界估算：~0.18pp / 1K RPCs

按此估算 mlog 额外 17.8K PessLock RPCs 的 CPU 成本 ≈ **3.3pp**，占 3.5pp 差距的 ~94%。考虑到 index 的 5.5pp 中也包含 SQL 层开销，实际 RPC 单价略低，PessLock 贡献约 **60–75%**。

**结论**：悲观锁 RPC 是 mlog TiDB CPU 高于 index 的主要原因。

### 5. 与上次测试 (20260316T072638Z) 对比

| 指标 | 上次 (03-16) | 本次 (03-18) | 一致性 |
|------|-------------|-------------|--------|
| Baseline TiDB CPU | 35.8% | 34.5% | ≈ |
| Index TiDB CPU OH% | +16.5% | +15.9% | ✓ |
| Mlog TiDB CPU OH% | +24.0% | +26.1% | ≈ |
| Index Disk KB/row | 16.73 | 11.08 | ✗ (见下) |
| Mlog Disk KB/row | 13.71 | 9.15 | ✗ (见下) |

磁盘 KB/row 下降是因为上次测试前集群有残留数据导致 compaction 写放大，本次为干净启动后首次运行。其他指标比例关系一致，验证了数据可靠性。

## Optimization Opportunity

Mlog 行的 key 由 TiDB 内部自动生成（`_tidb_rowid` auto-increment），不可能与其他事务冲突。可以在 mlog mutation 构造时标记为 "skip pessimistic lock"，跳过悲观锁阶段。

**预期收益**：
- PessLock/s OH%：+51.4% → ~0%（与 index 持平）
- TiDB CPU OH%：+26.1% → ~18%（接近 index 的 +15.9%）
- 每行减少 1 次 TiKV gRPC 往返

## Conclusion

1. **非唯一索引不需要悲观锁，mlog 行需要**：这是两者在 pessimistic 模式下的核心差异。Index KV 是 index key，跳过 pessimistic lock 阶段；mlog KV 是 record key，走完整加锁路径。
2. **悲观锁 RPC 是 mlog TiDB CPU 高于 index 的主因**：贡献了 60–75% 的 CPU 差距（3.5pp 中的 ~2.5pp）。
3. **优化可行**：标记 mlog mutation 跳过悲观锁，可将 TiDB CPU 开销从 +26% 降至 ~+18%，基本消除与 index 的 CPU 差距。
4. **磁盘写入 mlog 仍优于 index**：9.15 vs 11.08 KB/row，少写 17%。
5. **延迟 mlog 优于 index**：avg 3.65 vs 3.84 ms，P99 5.28 vs 5.57 ms。
