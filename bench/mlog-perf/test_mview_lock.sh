#!/usr/bin/env bash
# test_mview_lock.sh — 验证 SELECT ... FOR UPDATE 对 mview/mlog 加锁是否阻塞 REFRESH，以及 refresh 结果是否正确
#
# 用法:
#   ./test_mview_lock.sh [TIDB_HOST] [TIDB_PORT]
#   默认连接 127.0.0.1:4000
#
# 前置条件:
#   - TiDB 集群已启动（需要含 TiFlash，因为 mview 依赖 TiFlash）
#   - TiDB 使用支持 mview 的 feature branch 二进制
#   - mysql CLI 可用
#
# 结论摘要:
#   1. SELECT ... FOR UPDATE / FOR SHARE 不被 CheckMViewUpdatable 拦截（只拦 DML）
#   2. 锁住 mview 行 → 阻塞 COMPLETE refresh（DELETE 需要获取行锁），结果正确
#   3. 锁住 mview 行 → 阻塞 FAST refresh（当锁的行恰好是 FAST refresh 要修改的行时），结果正确
#   4. 锁住 mlog 行 → 不阻塞 COMPLETE refresh（COMPLETE 不操作 mlog），结果正确
#   5. 锁住 mlog 行 → 不阻塞 FAST refresh（FAST refresh 用快照读 mlog，不获取行锁），结果正确

set -euo pipefail

HOST="${1:-127.0.0.1}"
PORT="${2:-4000}"
DB="lock_test_mview"

# Session A lock hold time (seconds)
HOLD=10
# Wait for Session A to acquire lock before starting Session B
WAIT=2

PASS=0
FAIL=0

sql() {
    mysql --comments -h "$HOST" -P "$PORT" -u root -N "$DB" 2>/dev/null
}

sql_raw() {
    mysql --comments -h "$HOST" -P "$PORT" -u root -N 2>/dev/null
}

log()    { echo "  $*"; }
header() { echo; echo "=== $* ==="; }
pass()   { log "✓ PASS: $*"; PASS=$((PASS + 1)); }
fail()   { log "✗ FAIL: $*"; FAIL=$((FAIL + 1)); }

# Verify mview matches base table truth: SELECT a, sum(b), count(1) FROM t GROUP BY a
check_mview_correct() {
    local label="$1"
    local actual expected
    actual=$(echo "SELECT a, s, cnt FROM v ORDER BY a;" | sql)
    expected=$(echo "SELECT a, sum(b), count(1) FROM t GROUP BY a ORDER BY a;" | sql)
    if [ "$actual" = "$expected" ]; then
        pass "$label — mview data correct"
    else
        fail "$label — mview data MISMATCH"
        log "  actual:   $(echo "$actual" | tr '\n' '|')"
        log "  expected: $(echo "$expected" | tr '\n' '|')"
    fi
}

# -------------------------------------------------------------------
header "Setup"
# -------------------------------------------------------------------
echo "DROP DATABASE IF EXISTS $DB; CREATE DATABASE $DB;" | sql_raw
sql <<'SQL'
CREATE TABLE t (a INT NOT NULL PRIMARY KEY, b INT NOT NULL);
INSERT INTO t VALUES (1, 10), (2, 20), (3, 30);
CREATE MATERIALIZED VIEW LOG ON t (a, b);
CREATE MATERIALIZED VIEW v (a, s, cnt) AS SELECT a, sum(b), count(1) FROM t GROUP BY a;
REFRESH MATERIALIZED VIEW v COMPLETE;
SQL
log "Created table t, mlog, mview v. Initial COMPLETE refresh done."
check_mview_correct "setup"

# -------------------------------------------------------------------
header "场景 1: FOR UPDATE on mview → COMPLETE refresh"
header "  预期: 阻塞, 等锁释放后成功, 结果正确"
# -------------------------------------------------------------------
echo "BEGIN PESSIMISTIC; SELECT * FROM v WHERE a = 1 FOR UPDATE; SELECT SLEEP($HOLD); ROLLBACK;" | sql &
SA_PID=$!
sleep "$WAIT"

