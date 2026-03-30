#!/usr/bin/env bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"

# ---------- defaults (overridable via env or flags) ----------
TIDB_HOST="${TIDB_HOST:-}"
TIDB_PORT="${TIDB_PORT:-4000}"
TIDB_USER="${TIDB_USER:-root}"
TIDB_PASS="${TIDB_PASS:-}"
TIDB_DB="${TIDB_DB:-mlog_bench}"
THREADS=128
TIME=660
REPORT_INTERVAL=10
PERCENTILE=99
TARGET_ROW_RATE=""
OUTPUT_DIR=""
CLUSTER="${CLUSTER:-bench-mlog}"
CASE_FILTER=""   # comma-separated case IDs to run (empty = all)
DRY_RUN=false

# ---------- test matrix ----------
# format: "case_id:scenario:mlog_shard:batch_size:txn_mode:rate:priority"
CASES=(
  # P0: core comparisons
  "1:baseline:-:1:optimistic:0:P0"
  "2:mlog:shard:1:optimistic:0:P0"
  "3:mlog:noshard:1:optimistic:0:P0"
  "4:baseline:-:10:optimistic:0:P0"
  "5:mlog:shard:10:optimistic:0:P0"
  # P1: pessimistic (requires pessimistic-auto-commit=true)
  "6:baseline:-:1:pessimistic:0:P1"
  "7:mlog:shard:1:pessimistic:0:P1"
  # P2: rate-limited (requires --target-row-rate)
  "8:baseline:-:1:optimistic:RATE_X:P2"
  "9:mlog:shard:1:optimistic:RATE_X:P2"
)

# ---------- argument parsing ----------
parse_args() {
  while [[ $# -gt 0 ]]; do
    case "$1" in
      --host)            TIDB_HOST="$2";    shift 2 ;;
      --port)            TIDB_PORT="$2";    shift 2 ;;
      --user)            TIDB_USER="$2";    shift 2 ;;
      --password)        TIDB_PASS="$2";    shift 2 ;;
      --db)              TIDB_DB="$2";      shift 2 ;;
      --threads)         THREADS="$2";      shift 2 ;;
      --time)            TIME="$2";         shift 2 ;;
      --target-row-rate) TARGET_ROW_RATE="$2"; shift 2 ;;
      --output-dir)      OUTPUT_DIR="$2";   shift 2 ;;
      --cluster)         CLUSTER="$2";      shift 2 ;;
      --cases)           CASE_FILTER="$2";  shift 2 ;;
      --dry-run)         DRY_RUN=true;      shift ;;
      *) echo "Unknown option: $1"; exit 1 ;;
    esac
  done
  OUTPUT_DIR="${OUTPUT_DIR:-$SCRIPT_DIR/results/$(date -u +%Y%m%dT%H%M%SZ)}"
}

# ---------- helpers ----------

# TIDB_ADMIN_HOST: set in main() after parse_args — first host for admin SQL
TIDB_ADMIN_HOST=""

# Execute SQL, return raw data (silent, no headers)
mysql_query() {
  mysql -h"$TIDB_ADMIN_HOST" -P"$TIDB_PORT" -u"$TIDB_USER" ${TIDB_PASS:+-p"$TIDB_PASS"} \
    -sN -e "$1" "$TIDB_DB" 2>/dev/null
}

# Execute SQL, keep formatted output
mysql_exec() {
  mysql -h"$TIDB_ADMIN_HOST" -P"$TIDB_PORT" -u"$TIDB_USER" ${TIDB_PASS:+-p"$TIDB_PASS"} \
    -e "$1" "$TIDB_DB" 2>/dev/null
}

setup_database() {
  mysql -h"$TIDB_ADMIN_HOST" -P"$TIDB_PORT" -u"$TIDB_USER" ${TIDB_PASS:+-p"$TIDB_PASS"} \
    -e "DROP DATABASE IF EXISTS \`$TIDB_DB\`; CREATE DATABASE \`$TIDB_DB\`;" 2>/dev/null
}

