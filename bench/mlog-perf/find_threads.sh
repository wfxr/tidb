#!/usr/bin/env bash
# find_threads.sh — 递增并发找写入瓶颈（baseline 场景，无 mlog）
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"

# ---------- defaults (overridable via env) ----------
TIDB_HOST="${TIDB_HOST:-10.142.0.7}"
TIDB_PORT="${TIDB_PORT:-4000}"
TIDB_USER="${TIDB_USER:-root}"
TIDB_PASS="${TIDB_PASS:-}"
TIDB_DB="${TIDB_DB:-mlog_bench}"
THREAD_LIST=(32 64 128 256 512 1024)
TIME=60

# ---------- helpers ----------
mysql_cmd() {
  mysql -h"$TIDB_HOST" -P"$TIDB_PORT" -u"$TIDB_USER" ${TIDB_PASS:+-p"$TIDB_PASS"} "$@"
}

setup_database() {
  mysql_cmd -e "DROP DATABASE IF EXISTS \`$TIDB_DB\`; CREATE DATABASE \`$TIDB_DB\`;" 2>/dev/null
}

create_base_table() {
  {
    echo "SET SESSION tidb_scatter_region='table';"
    echo "SET SESSION tidb_wait_split_region_finish=1;"
    echo "SET SESSION tidb_wait_split_region_timeout=300;"
    cat "$SCRIPT_DIR/base_table.sql"
  } | mysql_cmd --comments "$TIDB_DB" 2>/dev/null
}

# ---------- main ----------
declare -a RESULTS=()

for t in "${THREAD_LIST[@]}"; do
  echo "=========================================="
  echo "Testing $t threads ..."
  echo "=========================================="

  # 1. Recreate schema
  setup_database
  create_base_table

  # 2. Run sysbench
  logfile="/tmp/ft_${t}.log"
  sysbench \
    --db-driver=mysql \
    --mysql-host="$TIDB_HOST" \
    --mysql-port="$TIDB_PORT" \
    --mysql-user="$TIDB_USER" \
    ${TIDB_PASS:+--mysql-password="$TIDB_PASS"} \
    --mysql-db="$TIDB_DB" \
    --threads="$t" \
    --time="$TIME" \
    --report-interval=10 \
    --percentile=99 \
    "$SCRIPT_DIR/mlog_insert.lua" \
    --batch_size=1 \
    --txn_mode=optimistic \
    run 2>&1 | tee "$logfile"

  # 3. Extract results
  tps=$(grep "transactions:" "$logfile" | grep -oP '[\d.]+(?=\s+per sec)')
  p99=$(grep "99th percentile:" "$logfile" | awk '{print $NF}')
  RESULTS+=("$(printf "%7d | %10s | %s" "$t" "$tps" "$p99")")

  echo ""
  echo ">>> $t threads: $tps TPS, P99 ${p99}ms"
  echo ""
done

# ---------- summary ----------
echo ""
echo "=========================================="
echo " Summary"
echo "=========================================="
printf "Threads |        TPS | P99(ms)\n"
printf "--------|------------|--------\n"
for line in "${RESULTS[@]}"; do
  echo "$line"
done