START=$SECONDS
RESULT=$(echo "REFRESH MATERIALIZED VIEW v COMPLETE; SELECT 'OK';" | sql 2>&1) || true
ELAPSED=$((SECONDS - START))

wait $SA_PID 2>/dev/null || true

if echo "$RESULT" | grep -q 'OK'; then
    if [ "$ELAPSED" -ge "$((HOLD - WAIT - 1))" ]; then
        pass "COMPLETE refresh blocked ${ELAPSED}s (lock held ${HOLD}s), then succeeded"
    else
        fail "COMPLETE refresh succeeded in ${ELAPSED}s — expected to be blocked ~$((HOLD - WAIT))s"
    fi
else
    fail "COMPLETE refresh failed unexpectedly: $RESULT"
fi
check_mview_correct "scenario 1"

# -------------------------------------------------------------------
header "场景 2: FOR UPDATE on mlog → COMPLETE refresh"
header "  预期: 不阻塞, 立即成功, 结果正确"
# -------------------------------------------------------------------
echo "INSERT INTO t VALUES (4, 40);" | sql

echo "BEGIN PESSIMISTIC; SELECT * FROM \`\$mlog\$t\` FOR UPDATE; SELECT SLEEP($HOLD); ROLLBACK;" | sql &
SA_PID=$!
sleep "$WAIT"

START=$SECONDS
RESULT=$(echo "REFRESH MATERIALIZED VIEW v COMPLETE; SELECT 'OK';" | sql 2>&1) || true
ELAPSED=$((SECONDS - START))

wait $SA_PID 2>/dev/null || true

if echo "$RESULT" | grep -q 'OK'; then
    if [ "$ELAPSED" -le 2 ]; then
        pass "COMPLETE refresh not blocked (${ELAPSED}s), mlog locks don't affect COMPLETE"
    else
        fail "COMPLETE refresh took ${ELAPSED}s — expected instant"
    fi
else
    fail "COMPLETE refresh failed: $RESULT"
fi
check_mview_correct "scenario 2"

# -------------------------------------------------------------------
header "场景 3a: FOR UPDATE on mview (非冲突行) → FAST refresh"
header "  预期: 不阻塞, 立即成功, 结果正确"
# -------------------------------------------------------------------
echo "REFRESH MATERIALIZED VIEW v COMPLETE;" | sql
echo "INSERT INTO t VALUES (5, 50);" | sql

# 锁 a=1，但 FAST refresh 只需要 INSERT a=5
echo "BEGIN PESSIMISTIC; SELECT * FROM v WHERE a = 1 FOR UPDATE; SELECT SLEEP($HOLD); ROLLBACK;" | sql &
SA_PID=$!
sleep "$WAIT"

START=$SECONDS
RESULT=$(echo "REFRESH MATERIALIZED VIEW v FAST; SELECT 'OK';" | sql 2>&1) || true
ELAPSED=$((SECONDS - START))

wait $SA_PID 2>/dev/null || true

if echo "$RESULT" | grep -q 'OK'; then
    if [ "$ELAPSED" -le 2 ]; then
        pass "FAST refresh not blocked (${ELAPSED}s), non-conflicting mview lock is fine"
    else
        fail "FAST refresh took ${ELAPSED}s — expected instant (non-conflicting row)"
    fi
else
    fail "FAST refresh failed: $RESULT"
fi
check_mview_correct "scenario 3a"

# -------------------------------------------------------------------
header "场景 3b: FOR UPDATE on mview (冲突行) → FAST refresh"
header "  预期: 阻塞, 等锁释放后成功, 结果正确"
# -------------------------------------------------------------------
echo "REFRESH MATERIALIZED VIEW v COMPLETE;" | sql
echo "UPDATE t SET b = 11 WHERE a = 1;" | sql

