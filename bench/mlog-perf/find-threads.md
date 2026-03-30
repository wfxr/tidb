# Plan: 寻找集群最优写入并发数

## Context

当前 benchmark 使用固定 32 线程，但集群配置（3×TiDB c4-standard-16 + 3×TiKV c4-highmem-16）的写入能力远不止如此。需要先通过递增并发找到吞吐量拐点，再用最优并发数跑正式 benchmark，结果才有意义。

## 方案

在压测机上写一个 `find_threads.sh` 脚本，对 baseline（无 mlog）场景做 6 档并发短测，找到 TPS 峰值对应的线程数。

### 并发梯度

32 → 64 → 128 → 256 → 512 → 1024

### 每轮流程

1. `DROP DATABASE IF EXISTS mlog_bench; CREATE DATABASE mlog_bench;`
2. 用与 `run_bench.sh` 相同的方式建基表（带 session 变量 + `base_table.sql`，使用 `--comments`）
3. 跑 60s sysbench（`mlog_insert.lua`, batch=1, optimistic）
4. 提取 TPS 和 P99 延迟

### 脚本逻辑

```bash
#!/usr/bin/env bash
# find_threads.sh — 递增并发找写入瓶颈
TIDB_HOST=${TIDB_HOST:-}  # auto-discovered from tiup cluster
TIDB_PORT=4000
TIDB_DB=mlog_bench
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
THREAD_LIST=(32 64 128 256 512 1024)
TIME=60

for t in "${THREAD_LIST[@]}"; do
  # 1. 重建 schema
  mysql ... -e "DROP DATABASE IF EXISTS ...; CREATE DATABASE ...;"
  { echo "SET SESSION tidb_scatter_region='table';";
    echo "SET SESSION tidb_wait_split_region_finish=1;";
    echo "SET SESSION tidb_wait_split_region_timeout=300;";
    cat base_table.sql; } | mysql --comments ...

  # 2. 跑 sysbench
  sysbench ... --threads=$t --time=$TIME mlog_insert.lua \
    --batch_size=1 --txn_mode=optimistic run 2>&1 | tee /tmp/ft_${t}.log

  # 3. 提取结果
  tps=$(grep "transactions:" ... | grep -oP '[\d.]+(?= per sec)')
  p99=$(grep "99th percentile:" ... | awk '{print $NF}')
  echo "$t threads: $tps TPS, P99 ${p99}ms"
done
```

### 关键文件

| 文件 | 用途 |
|------|------|
| `bench/mlog-perf/base_table.sql` | 基表 DDL（必须复用，保持 schema 一致） |
| `bench/mlog-perf/mlog_insert.lua` | sysbench Lua 脚本（已修复 tid<<32） |
| `bench/mlog-perf/find_threads.sh` | **新建** — 并发探测脚本 |

### 输出

脚本结束时打印汇总表：

```
Threads |    TPS | P99(ms)
--------|--------|--------
     32 |   3086 |   16.41
     64 |   xxxx |   xx.xx
    128 |   xxxx |   xx.xx
    256 |   xxxx |   xx.xx
    512 |   xxxx |   xx.xx
   1024 |   xxxx |   xx.xx
```

### 执行步骤

1. 在本地创建 `bench/mlog-perf/find_threads.sh`
2. SCP 上传到压测机 `~/bench/mlog-perf/`
3. SSH 执行，总耗时约 7 分钟（6 × 60s + schema 开销）
4. 根据结果确定最优 threads，更新 `run_bench.sh` 的 `THREADS` 默认值

### 判定标准

- **最优并发**：TPS 达到峰值且 P99 没有急剧飙升的那个档位
- 如果相邻档位 TPS 差异 <5%，取较低的那个（延迟更稳定）

## 结果 R1（2026-03-12，单 TiDB 节点）

sysbench 仅连接单个 TiDB 节点（`--mysql-host=<single-tidb-ip>`），负载严重倾斜。

集群：3×TiDB c4-standard-16 + 3×TiKV c4-highmem-16 + 1×PD c4-standard-8

| Threads |    TPS | P99(ms) | 备注 |
|--------:|-------:|--------:|------|
|      32 | 3070.4 |   16.41 | |
|      64 | 4363.5 |   25.74 | |
|     128 | 6019.9 |   41.10 | |
|     256 | 6583.2 |   77.19 | **峰值** |
|     512 | 6121.0 |  161.51 | TPS 下降 7%，P99 翻倍 |
|    1024 |      — |       — | sysbench 线程初始化失败 |

结论：128 线程最优，但由于单节点瓶颈，吞吐量被低估。

## 结果 R2（2026-03-13，三 TiDB 节点 round-robin）

sysbench 连接全部 3 个 TiDB 节点（`--mysql-host=<tidb-ip-0>,<tidb-ip-1>,<tidb-ip-2>`），连接按 round-robin 均匀分布。

集群：3×TiDB c4-standard-16 + 3×TiKV c4-highmem-16 + 1×PD c4-standard-8

| Threads |    TPS | P99(ms) | 备注 |
|--------:|-------:|--------:|------|
|      32 | 5600.2 |    7.84 | |
|      64 | 7603.5 |   12.98 | |
|      96 | 8001.3 |   18.61 | |
|   **128** | **8570.9** |   **22.69** | **峰值** |
|     160 | 8415.4 |   28.67 | TPS -1.8%，进入平台期 |
|     192 | 8430.0 |   34.33 | |
|     256 | 8294.4 |   48.34 | |
|     320 | 8410.4 |   59.99 | |
|     384 | 8307.4 |   71.83 | |
|     448 | 8410.8 |   81.48 | |
|     512 | 8421.3 |   90.78 | |

### 分析

- 三节点负载均衡效果显著：同样 128 线程，TPS 从 R1 的 6020 提升到 **8571（+42%）**；32 线程从 3070 提升到 5600（+82%）。
- TPS 在 32→128 近线性增长，128 之后进入平台期（128~512 范围内 TPS 波动仅 ±3%），说明瓶颈已从 TiDB 转移到 TiKV 侧。
- P99 延迟随线程数线性增长：128 线程 23ms → 512 线程 91ms（4x），增加并发只是堆积延迟。
- 128→160 TPS 差距仅 1.8%（噪声范围），但 P99 从 23ms 涨到 29ms（+26%）。

### 结论

**最优并发数：128 线程**（与 R1 一致）。128 是吞吐量到达平台期的起点，P99 延迟仍然很低（23ms）。超过 128 后 TPS 无增益，延迟线性增长。`run_bench.sh` 的 `THREADS=128` 默认值无需修改。
