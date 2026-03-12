# Mlog Write Overhead Benchmark — Implementation Plan

## Context

评估物化视图日志（mlog）对基表 INSERT 性能的影响。基表每次 INSERT 在同一事务内额外写入一行 mlog，需量化吞吐量下降和延迟上升。设计文档已完成（`mlog-bench-design.md`），基表 DDL（`base_table.sql`）和 mlog DDL（`mlog.sql`）已就绪，需实现三个缺失文件：Lua 脚本、编排脚本、分析脚本。

## Files to Create

```
bench/mlog-perf/
  base_table.sql       ✅ 已存在
  mlog.sql             ✅ 已存在
  mlog_insert.lua      ❌ 待实现
  run_bench.sh         ❌ 待实现
  analyze_results.py   ❌ 待实现
```

---

## 1. `mlog_insert.lua` — sysbench 自定义 Lua 脚本

### 1.1 全局参数注册

**必须**用 `sysbench.cmdline.options` 注册自定义参数，否则 sysbench 不会识别 `--batch_size` 等选项：

```lua
sysbench.cmdline.options = {
  batch_size = {"Rows per INSERT statement", 1},
  txn_mode = {"Transaction mode: optimistic or pessimistic", "optimistic"},
  table_name = {"Target table name", "bc_bet_records"},
}
```

### 1.2 `thread_init()`

```lua
function thread_init()
  drv = sysbench.sql.driver()
  con = drv:connect()

  -- 设置事务模式
  con:query("SET SESSION tidb_txn_mode='" .. sysbench.opt.txn_mode .. "'")
  con:query("SET SESSION tidb_dml_type='standard'")

  -- 线程级计数器，用于生成唯一键
  tid = sysbench.tid + 1  -- 1-based thread id
  counter = 0
end
```

### 1.3 唯一键生成

- **`id`**: `tid * 281474976710656 + counter`（即 `tid << 48`，Lua double 精度 < 2^53，safe）
- **`order_no`**: `string.format("ORD-%d-%d", tid, counter)`
- **`record_id`**: `string.format("REC-%d-%d", tid, counter)`

每次 `event()` 调用中，对 batch 内每行递增 counter。32 线程 × 660s × 50k QPS ≈ 10^9 << 2^48，不会溢出。

### 1.4 数据分布函数

```lua
local function rand_account()    return string.format("user_%07d", sysbench.rand.uniform(1, 1000000)) end
local function rand_site_code()  return string.format("SITE_%02d", sysbench.rand.uniform(1, 50)) end
local function rand_platform()   return sysbench.rand.uniform(1, 20) end
local function rand_category()   return sysbench.rand.uniform(1, 20) end
local function rand_game_id()    return sysbench.rand.uniform(1, 2000) end
local function rand_currency()
  local currencies = {"CNY", "USD", "EUR", "GBP", "JPY"}
  return currencies[sysbench.rand.uniform(1, 5)]
end
local function rand_settle_status()
  local r = sysbench.rand.uniform(1, 100)
  if r <= 80 then return 2      -- 已结算 80%
  elseif r <= 99 then return 1  -- 未结算 19%
  else return 3 end             -- 撤销 1%
end
local function rand_decimal(max)
  return sysbench.rand.uniform(0, max * 100) / 100.0
end
local function rand_datetime_90d()
  local now = os.time()
  local offset = sysbench.rand.uniform(0, 90 * 86400)
  return os.date("%Y-%m-%d %H:%M:%S", now - offset)
end
```

### 1.5 `event()` — 构建 INSERT 语句

显式列出全部 **40 个可插入列**（排除 `settle_day` GENERATED STORED 列）。

已校验：40 列 ↔ 37 个 format 占位符 + 3 个字面量（NOW(), NOW(), 'mobile', 'EU', 'active', 'football', 0）→ 40 个 VALUES 位置全部对齐。