# 锁 a=1，FAST refresh 也需要 UPDATE mview 中 a=1
echo "BEGIN PESSIMISTIC; SELECT * FROM v WHERE a = 1 FOR UPDATE; SELECT SLEEP($HOLD); ROLLBACK;" | sql &
SA_PID=$!
sleep "$WAIT"

START=$SECONDS
RESULT=$(echo "REFRESH MATERIALIZED VIEW v FAST; SELECT 'OK';" | sql 2>&1) || true
ELAPSED=$((SECONDS - START))

wait $SA_PID 2>/dev/null || true

if echo "$RESULT" | grep -q 'OK'; then
    if [ "$ELAPSED" -ge "$((HOLD - WAIT - 1))" ]; then
        pass "FAST refresh blocked ${ELAPSED}s (conflicting row), then succeeded"
    else
        fail "FAST refresh succeeded in ${ELAPSED}s — expected to be blocked ~$((HOLD - WAIT))s"
    fi
else
    fail "FAST refresh failed: $RESULT"
fi
check_mview_correct "scenario 3b"

# -------------------------------------------------------------------
header "场景 4: FOR UPDATE on mlog → FAST refresh"
header "  预期: 不阻塞, 立即成功, 结果正确"
# -------------------------------------------------------------------
echo "REFRESH MATERIALIZED VIEW v COMPLETE;" | sql
echo "INSERT INTO t VALUES (6, 60);" | sql

echo "BEGIN PESSIMISTIC; SELECT * FROM \`\$mlog\$t\` FOR UPDATE; SELECT SLEEP($HOLD); ROLLBACK;" | sql &
SA_PID=$!
sleep "$WAIT"

START=$SECONDS
RESULT=$(echo "REFRESH MATERIALIZED VIEW v FAST; SELECT 'OK';" | sql 2>&1) || true
ELAPSED=$((SECONDS - START))

wait $SA_PID 2>/dev/null || true

if echo "$RESULT" | grep -q 'OK'; then
    if [ "$ELAPSED" -le 2 ]; then
        pass "FAST refresh not blocked (${ELAPSED}s), mlog locks don't affect FAST"
    else
        fail "FAST refresh took ${ELAPSED}s — expected instant"
    fi
else
    fail "FAST refresh failed: $RESULT"
fi
check_mview_correct "scenario 4"

# -------------------------------------------------------------------
header "场景 5: 验证 FOR UPDATE / FOR UPDATE NOWAIT 不被拦截"
header "  预期: 全部允许 (CheckMViewUpdatable 只拦 DML)"
# -------------------------------------------------------------------
for target in "v" '`$mlog$t`'; do
    for lock_type in "FOR UPDATE" "FOR UPDATE NOWAIT"; do
        RESULT=$(echo "BEGIN PESSIMISTIC; SELECT * FROM $target $lock_type; ROLLBACK;" | sql 2>&1) || true
        if echo "$RESULT" | grep -qi 'error'; then
            fail "SELECT $lock_type on $target was rejected: $RESULT"
        else
            pass "SELECT $lock_type on $target is allowed"
        fi
    done
done

# -------------------------------------------------------------------
header "Cleanup"
# -------------------------------------------------------------------
echo "DROP DATABASE IF EXISTS $DB;" | sql_raw
log "Dropped database $DB"

# -------------------------------------------------------------------
header "Summary"
# -------------------------------------------------------------------
echo
echo "  Total: $((PASS + FAIL))  Passed: $PASS  Failed: $FAIL"
echo
echo "  结论:"
echo "  - SELECT ... FOR UPDATE 对 mview/mlog 都不被拦截 (只拦 DML)"
echo "  - 锁 mview 行 → 阻塞 COMPLETE/FAST refresh (当行冲突时), 结果正确"
echo "  - 锁 mlog 行 → 不阻塞任何 refresh, 结果正确"
echo "  - 建议: 在 planner 层拦截 mview/mlog 上的 FOR UPDATE, 与 DML 保持一致"
echo

if [ "$FAIL" -gt 0 ]; then
    exit 1
fi