create_schema() {
  local scenario=$1
  local mlog_shard=$2

  # Base table: session variables + DDL must be in the same mysql connection
  {
    echo "SET SESSION tidb_scatter_region='table';"
    echo "SET SESSION tidb_wait_split_region_finish=1;"
    echo "SET SESSION tidb_wait_split_region_timeout=300;"
    cat "$SCRIPT_DIR/base_table.sql"
  } | mysql -h"$TIDB_ADMIN_HOST" -P"$TIDB_PORT" -u"$TIDB_USER" ${TIDB_PASS:+-p"$TIDB_PASS"} \
      --comments "$TIDB_DB" 2>/dev/null

  # Mlog table: --comments preserves /*T! SHARD... */ hints for shard mode;
  #             --skip-comments strips them for noshard mode
  if [[ "$scenario" == "mlog" ]]; then
    local comments_flag="--comments"
    [[ "$mlog_shard" == "noshard" ]] && comments_flag="--skip-comments"
    {
      echo "SET SESSION tidb_scatter_region='table';"
      echo "SET SESSION tidb_wait_split_region_finish=1;"
      echo "SET SESSION tidb_wait_split_region_timeout=300;"
      cat "$SCRIPT_DIR/mlog.sql"
    } | mysql -h"$TIDB_ADMIN_HOST" -P"$TIDB_PORT" -u"$TIDB_USER" ${TIDB_PASS:+-p"$TIDB_PASS"} \
        $comments_flag "$TIDB_DB" 2>/dev/null
  fi
}