```lua
function event()
  local batch = sysbench.opt.batch_size
  local cols = "id, record_id, order_no, round_id, platform_id, category_id, " ..
               "site_code, site_prefix, agent_code, account, third_user_name, " ..
               "pull_time, third_game_code, all_bet, valid_bet, net_profit, " ..
               "rake, jackpot, bet_time, bet_time_stamp, settle_time, " ..
               "settle_time_stamp, settle_status, device, bet_ip, " ..
               "third_group_code, after_balance, is_combo, odds_type, odds, " ..
               "order_status, sports_type, winlost_time, game_id, currency, " ..
               "settle_time_zone, settle_date, version_no, tax_rate, tax"

  local values_list = {}
  for i = 1, batch do
    counter = counter + 1
    local id = tid * 281474976710656 + counter  -- tid << 48
    local account = rand_account()
    local site_code = rand_site_code()
    local platform_id = rand_platform()
    local category_id = rand_category()
    local game_id = rand_game_id()
    local currency = rand_currency()
    local settle_status = rand_settle_status()
    local settle_tz = rand_datetime_90d()
    local settle_date = string.sub(settle_tz, 1, 10)
    local bet_time = rand_datetime_90d()
    local now_ts = os.time()

    local vals = string.format(
      "(%d, 'REC-%d-%d', 'ORD-%d-%d', 'RND-%d-%d', %d, %d, " ..
      "'%s', 'PRE_%s', 'AGT_%s', '%s', 'TU_%s', " ..
      "NOW(), 'GC_%d', %.2f, %.2f, %.2f, " ..
      "%.2f, %.2f, '%s', %d, '%s', " ..
      "%d, %d, 'mobile', '10.0.%d.%d', " ..
      "'GRP_%d', %.2f, %d, 'EU', %.2f, " ..
      "'active', 'football', NOW(), %d, '%s', " ..
      "'%s', '%s', 0, %.4f, %.4f)",
      id, tid, counter, tid, counter, tid, counter,
      platform_id, category_id,
      site_code, site_code, site_code, account, account,
      game_id,
      rand_decimal(10000), rand_decimal(10000), rand_decimal(5000),
      rand_decimal(1000), rand_decimal(5000),
      bet_time, now_ts, settle_tz,
      now_ts, settle_status,
      sysbench.rand.uniform(0,255), sysbench.rand.uniform(0,255),
      sysbench.rand.uniform(1, 100),
      rand_decimal(50000), sysbench.rand.uniform(0, 1), rand_decimal(10),
      game_id, currency,
      settle_tz, settle_date,
      rand_decimal(0.1), rand_decimal(100)
    )
    values_list[i] = vals
  end

  local sql = "INSERT INTO " .. sysbench.opt.table_name ..
              " (" .. cols .. ") VALUES " .. table.concat(values_list, ", ")
  con:query(sql)
end
```

**列-值对齐验证（40 列）：**

| #  | Column           | Value                              |
|----|------------------|------------------------------------|
| 1  | id               | `%d` → id                          |
| 2  | record_id        | `'REC-%d-%d'` → tid, counter       |
| 3  | order_no         | `'ORD-%d-%d'` → tid, counter       |
| 4  | round_id         | `'RND-%d-%d'` → tid, counter       |
| 5  | platform_id      | `%d` → platform_id                 |
| 6  | category_id      | `%d` → category_id                 |
| 7  | site_code        | `'%s'` → site_code                 |
| 8  | site_prefix      | `'PRE_%s'` → site_code             |
| 9  | agent_code       | `'AGT_%s'` → site_code             |
| 10 | account          | `'%s'` → account                   |
| 11 | third_user_name  | `'TU_%s'` → account                |
| 12 | pull_time        | `NOW()` 字面量                     |
| 13 | third_game_code  | `'GC_%d'` → game_id                |
| 14 | all_bet          | `%.2f` → rand_decimal(10000)       |
| 15 | valid_bet        | `%.2f` → rand_decimal(10000)       |
| 16 | net_profit       | `%.2f` → rand_decimal(5000)        |
| 17 | rake             | `%.2f` → rand_decimal(1000)        |
| 18 | jackpot          | `%.2f` → rand_decimal(5000)        |
| 19 | bet_time         | `'%s'` → bet_time                  |
| 20 | bet_time_stamp   | `%d` → now_ts                      |
| 21 | settle_time      | `'%s'` → settle_tz                 |
| 22 | settle_time_stamp| `%d` → now_ts                      |
| 23 | settle_status    | `%d` → settle_status               |
| 24 | device           | `'mobile'` 字面量                  |
| 25 | bet_ip           | `'10.0.%d.%d'` → rand, rand        |
| 26 | third_group_code | `'GRP_%d'` → rand(1,100)           |
| 27 | after_balance    | `%.2f` → rand_decimal(50000)       |
| 28 | is_combo         | `%d` → rand(0,1)                   |
| 29 | odds_type        | `'EU'` 字面量                      |
| 30 | odds             | `%.2f` → rand_decimal(10)          |
| 31 | order_status     | `'active'` 字面量                  |
| 32 | sports_type      | `'football'` 字面量                |
| 33 | winlost_time     | `NOW()` 字面量                     |
| 34 | game_id          | `%d` → game_id                     |
| 35 | currency         | `'%s'` → currency                  |
| 36 | settle_time_zone | `'%s'` → settle_tz                 |
| 37 | settle_date      | `'%s'` → settle_date               |
| 38 | version_no       | `0` 字面量                         |
| 39 | tax_rate         | `%.4f` → rand_decimal(0.1)         |
| 40 | tax              | `%.4f` → rand_decimal(100)         |

