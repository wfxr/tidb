# Mlog Write Overhead Benchmark Design

## 目标

评估物化视图日志（mlog）对基表 INSERT 性能的影响。基表每次 INSERT 会在同一事务内额外写入一行 mlog，本测试量化这一额外写入带来的吞吐量下降和延迟上升。

## 背景

### mlog 写入机制

- 基表每条 INSERT 在同一事务中产生 1 行 mlog 写入（1:1 比例）
- mlog 表结构：11 个跟踪列 + 2 个系统列（`_MLOG$_DML_TYPE`, `_MLOG$_OLD_NEW`），无主键、无二级索引
- 写入路径：`writeMLogRow()`（`pkg/table/tables/mview_log.go`）
- mlog 逐行写入，无批量优化

### KV 写放大分析

基表（`bc_bet_records`）每次 INSERT 产生的 KV 对：

| 类型 | 数量 |
|------|------|
| 行数据 | 1 |
| NONCLUSTERED PK (`id`) | 1 |
| UNIQUE KEY (`order_no`, `platform_id`) | 1 |
| 普通二级索引 | 19 |
| **基表合计** | **22** |

mlog 每次 INSERT 产生 **1 KV 对**（无索引）。纯 KV 写入量增幅 ≈ 1/22 ≈ **4.5%**。实际 overhead 略高于此，因为 mlog 行可能落在不同 TiKV region，带来额外 RPC 和 Raft 日志开销。

## 测试方案

### 核心问题

| # | 问题 | 对比用例 |
|---|------|----------|
| Q1 | 单行 INSERT 的 mlog overhead | #1 vs #2 |
| Q2 | 批量 INSERT (batch=10) 是否摊薄开销 | #4 vs #5 |
| Q3 | mlog 不 shard 会劣化多少 | #2 vs #3 |
| Q4 | 悲观事务下 overhead 是否不同 | #6 vs #7，对比 Q1 |
| Q5 | 限流场景 overhead 是否可忽略 | #8 vs #9 |

### 测试矩阵

| # | Scenario | Mlog Shard | Batch | TxnMode | RowRate | 优先级 |
|---|----------|------------|-------|---------|---------|--------|
| 1 | baseline | - | 1 | optimistic | unlim | P0 |
| 2 | mlog | shard | 1 | optimistic | unlim | P0 |
| 3 | mlog | noshard | 1 | optimistic | unlim | P0 |
| 4 | baseline | - | 10 | optimistic | unlim | P0 |
| 5 | mlog | shard | 10 | optimistic | unlim | P0 |
| 6 | baseline | - | 1 | pessimistic | unlim | P1 |
| 7 | mlog | shard | 1 | pessimistic | unlim | P1 |
| 8 | baseline | - | 1 | optimistic | rate-X | P2 |
| 9 | mlog | shard | 1 | optimistic | rate-X | P2 |

其中 `rate-X` 表示 P2 的目标限流行速，不预先写死。先完成 P0，得到 batch=1 optimistic 下 baseline/mlog 的实测 rows/s，再选择一个明确低于两者瓶颈的目标值作为 `target_row_rate`。初始估计可从 5000 行/s 开始试探，但最终以预跑结果为准。

建议先跑 P0（~58 min），根据结果决定是否继续 P1/P2。全部跑完约 104 min。

### sysbench 参数

| 参数 | 值 | 说明 |
|------|----|------|
| `--threads` | 32 | 典型 OLTP 并发 |
| `--time` | 660 | 60s warmup + 600s 测量 |
| `--report-interval` | 10 | 每 10s 输出中间结果 |
| `--percentile` | 99 | p99 延迟 |

分析时丢弃前 60 秒 warmup 数据。每个用例记录 UTC 起止时间戳，便于对照 Grafana / TiDB Dashboard。

**Rate limit 换算：** sysbench `--rate` 限制 events/sec，每个 event 插入 `batch_size` 行，所以 `--rate = target_row_rate / batch_size`。

### 预期结果

| 场景 | 预期 overhead | 理由 |
|------|--------------|------|
| batch=1, unlim | **5-15%** | mlog 增加 ~4.5% KV 写入量，叠加 RPC/Raft 开销 |
| batch=10, unlim | **3-8%** | 批量写入摊薄事务提交开销，mlog 相对占比更小 |
| rate=X 行/s | **< 2%** | 目标速率低于 P0 实测瓶颈，额外写入不构成实质影响 |
| shard vs noshard | **shard 更优** | 分散写入热点，减少 latch 争用 |
| pessimistic | **略高于 optimistic** | 悲观事务有额外锁开销 |

## 实现设计

### Schema 管理

每个用例开始前 `DROP DATABASE` 重建，确保干净环境。

基表 DDL 见 `base_table.sql`（41 列，含 1 个 GENERATED STORED 列、20 个二级索引、NONCLUSTERED PK、`SHARD_ROW_ID_BITS=4 PRE_SPLIT_REGIONS=3`）。

mlog DDL 见 `mlog.sql`，shard 选项包裹在 `/*T! ... */` 注释中。通过 mysql 客户端参数控制 shard/noshard：
- **shard**：`mysql --comments < mlog.sql` — 保留 TiDB 注释
- **noshard**：`mysql --skip-comments < mlog.sql` — 剥离注释

### DDL 后准备流程

为避免把建表后的 split/scatter 调度抖动算进 benchmark，`run_bench.sh` 在每个用例开始前执行固定准备流程：

