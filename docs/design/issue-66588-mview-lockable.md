# Design: Reject locking reads on materialized view / materialized view log (issue #66588)

## Background

PR #66396 already rejects explicit DML on materialized view (MV) and materialized view log (MLog) tables via `CheckMViewUpdatable()`.

However, locking reads are still allowed on MV/MLog in current code paths:

- `SELECT ... FOR UPDATE`
- `SELECT ... FOR SHARE`
- `SELECT ... LOCK IN SHARE MODE`

The main planner lock path (`buildSelectLock`) does not perform MV/MLog checks, and fast paths (`PointGet` / `BatchPointGet`) can also bypass the check.

This can conflict with `REFRESH MATERIALIZED VIEW` (which runs in pessimistic transactions and writes to MV tables).

## Goals

1. Reject locking reads on MV/MLog consistently across all planner paths.
2. Cover both normal planner lock path and point/batch point-get fast paths.
3. Use clear, lock-specific error semantics.

## Non-goals

- No change to normal non-locking `SELECT`.
- No change to existing DML reject behavior (still uses 1288 where applicable).
- No syntax change and no new system variable.

## Decision summary

- New helper name: `CheckMViewLockable`.
- Locking-read errors use code **8040** (`ErrUnsupportedOp`) with custom message, instead of reusing 1288.
- The lockability check applies only to **explicit SELECT locking reads** (user SQL with lock syntax).
  Internal lock injection used by `UPDATE`/`DELETE` pessimistic optimization must keep existing DML behavior.
- Fast paths do not throw directly; they fallback to normal planning and let the normal lock path return the final planner error.
- Lock-type to operation mapping is centralized in one helper and must include `... SKIP LOCKED` variants.

## Public behavior change

For MV and MLog tables, locking reads return 8040 with explicit message, e.g. lock read unsupported on materialized view/log tables.

Notes:

- `FOR SHARE` / `LOCK IN SHARE MODE` still obey existing noop-function gate behavior. If noop mode blocks first, existing noop error behavior remains unchanged.
- Internal lock injection for `UPDATE`/`DELETE` pessimistic optimization is out of scope for this 8040 path; existing DML reject semantics remain unchanged.

## Proposed code changes

### 1) Add lockability checker in planner core

**File:** `pkg/planner/core/util.go`

Add:

```go
func CheckMViewLockable(
    sv *variable.SessionVars,
    tableInfo *model.TableInfo,
    aliasName string,
    lockType ast.SelectLockType,
) error
```

Behavior:

1. Return `nil` if table is neither MV nor MLog.
2. If in MV maintenance mode:
   - allow when `InRestrictedSQL == true`;
   - otherwise return existing internal error (same invariant as `CheckMViewUpdatable`).
3. Map lock type to operation string (single source of truth; reused by normal path and fast path):
   - `FOR UPDATE`, `FOR UPDATE NOWAIT`, `FOR UPDATE WAIT N`, `FOR UPDATE SKIP LOCKED` -> `SELECT FOR UPDATE`
   - `FOR SHARE`, `FOR SHARE NOWAIT`, `FOR SHARE SKIP LOCKED` -> `SELECT FOR SHARE`
   - `LOCK IN SHARE MODE` is parsed as `FOR SHARE`, so it uses the same mapping.
   - other lock types -> `nil`
4. Determine table kind string:
   - `materialized view`
   - `materialized view log`
5. Return new planner error (8040), carrying operation + kind + table name.

### 2) Add planner error for MV/MLog locking read

**File:** `pkg/util/dbterror/plannererrors/planner_terror.go`

Add a new error variable in planner errors, built from `mysql.ErrUnsupportedOp` (8040) with custom message.

Suggested message template:

```text
%s is not supported on %s %-.100s
```

Formatting note:

- Keep `%-.100s` to align with existing MySQL/TiDB error templates.
- This template is rendered through TiDB's standard SQL error formatting path (ultimately using `fmt.Sprintf`), so string precision truncation is preserved.

Arguments:

1. operation (`SELECT FOR UPDATE` / `SELECT FOR SHARE`)
2. table kind (`materialized view` / `materialized view log`)
3. alias or table name

Also register this error in:

**File:** `pkg/util/dbterror/plannererrors/errors_test.go`