Format 占位符 37 个，参数 37 个 ✅。字面量 6 处（NOW×2, mobile, EU, active, football, 0）✅。

### 1.6 `thread_done()`

```lua
function thread_done()
  con:disconnect()
end
```

### 1.7 关键约束

- **不需要** `prepare()` / `cleanup()`，schema 管理由 `run_bench.sh` 负责
- 每个 event 只执行一条 SQL（autocommit），**不**显式 `BEGIN`/`COMMIT`
- `os.time()` / `os.date()` 在每行调用有 syscall 开销，但 baseline 和 mlog 场景同等开销，不影响相对对比

---

## 2. `run_bench.sh` — 测试编排与校验

### 2.1 总体结构

```
run_bench.sh [--host HOST] [--port PORT] [--user USER] [--password PASS]
             [--db DB] [--threads 32] [--time 660] [--priority p0|p1|p2|all]
             [--target-row-rate RATE] [--output-dir DIR]
```

### 2.2 脚本头部

```bash
#!/usr/bin/env bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
```

### 2.3 配置与参数解析

```bash
# 默认值（支持环境变量覆盖）
TIDB_HOST="${TIDB_HOST:-127.0.0.1}"
TIDB_PORT="${TIDB_PORT:-4000}"
TIDB_USER="${TIDB_USER:-root}"
TIDB_PASS="${TIDB_PASS:-}"
TIDB_DB="${TIDB_DB:-mlog_bench}"
THREADS=32
TIME=660
REPORT_INTERVAL=10
PERCENTILE=99
PRIORITY="P0"
TARGET_ROW_RATE=""
OUTPUT_DIR=""

parse_args() {
  while [[ $# -gt 0 ]]; do
    case "$1" in
      --host) TIDB_HOST="$2"; shift 2 ;;
      --port) TIDB_PORT="$2"; shift 2 ;;
      --user) TIDB_USER="$2"; shift 2 ;;
      --password) TIDB_PASS="$2"; shift 2 ;;
      --db) TIDB_DB="$2"; shift 2 ;;
      --threads) THREADS="$2"; shift 2 ;;
      --time) TIME="$2"; shift 2 ;;
      --priority) PRIORITY=$(echo "$2" | tr '[:lower:]' '[:upper:]'); shift 2 ;;
      --target-row-rate) TARGET_ROW_RATE="$2"; shift 2 ;;
      --output-dir) OUTPUT_DIR="$2"; shift 2 ;;
      *) echo "Unknown option: $1"; exit 1 ;;
    esac
  done
  OUTPUT_DIR="${OUTPUT_DIR:-$SCRIPT_DIR/results/$(date -u +%Y%m%dT%H%M%SZ)}"
}
```

### 2.4 测试矩阵

```bash
# 格式: "case_id:scenario:mlog_shard:batch_size:txn_mode:rate:priority"
CASES=(
  "1:baseline:-:1:optimistic:0:P0"
  "2:mlog:shard:1:optimistic:0:P0"
  "3:mlog:noshard:1:optimistic:0:P0"
  "4:baseline:-:10:optimistic:0:P0"
  "5:mlog:shard:10:optimistic:0:P0"
  "6:baseline:-:1:pessimistic:0:P1"
  "7:mlog:shard:1:pessimistic:0:P1"
  "8:baseline:-:1:optimistic:RATE_X:P2"
  "9:mlog:shard:1:optimistic:RATE_X:P2"
)
```

### 2.5 辅助函数

#### `mysql_query(sql)` — 执行 SQL，返回纯数据（-sN）