verify_regions() {
  local scenario=$1
  local mlog_shard=$2

  # Base table: SHARD_ROW_ID_BITS=4 PRE_SPLIT_REGIONS=3 → 2^3=8 row-data regions minimum
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

verify_pessimistic_config() {
  local pac
  pac=$(mysql_query "SHOW CONFIG WHERE name='pessimistic-txn.pessimistic-auto-commit';" 2>/dev/null \
        | awk '{print $NF}' | head -1) || true
  if [[ "$pac" != "true" ]]; then
    echo "[WARN] pessimistic-auto-commit is not enabled. P1 results may not reflect pessimistic auto-commit path."
    return 1
  fi
  return 0
}

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

run_sysbench() {
  local case_id=$1 batch_size=$2 txn_mode=$3 rate=$4
  local outfile="$OUTPUT_DIR/case_${case_id}.log"
  local rate_opt=""
  if (( rate > 0 )); then
    # --rate limits events/sec; each event inserts batch_size rows
    local events_rate=$(( rate / batch_size ))
    rate_opt="--rate=$events_rate"
  fi

  echo "[$(date -u +%FT%TZ)] Starting case #${case_id} ..."

  if [[ "$METRICS_ENABLED" == "true" ]]; then
    python3 "$SCRIPT_DIR/collect_metrics.py" snapshot \
      --nodes "$NODES_FILE" --output-dir "$OUTPUT_DIR" \
      --case-id "$case_id" --label before
  fi

  local start_ts start_epoch
  start_ts=$(date -u +%FT%TZ)
  start_epoch=$(date +%s)

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

  local end_ts end_epoch elapsed
  end_ts=$(date -u +%FT%TZ)
  end_epoch=$(date +%s)
  elapsed=$((end_epoch - start_epoch))
  echo "$start_ts" > "$OUTPUT_DIR/case_${case_id}.start_ts"
  echo "$end_ts" > "$OUTPUT_DIR/case_${case_id}.end_ts"

  if [[ "$METRICS_ENABLED" == "true" ]]; then
    python3 "$SCRIPT_DIR/collect_metrics.py" snapshot \
      --nodes "$NODES_FILE" --output-dir "$OUTPUT_DIR" \
      --case-id "$case_id" --label after
    python3 "$SCRIPT_DIR/collect_metrics.py" compute \
      --nodes "$NODES_FILE" --output-dir "$OUTPUT_DIR" \
      --case-id "$case_id" --elapsed "$elapsed"
  fi
}

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

  # 2) Row count validation
  local total_events
  total_events=$(grep -oP 'total number of events:\s+\K\d+' "$outfile")
  local expected_rows=$(( total_events * batch_size ))
  local actual_rows
  actual_rows=$(mysql_query "SELECT count(*) FROM bc_bet_records;")
  if (( actual_rows != expected_rows )); then
    echo "[VALIDATE FAIL] Case #$case_id: expected $expected_rows rows, got $actual_rows"
    return 1
  fi

  # 3) Mlog row count must match base table
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

# ---------- main ----------
main() {
  parse_args "$@"

  # Auto-discover TiDB hosts from TiUP cluster if not specified
  if [[ -z "$TIDB_HOST" ]]; then
    TIDB_HOST=$(~/.tiup/bin/tiup cluster display "$CLUSTER" --format json 2>/dev/null \
      | python3 -c "
import sys, json
d = json.load(sys.stdin)
hosts = sorted(set(i['host'] for i in d.get('instances', []) if i.get('role', '').split()[0] == 'tidb'))
print(','.join(hosts))
") || true
    if [[ -z "$TIDB_HOST" ]]; then
      echo "ERROR: Cannot discover TiDB hosts. Use --host or ensure tiup cluster '$CLUSTER' exists." >&2
      exit 1
    fi
    echo "[DISCOVER] TiDB hosts: $TIDB_HOST"
  fi

  # First host for admin SQL (DDL, validation) — all TiDB nodes share state.
  # sysbench gets the full comma-separated list for round-robin distribution.
  TIDB_ADMIN_HOST="${TIDB_HOST%%,*}"

  # Pre-flight: check pessimistic-auto-commit once (only affects pessimistic cases)
  PESSIMISTIC_OK=false
  if verify_pessimistic_config; then
    PESSIMISTIC_OK=true
  fi

  METRICS_ENABLED=false
  if [[ "$DRY_RUN" != "true" ]]; then
    mkdir -p "$OUTPUT_DIR"

    # Discover cluster nodes for metrics collection
    NODES_FILE="$OUTPUT_DIR/nodes.json"
    METRICS_ENABLED=true
    if ! python3 "$SCRIPT_DIR/collect_metrics.py" discover \
         --cluster "$CLUSTER" --output "$NODES_FILE"; then
      echo "[WARN] Metrics collection disabled (cluster discovery failed)"
      METRICS_ENABLED=false
    fi
  fi

  if [[ "$DRY_RUN" == "true" ]]; then
    echo ""
    echo "[DRY-RUN] Configuration:"
    echo "  TiDB hosts:  $TIDB_HOST"
    echo "  Threads:     $THREADS"
    echo "  Time:        ${TIME}s"
    echo "  Cluster:     $CLUSTER"
    echo "  Metrics:     $METRICS_ENABLED"
    echo "  Pessimistic: $PESSIMISTIC_OK"
    echo ""
    echo "[DRY-RUN] Cases to run:"
  fi

  for case_def in "${CASES[@]}"; do
    IFS=':' read -r case_id scenario mlog_shard batch_size txn_mode rate priority <<< "$case_def"

    # Case filter (--cases 8,9)
    if [[ -n "$CASE_FILTER" ]] && [[ ! ",$CASE_FILTER," == *",$case_id,"* ]]; then
      continue
    fi

    # P2 rate substitution
    if [[ "$rate" == "RATE_X" ]]; then
      if [[ -z "$TARGET_ROW_RATE" ]]; then
        echo "[SKIP] Case #$case_id: --target-row-rate not set, skipping P2"
        continue
      fi
      rate="$TARGET_ROW_RATE"
    fi

    # P1 pessimistic pre-check
    if [[ "$txn_mode" == "pessimistic" ]] && [[ "$PESSIMISTIC_OK" != "true" ]]; then
      echo "[SKIP] Case #$case_id: pessimistic-auto-commit not enabled"
      continue
    fi

    if [[ "$DRY_RUN" == "true" ]]; then
      echo "  Case #$case_id: $scenario / shard=$mlog_shard / batch=$batch_size / txn=$txn_mode / rate=$rate"
      continue
    fi

    echo "=========================================="
    echo "Case #$case_id: $scenario / shard=$mlog_shard / batch=$batch_size / txn=$txn_mode / rate=$rate"
    echo "=========================================="

    write_metadata "$case_id" "$scenario" "$mlog_shard" "$batch_size" "$txn_mode" "$rate"

    # Snapshot config files into output dir (once, on first case)
    if [[ ! -f "$OUTPUT_DIR/base_table.sql" ]]; then
      cp "$SCRIPT_DIR/base_table.sql" "$SCRIPT_DIR/mlog.sql" "$SCRIPT_DIR/mlog_insert.lua" "$OUTPUT_DIR/"
    fi

    # Recreate database
    setup_database

    # Create schema
    if ! create_schema "$scenario" "$mlog_shard"; then
      echo "[PREPARE FAIL] Case #$case_id: schema creation failed"
      continue
    fi

    # Region verification (retry once on failure)
    if ! verify_regions "$scenario" "$mlog_shard"; then
      echo "[PREPARE FAIL] Case #$case_id: region verify failed, rebuilding..."
      setup_database
      create_schema "$scenario" "$mlog_shard"
      if ! verify_regions "$scenario" "$mlog_shard"; then
        echo "[PREPARE FAIL] Case #$case_id: region verify failed again, skipping"
        continue
      fi
    fi

    # Run sysbench
    run_sysbench "$case_id" "$batch_size" "$txn_mode" "$rate"

    # Post-run validation
    validate_case "$case_id" "$scenario" "$batch_size" || true

    echo ""
  done

  if [[ "$DRY_RUN" == "true" ]]; then
    echo ""
    echo "[DRY-RUN] Done. No benchmarks executed."
  else
    echo "[DONE] Results saved to $OUTPUT_DIR"
    echo "Run: python3 $SCRIPT_DIR/analyze_results.py $OUTPUT_DIR"
  fi
}

main "$@"