so error-code consistency tests include this new planner error.

### 3) Enforce check in normal lock path

**Files:** `pkg/planner/core/logical_plan_builder.go`, `pkg/planner/core/planbuilder.go`

Add a pre-check step for **SELECT statement lock syntax path** (the path where `sel.LockInfo` is handled),
before constructing `LogicalLock`.

Execution order requirement:

- This check must run after preprocess/noop-function gate handling.
- For `LOCK IN SHARE MODE` under `tidb_enable_noop_functions=WARN`, warning is appended first by noop gate, then lockability check returns 8040 if target is MV/MLog.

Important scope guard:

- Do **not** apply this check to internal/synthetic lock insertion used by `UPDATE`/`DELETE`
  (`buildUpdate`/`buildDelete` calling `buildSelectLock(..., FOR UPDATE)` for optimization).
- This preserves existing DML reject semantics (`CheckMViewUpdatable`, error 1288) and avoids behavior drift.

For explicit SELECT lock checks:

Decision flow (explicit SELECT lock syntax only):

| Condition | Tables to check | Why |
| --- | --- | --- |
| `lock.Tables` is empty | all lockable table IDs from `b.handleHelper.tailMap()` | matches the lock scope of regular locking read |
| `lock.Tables` is non-empty and any targets resolve | only `resolvedIDs` | execution uses `LockTableIDs` to limit lock scope to resolved targets |
| `lock.Tables` is non-empty, none resolve, and `unresolvedCount > 0` | fallback to all lockable table IDs from `b.handleHelper.tailMap()` | fail-closed to avoid bypass via unresolvable `OF` targets |
| lock type does not map to lock-read operation string | skip MV/MLog lockability check | no locking-read semantics to reject |

- If `lock.Tables` is empty:
  - check all actually lockable table IDs from `b.handleHelper.tailMap()` by resolving `TableInfo` from infoschema.
  - API choice: use `b.is.TableInfoByID(tableID)` (metadata-only lookup), not `TableByID`.
- If `lock.Tables` is non-empty (e.g. `FOR UPDATE OF ...`):
  - Try resolving each target via `resolveCtx.GetTableName`.
  - Collect `resolvedIDs` and `unresolvedCount`.
  - If `resolvedIDs` is non-empty:
    - check only `resolvedIDs` (match execution behavior where non-empty `LockTableIDs` limits lock scope to this set).
  - If `resolvedIDs` is empty and `unresolvedCount > 0`:
    - run a fail-closed fallback check on all lockable table IDs from `b.handleHelper.tailMap()`.
    - This prevents bypass when all explicit lock targets are unresolvable to `TableInfo`.
    - Keep this branch as defense-in-depth even if some unresolved `OF` forms may also be rejected earlier by other validation paths.

For each checked target table, return error immediately on violation.

### 4) Enforce check in fast paths (avoid bypass)

**File:** `pkg/planner/core/point_get_plan.go`

Apply lockability check in both:

- `tryPointGetPlan(...)`
- `tryWhereIn2BatchPointGet(...)`

When `selStmt.LockInfo` exists and lock type maps to a lock-read operation string
(`SELECT FOR UPDATE` or `SELECT FOR SHARE`, including `... SKIP LOCKED`) and target is MV/MLog:

- call `CheckMViewLockable(...)`;
- if it returns error, return `nil` from fast-path builder, so planner falls back to normal path and reports the same final error there.

This preserves fast-path function contract and avoids introducing new error plumbing.

Implementation note:

- Do not gate this with `logicalop.IsSupportedSelectLockType`, because current helper does not include `SKIP LOCKED`.
- Reuse the same lock-type mapper used by `CheckMViewLockable` to avoid divergence.

## Test plan

### Unit tests

1. **`pkg/planner/core/util_test.go`**
   - Add `TestCheckMViewLockable`:
     - base table + lock read -> success
     - MV + lock read -> 8040
     - MLog + lock read -> 8040
     - MV maintenance + restricted SQL -> success
     - MV maintenance without restricted SQL -> internal invariant error

2. **`pkg/util/dbterror/plannererrors/errors_test.go`**
   - Add new planner error to `kvErrs` list.

### Integration tests

**Files:**