```bash
mysql_query() {
  mysql -h"$TIDB_HOST" -P"$TIDB_PORT" -u"$TIDB_USER" ${TIDB_PASS:+-p"$TIDB_PASS"} \
    -sN -e "$1" "$TIDB_DB" 2>/dev/null
}
```

#### `mysql_exec(sql)` — 执行 SQL，保留格式输出

```bash
mysql_exec() {
  mysql -h"$TIDB_HOST" -P"$TIDB_PORT" -u"$TIDB_USER" ${TIDB_PASS:+-p"$TIDB_PASS"} \
    -e "$1" "$TIDB_DB" 2>/dev/null
}
```

> **注意**：stderr 必须重定向到 `/dev/null` 而非 `2>&1`。mariadb 兼容层会在 stderr 输出 deprecation warning，`2>&1` 会将其混入 stdout 导致算术比较失败（冒烟测试中发现并修复）。

#### `setup_database()` — 重建数据库

```bash
setup_database() {
  mysql -h"$TIDB_HOST" -P"$TIDB_PORT" -u"$TIDB_USER" ${TIDB_PASS:+-p"$TIDB_PASS"} \
    -e "DROP DATABASE IF EXISTS \`$TIDB_DB\`; CREATE DATABASE \`$TIDB_DB\`;" 2>/dev/null
}
```

#### `create_schema(scenario, mlog_shard)` — 建表（session 变量 + DDL 在同一连接）

```bash
create_schema() {
  local scenario=$1
  local mlog_shard=$2

  # 基表：session 变量 + DDL 必须在同一 mysql 连接中
  # tidb_wait_split_region_finish=1 确保 PRE_SPLIT_REGIONS 完成后才返回
  {
    echo "SET SESSION tidb_scatter_region='table';"
    echo "SET SESSION tidb_wait_split_region_finish=1;"
    echo "SET SESSION tidb_wait_split_region_timeout=300;"
    cat "$SCRIPT_DIR/base_table.sql"
  } | mysql -h"$TIDB_HOST" -P"$TIDB_PORT" -u"$TIDB_USER" ${TIDB_PASS:+-p"$TIDB_PASS"} \
      --comments "$TIDB_DB" 2>/dev/null

  # mlog：shard 时用 --comments 保留 /*T! SHARD... */ 注释
  #        noshard 时用 --skip-comments 剥离
  if [[ "$scenario" == "mlog" ]]; then
    local comments_flag="--comments"
    [[ "$mlog_shard" == "noshard" ]] && comments_flag="--skip-comments"
    {
      echo "SET SESSION tidb_scatter_region='table';"
      echo "SET SESSION tidb_wait_split_region_finish=1;"
      echo "SET SESSION tidb_wait_split_region_timeout=300;"
      cat "$SCRIPT_DIR/mlog.sql"
    } | mysql -h"$TIDB_HOST" -P"$TIDB_PORT" -u"$TIDB_USER" ${TIDB_PASS:+-p"$TIDB_PASS"} \
        $comments_flag "$TIDB_DB" 2>/dev/null
  fi
}
```

**注意**：mlog 建表也需要 `tidb_wait_split_region_finish=1`，否则 shard 场景的 region split 可能还未完成就进入压测。

#### `verify_regions(scenario, mlog_shard)` — Region 校验

用 `SHOW TABLE ... REGIONS` + `tail | wc -l` 计数：

```bash
verify_regions() {
  local scenario=$1
  local mlog_shard=$2

  # 基表 region 校验
  # SHARD_ROW_ID_BITS=4 PRE_SPLIT_REGIONS=3 → 行数据 2^3=8 regions
  local base_regions
  base_regions=$(mysql_exec "SHOW TABLE bc_bet_records REGIONS;" | tail -n +2 | wc -l)
  echo "[VERIFY] Base table regions: $base_regions"
  if (( base_regions < 8 )); then
    echo "[VERIFY FAIL] Base table regions too few: $base_regions (expect >= 8)"
    return 1
  fi

  if [[ "$scenario" == "mlog" ]]; then
    local mlog_regions
    mlog_regions=$(mysql_exec 'SHOW TABLE `$mlog$bc_bet_records` REGIONS;' | tail -n +2 | wc -l)
    echo "[VERIFY] Mlog regions: $mlog_regions"
    if [[ "$mlog_shard" == "shard" ]] && (( mlog_regions < 4 )); then
      echo "[VERIFY FAIL] Mlog shard regions too few: $mlog_regions (expect >= 4)"
      return 1
    fi
    if [[ "$mlog_shard" == "noshard" ]] && (( mlog_regions != 1 )); then
      echo "[VERIFY WARN] Mlog noshard expected 1 region, got: $mlog_regions"
    fi
  fi
  return 0
}
```