1. `SET SESSION tidb_scatter_region='table'`
2. `SET SESSION tidb_wait_split_region_finish=1`
3. `SET SESSION tidb_wait_split_region_timeout=300`
4. 创建基表
5. 创建 mlog（baseline 场景跳过）
6. 用 `SHOW TABLE ... REGIONS` 校验 region 数量和命名符合预期
7. 校验通过后再启动 sysbench warmup

这样可以尽量保证：
- **shard 场景**：pre-split 和 scatter 在压测前完成，避免把 DDL 后台任务混入测量窗口
- **noshard 场景**：确认 mlog 只有单个初始 region，作为 Q3 的稳定对照组

**推荐校验项：**

| 对象 | 校验 |
|------|------|
| 基表 | `SHOW TABLE bc_bet_records REGIONS` 返回数量符合 `SHARD_ROW_ID_BITS=4 PRE_SPLIT_REGIONS=3` 预期 |
| mlog-shard | `SHOW TABLE $mlog$... REGIONS` 至少看到 pre-split 生成的 region 名称 |
| mlog-noshard | `SHOW TABLE $mlog$... REGIONS` 仅 1 个初始 region |

若校验失败，标记 `PREPARE FAIL` 并重建该用例，不进入压测。

### 数据生成（`mlog_insert.lua`）

sysbench 自定义 Lua 脚本，向基表的 40 个可插入列（排除 `settle_day` GENERATED 列）写入随机数据。

**唯一键冲突避免：** `id` 采用 `(thread_id << 48) | counter` snowflake 风格编码；`order_no` / `record_id` 编码 `"{prefix}-{tid}-{counter}"`，每线程独立递增，无需协调。

**数据分布：**

| 列 | 分布 |
|----|------|
| `account` | 100 万基数均匀分布 |
| `site_code` | 50 个值 |
| `platform_id` / `category_id` | 各 20 个值 |
| `game_id` | 2000 个值 |
| `currency` | 5 个值 |
| `settle_status` | 已结算/未结算/撤销 = 80%/19%/1% |
| `bet_time` / `settle_time_zone` | 最近 90 天随机 |
| `settle_date` | 与 `settle_time_zone` 同日 |
| DECIMAL 列 | 随机合理范围值 |

**批量 INSERT：** 拼接 `INSERT INTO ... VALUES (...), (...), ...` 多行语句。

### 事务模式控制

P1 的目标不是显式 `BEGIN PESSIMISTIC` 多语句事务，而是 **autocommit single-statement INSERT 的 pessimistic path**。

因此每个 sysbench 连接在 `thread_init()` 中需要显式设置：

- `SET SESSION tidb_txn_mode='pessimistic'` 或 `optimistic`
- `SET SESSION tidb_dml_type='standard'`

其中第二项是必须的，因为 `tidb_dml_type=bulk` 会绕开 `pessimistic-auto-commit` 路径。

P1 用例需确认以下条件成立，不满足则跳过并告警：

1. TiDB 配置 `pessimistic-auto-commit = true`
2. sysbench 连接处于 autocommit 模式
3. 当前 workload 没有显式 `BEGIN` / `COMMIT` 包裹单条 INSERT

只有在这些条件成立时，P1 才代表 autocommit single-statement INSERT 的 pessimistic path。

### 单轮校验

每个用例结束后自动执行：

1. `sysbench errors == 0 && reconnects == 0`
2. `SELECT count(*) FROM bc_bet_records` == `sysbench total_events × batch_size`
3. （仅 mlog 场景）mlog 行数 == 基表行数

任一检查失败标记 `VALIDATE FAIL`，需定位问题后重跑。

## 结果分析（`analyze_results.py`）

从 sysbench 输出提取 QPS、Avg Latency、P99 Latency、Max Latency。从 `--report-interval` 行提取时间序列（丢弃前 60s warmup）。

统一使用 **rows/s**（= TPS × batch_size）作为吞吐指标，便于跨 batch 对比。OH% = `(mlog - baseline) / baseline × 100`（负数表示性能下降）。

对 report-interval 数据计算变异系数（CV），标记 CV > 10% 的用例为不稳定。

**输出示例：**

```
Batch | TxnMode    | RowRate | Baseline rows/s | Mlog-NoShard rows/s | OH%    | Mlog-Shard rows/s | OH%
1     | optimistic | unlim   | 12345           | 11000               | -10.9% | 11200             | -9.3%
1     | optimistic | 5000    | 5000            | -                   | -      | 4960              | -0.8%
10    | optimistic | unlim   | 98000           | -                   | -      | 94000             | -4.1%
```

### 分析维度

1. **横向对比**：baseline vs mlog，QPS 下降百分比和延迟上升百分比
2. **纵向对比**：不同 batch size 下 overhead 变化趋势
3. **Shard 效果**：shard vs noshard QPS 差异
4. **稳定性**：report-interval 时间序列波动

## 文件结构

```
bench/mlog-perf/
  base_table.sql      # 基表 DDL
  mlog.sql            # mlog DDL（含 shard 选项）
  mlog_insert.lua     # sysbench Lua 脚本
  run_bench.sh        # 测试编排与校验
  analyze_results.py  # 结果分析
```

## 关键代码引用

| 文件 | 用途 |
|------|------|
| `pkg/table/tables/mview_log.go:337` | mlog 写入核心路径 `writeMLogRow()` |
| `pkg/ddl/executor.go:1088` | mlog 表 schema 生成逻辑 |
| `pkg/planner/core/util.go:419` | `CheckMViewUpdatable()` 保护 mlog 表 |
| `pkg/config/config.go:836` | `PessimisticAutoCommit` 配置定义 |