- `tests/integrationtest/t/executor/mview_log_dml.test`
- `tests/integrationtest/r/executor/mview_log_dml.result`

Add cases for MV and MLog:

- `FOR UPDATE`
- `FOR SHARE`
- `FOR UPDATE NOWAIT`
- `FOR UPDATE WAIT N`
- `FOR UPDATE SKIP LOCKED` (lock-intent variant regression guard)
- `FOR SHARE NOWAIT` (explicit lock-type mapping regression guard)
- `FOR SHARE SKIP LOCKED` (lock-intent variant regression guard)
- `LOCK IN SHARE MODE` + `tidb_enable_noop_functions=ON` (should reach `CheckMViewLockable` and return 8040)
- `LOCK IN SHARE MODE` + `tidb_enable_noop_functions=OFF` (noop error should trigger first)
- `LOCK IN SHARE MODE` + `tidb_enable_noop_functions=WARN` (append warning, then return 8040)
- point-get shape (`pk = const`) and batch-point-get shape (`pk in (...)`) with:
  - `FOR UPDATE`
  - `FOR UPDATE SKIP LOCKED`
  - `FOR SHARE SKIP LOCKED`
- prepared execution path (`prepare` + `execute`) for lock reads
- `FOR UPDATE OF ...` selective-lock cases:
  - resolved-only base alias (must not fail with 8040), e.g.
    `SELECT * FROM base b JOIN mv m ON ... FOR UPDATE OF b`
  - resolved-only MV alias (must fail with 8040), e.g.
    `SELECT * FROM base b JOIN mv m ON ... FOR UPDATE OF m`
  - mixed resolved + unresolved (`resolvedIDs` non-empty; unresolved ignored), e.g.
    `SELECT * FROM base b JOIN mv m ON ... FOR UPDATE OF b, missing_alias`
    (must not fail with 8040 when MV/MLog alias is not in resolved set)
  - all-unresolved (`resolvedIDs` empty; fallback to all lockable IDs), e.g.
    `SELECT * FROM base b JOIN mv m ON ... FOR UPDATE OF missing_alias`
    (must fail with 8040 via fallback)
- CTE / subquery boundary cases:
  - `WITH cte AS (SELECT * FROM mv_or_mlog) SELECT * FROM cte FOR UPDATE` should be covered explicitly.
  - Expected semantics are best-effort at lock scope: if lockability check can observe underlying MV/MLog table IDs, reject with 8040; otherwise, behavior follows observed lock scope.
  - Do not rely on structural assumptions about CTE/subquery handle propagation; use tests to validate and pin current behavior.

Plan cache interaction expectations:

- Non-prepared plan cache path should not cache locking-read `SELECT` statements (existing behavior for `SelectStmt` with lock syntax).
- Prepared plan cache may cache locking-read plans.
- This check runs in planning/build path. A cache hit reuses an already-checked plan; schema/version changes for for-update read should trigger plan rebuild, then re-run checks.

Add regression guard for DML semantics unchanged:

- `UPDATE`/`DELETE` on MV/MLog in pessimistic mode should still fail through existing
  `CheckMViewUpdatable` path (1288), not new 8040 lock-read error.

Expected: lock reads on MV/MLog fail with code 8040 and lock-specific message.

## Risks and compatibility

- Correctness risk: low-medium (mainly `FOR UPDATE OF ...` target resolution and the resolved-only vs all-unresolved fallback split in `buildSelectLock`).
- Compatibility risk: low; behavior becomes stricter only for previously unintended operations.
- Performance impact: negligible (planner-time metadata check only).

## Validation commands (after implementation)

```bash
go test -run TestCheckMViewLockable -tags=intest,deadlock ./pkg/planner/core
go test -run TestError ./pkg/util/dbterror/plannererrors
pushd tests/integrationtest && ./run-tests.sh -r executor/mview_log_dml && popd
make bazel_lint_changed
```

## Acceptance criteria

1. Any MV/MLog locking read that reaches lockability check is rejected with 8040 (noop-function gate precedence cases excluded).
2. No fast-path bypass remains.
3. Base-table locking reads behave unchanged.
4. Existing noop-function behavior precedence remains unchanged.
5. `UPDATE`/`DELETE` on MV/MLog keep existing reject semantics (1288 via `CheckMViewUpdatable`).