#### `verify_pessimistic_config()` — P1 前置校验

```bash
verify_pessimistic_config() {
  local pac
  pac=$(mysql_query "SHOW CONFIG WHERE name='pessimistic-txn.pessimistic-auto-commit';" 2>/dev/null \
        | awk '{print $NF}') || true
  if [[ "$pac" != "true" ]]; then
    echo "[WARN] pessimistic-auto-commit is not enabled."
    return 1
  fi
  return 0
}
```

#### `run_sysbench(case_id, batch_size, txn_mode, rate)` — 执行 sysbench

```bash
run_sysbench() {
  local case_id=$1 batch_size=$2 txn_mode=$3 rate=$4
  local outfile="$OUTPUT_DIR/case_${case_id}.log"
  local rate_opt=""
  if (( rate > 0 )); then
    local events_rate=$(( rate / batch_size ))
    rate_opt="--rate=$events_rate"
  fi

  local start_ts
  start_ts=$(date -u +%FT%TZ)
  echo "[${start_ts}] Starting case #${case_id} ..."

  sysbench \
    --db-driver=mysql \
    --mysql-host="$TIDB_HOST" \
    --mysql-port="$TIDB_PORT" \
    --mysql-user="$TIDB_USER" \
    ${TIDB_PASS:+--mysql-password="$TIDB_PASS"} \
    --mysql-db="$TIDB_DB" \
    --threads="$THREADS" \
    --time="$TIME" \
    --report-interval="$REPORT_INTERVAL" \
    --percentile="$PERCENTILE" \
    $rate_opt \
    "$SCRIPT_DIR/mlog_insert.lua" \
    --batch_size="$batch_size" \
    --txn_mode="$txn_mode" \
    run 2>&1 | tee "$outfile"

  local end_ts
  end_ts=$(date -u +%FT%TZ)
  echo "$start_ts" > "$OUTPUT_DIR/case_${case_id}.start_ts"
  echo "$end_ts" > "$OUTPUT_DIR/case_${case_id}.end_ts"
}
```

#### `validate_case(case_id, scenario, batch_size)` — 单轮校验

```bash
validate_case() {
  local case_id=$1 scenario=$2 batch_size=$3
  local outfile="$OUTPUT_DIR/case_${case_id}.log"

  # 1) sysbench errors/reconnects == 0
  local errors reconnects
  errors=$(grep -oP 'ignored errors:\s+\K\d+' "$outfile" | tail -1)
  reconnects=$(grep -oP 'reconnects:\s+\K\d+' "$outfile" | tail -1)
  if (( errors != 0 || reconnects != 0 )); then
    echo "[VALIDATE FAIL] Case #$case_id: errors=$errors reconnects=$reconnects"
    return 1
  fi

  # 2) 行数校验
  local total_events
  total_events=$(grep -oP 'total number of events:\s+\K\d+' "$outfile")
  local expected_rows=$(( total_events * batch_size ))
  local actual_rows
  actual_rows=$(mysql_query "SELECT count(*) FROM bc_bet_records;")
  if (( actual_rows != expected_rows )); then
    echo "[VALIDATE FAIL] Case #$case_id: expected $expected_rows rows, got $actual_rows"
    return 1
  fi

  # 3) mlog 行数校验
  if [[ "$scenario" == "mlog" ]]; then
    local mlog_rows
    mlog_rows=$(mysql_query 'SELECT count(*) FROM `$mlog$bc_bet_records`;')
    if (( mlog_rows != actual_rows )); then
      echo "[VALIDATE FAIL] Case #$case_id: mlog rows $mlog_rows != base rows $actual_rows"
      return 1
    fi
  fi

  echo "[VALIDATE OK] Case #$case_id: $actual_rows rows"
  return 0
}
```

**注意**：mlog 表名 `$mlog$bc_bet_records` 在 bash 中需要用**单引号**包裹 SQL 以避免 `$` 被 shell 展开，在 SQL 中用反引号转义。

### 2.6 Metadata 输出

`run_bench.sh` 在每个用例运行前写入 `.meta` 文件，供 `analyze_results.py` 使用：

```bash
write_metadata() {
  local case_id=$1 scenario=$2 mlog_shard=$3 batch_size=$4 txn_mode=$5 rate=$6
  local meta_file="$OUTPUT_DIR/case_${case_id}.meta"
  cat > "$meta_file" <<EOF
scenario=$scenario
mlog_shard=$mlog_shard
batch_size=$batch_size
txn_mode=$txn_mode
rate=$rate
EOF
}
```

用 key=value 格式代替 JSON，避免依赖 jq。

### 2.7 主循环

```bash
main() {
  parse_args "$@"
  mkdir -p "$OUTPUT_DIR"

  cp "$SCRIPT_DIR/base_table.sql" "$SCRIPT_DIR/mlog.sql" "$SCRIPT_DIR/mlog_insert.lua" "$OUTPUT_DIR/"

  for case_def in "${CASES[@]}"; do
    IFS=':' read -r case_id scenario mlog_shard batch_size txn_mode rate priority <<< "$case_def"

    # 过滤优先级
    if [[ "$PRIORITY" != "ALL" ]] && [[ "$priority" > "$PRIORITY" ]]; then
      echo "[SKIP] Case #$case_id ($priority > $PRIORITY)"
      continue
    fi

    # P2 rate 替换
    if [[ "$rate" == "RATE_X" ]]; then
      if [[ -z "$TARGET_ROW_RATE" ]]; then
        echo "[SKIP] Case #$case_id: --target-row-rate not set, skipping P2"
        continue
      fi
      rate="$TARGET_ROW_RATE"
    fi

    # P1 前置检查
    if [[ "$txn_mode" == "pessimistic" ]]; then
      if ! verify_pessimistic_config; then
        echo "[SKIP] Case #$case_id: pessimistic-auto-commit not enabled"
        continue
      fi
    fi

    echo "=========================================="
    echo "Case #$case_id: $scenario / shard=$mlog_shard / batch=$batch_size / txn=$txn_mode / rate=$rate"
    echo "=========================================="

    write_metadata "$case_id" "$scenario" "$mlog_shard" "$batch_size" "$txn_mode" "$rate"
    setup_database
    if ! create_schema "$scenario" "$mlog_shard"; then
      echo "[PREPARE FAIL] Case #$case_id: schema creation failed"
      continue
    fi

    # Region 校验（失败重试一次）
    if ! verify_regions "$scenario" "$mlog_shard"; then
      echo "[PREPARE FAIL] Case #$case_id: region verify failed, rebuilding..."
      setup_database
      create_schema "$scenario" "$mlog_shard"
      if ! verify_regions "$scenario" "$mlog_shard"; then
        echo "[PREPARE FAIL] Case #$case_id: region verify failed again, skipping"
        continue
      fi
    fi

    run_sysbench "$case_id" "$batch_size" "$txn_mode" "$rate"
    validate_case "$case_id" "$scenario" "$batch_size" || true

    echo ""
  done

  echo "[DONE] Results saved to $OUTPUT_DIR"
  echo "Run: python3 $SCRIPT_DIR/analyze_results.py $OUTPUT_DIR"
}

main "$@"
```

### 2.8 时间估算

每个用例 660s + 建表/校验约 60s ≈ 12min。P0 共 5 个用例 ≈ 60min，全部 9 个 ≈ 108min。

---

## 3. `analyze_results.py` — 结果分析脚本

### 3.1 输入

```
python3 analyze_results.py <results_dir>
```

读取 `results_dir/case_*.log` 和 `results_dir/case_*.meta`。

### 3.2 Metadata 读取

```python
def load_case_meta(results_dir, case_id):
    meta = {}
    with open(os.path.join(results_dir, f"case_{case_id}.meta")) as f:
        for line in f:
            k, v = line.strip().split("=", 1)
            meta[k] = v
    meta["batch_size"] = int(meta["batch_size"])
    meta["rate"] = int(meta["rate"]) if meta["rate"] != "0" else 0
    return meta
```

### 3.3 Sysbench 输出解析

#### 最终统计提取

```python
def parse_summary(log_text):
    result = {}
    m = re.search(r'transactions:\s+\d+\s+\(([\d.]+) per sec\.\)', log_text)
    if m: result['tps'] = float(m.group(1))
    m = re.search(r'avg:\s+([\d.]+)', log_text)
    if m: result['avg_lat'] = float(m.group(1))
    m = re.search(r'99th percentile:\s+([\d.]+)', log_text)
    if m: result['p99_lat'] = float(m.group(1))
    m = re.search(r'max:\s+([\d.]+)', log_text)
    if m: result['max_lat'] = float(m.group(1))
    return result
```

#### Report-interval 时间序列提取

```python
def parse_timeseries(log_text, warmup_secs=60):
    pattern = r'\[\s*(\d+)s\s*\].*tps:\s*([\d.]+).*lat \(ms,99%\):\s*([\d.]+)'
    points = []
    for m in re.finditer(pattern, log_text):
        ts, tps, lat99 = int(m.group(1)), float(m.group(2)), float(m.group(3))
        if ts > warmup_secs:
            points.append((ts, tps, lat99))
    return points
```

### 3.4 指标计算

```python
rows_per_sec = summary['tps'] * meta['batch_size']
oh_pct = (mlog_rows_s - baseline_rows_s) / baseline_rows_s * 100  # 负数 = 性能下降

# 时间序列稳定性 (CV = std/mean)
tps_values = [p[1] for p in timeseries]
cv = statistics.stdev(tps_values) / statistics.mean(tps_values) if len(tps_values) > 1 else 0
stable = cv < 0.10
```

### 3.5 Baseline 匹配逻辑

按 `(batch_size, txn_mode, rate_category)` 分组匹配 baseline 和 mlog：

| 分组键 | Baseline Case | Mlog Cases |
|--------|---------------|------------|
| (1, optimistic, unlim) | #1 | #2 (shard), #3 (noshard) |
| (10, optimistic, unlim) | #4 | #5 (shard) |
| (1, pessimistic, unlim) | #6 | #7 (shard) |
| (1, optimistic, rate-X) | #8 | #9 (shard) |

```python
def group_key(meta):
    rate_cat = "unlim" if meta['rate'] == 0 else f"rate-{meta['rate']}"
    return (meta['batch_size'], meta['txn_mode'], rate_cat)
```

### 3.6 输出

#### 汇总表（stdout）

```
Batch | TxnMode     | RowRate | Baseline rows/s | Mlog-NoShard rows/s | OH%    | Mlog-Shard rows/s | OH%    | CV
1     | optimistic  | unlim   | 12345           | 11000               | -10.9% | 11200             | -9.3%  | 3.2%
10    | optimistic  | unlim   | 98000           | -                   | -      | 94000             | -4.1%  | 2.8%
```

#### 延迟对比表

```
Case# | Scenario     | Avg Lat (ms) | P99 Lat (ms) | Max Lat (ms)
1     | baseline     | 25.67        | 56.78        | 456.78
2     | mlog-shard   | 27.12        | 60.34        | 489.12
```

#### 时间序列 CSV

每个 case 输出 `case_N_timeseries.csv`：`timestamp,tps,rows_s,lat_99`

#### 汇总 CSV

输出 `summary.csv` 包含所有 case 的统计数据和 overhead 计算，便于后续绘图。

### 3.7 实现要点

- 仅用标准库（`re`, `os`, `sys`, `statistics`, `csv`），无第三方依赖
- 每个 case 标记状态：PASS / WARN（CV > 10%）/ MISSING

---

## 4. 实现顺序

1. **`mlog_insert.lua`** — 核心 Lua 脚本
2. **`run_bench.sh`** — 编排脚本
3. **`analyze_results.py`** — 分析脚本

---

## 5. Verification

### 本地验证（无需集群）

1. `sysbench "$SCRIPT_DIR/mlog_insert.lua" help` — 应打印 batch_size / txn_mode / table_name 参数
2. `bash -n run_bench.sh` — shell 语法检查
3. 构造 mock sysbench 输出文件 + meta 文件，运行 `python3 analyze_results.py mock_dir/`，验证 overhead 计算

### 集群验证

1. 快速跑（`--time=10 --threads=2`）验证 INSERT 成功、行数正确
2. 验证 mlog 场景 `$mlog$bc_bet_records` 行数 == 基表行数
3. 验证 batch=10 场景每 event 插入 10 行
4. 验证 `--skip-comments` 剥离了 SHARD 选项：`SHOW CREATE TABLE \`$mlog$bc_bet_records\``
5. 完整 P0 跑一轮，用 `analyze_results.py` 生成报告
