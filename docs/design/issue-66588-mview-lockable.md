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
- Fast paths do not throw directly; they fallback to normal planning and let the normal lock path return the final planner error.

## Public behavior change

For MV and MLog tables, locking reads return 8040 with explicit message, e.g. lock read unsupported on materialized view/log tables.

Notes:

- `FOR SHARE` / `LOCK IN SHARE MODE` still obey existing noop-function gate behavior. If noop mode blocks first, existing noop error behavior remains unchanged.

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
3. Map lock type to operation string:
   - `FOR UPDATE`, `FOR UPDATE NOWAIT`, `FOR UPDATE WAIT N`, `FOR UPDATE SKIP LOCKED` -> `SELECT FOR UPDATE`
   - `FOR SHARE`, `FOR SHARE NOWAIT`, `FOR SHARE SKIP LOCKED` -> `SELECT FOR SHARE`
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
%s is not supported on %s table %-.100s
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

**File:** `pkg/planner/core/planbuilder.go`

In `buildSelectLock` (before constructing `LogicalLock`):

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

For each checked target table, return error immediately on violation.

### 4) Enforce check in fast paths (avoid bypass)

**File:** `pkg/planner/core/point_get_plan.go`

Apply lockability check in both:

- `tryPointGetPlan(...)`
- `tryWhereIn2BatchPointGet(...)`

When lock type is a supported lock-read type and the target is MV/MLog:

- call `CheckMViewLockable(...)`;
- if it returns error, return `nil` from fast-path builder, so planner falls back to normal path and reports the same final error there.

This preserves fast-path function contract and avoids introducing new error plumbing.

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
- `FOR UPDATE NOWAIT`
- `FOR UPDATE WAIT N`
- `FOR UPDATE SKIP LOCKED` (lock-intent variant regression guard)
- `FOR SHARE NOWAIT` (explicit lock-type mapping regression guard)
- `FOR SHARE SKIP LOCKED` (lock-intent variant regression guard)
- `LOCK IN SHARE MODE` + `tidb_enable_noop_functions=ON` (should reach `CheckMViewLockable` and return 8040)
- `LOCK IN SHARE MODE` + `tidb_enable_noop_functions=OFF` (noop error should trigger first)
- `LOCK IN SHARE MODE` + `tidb_enable_noop_functions=WARN` (append warning, then return 8040)
- point-get shape (`pk = const`) and batch-point-get shape (`pk in (...)`) with `FOR UPDATE`
- prepared execution path (`prepare` + `execute`) for lock reads
- `FOR UPDATE OF ...` selective-lock cases:
  - resolved-only case: `OF` only contains resolved base-table alias while MV is also present in `FROM` (must not fail with 8040)
  - resolved-only case: `OF` contains resolved MV alias (must fail with 8040)
  - mixed case: `OF` contains resolved base alias + unresolved alias, with MV present in `FROM` but not in resolved set (must not fail with 8040)
  - all-unresolved case: `OF` targets are all unresolved aliases and underlying lockable set contains MV/MLog (must fail with 8040 via fallback)
- CTE / subquery boundary cases:
  - `WITH cte AS (SELECT * FROM mv_or_mlog) SELECT * FROM cte FOR UPDATE` should be covered explicitly.
  - Do not rely on structural assumptions about CTE handle propagation; use test to validate whether lockability check can still observe underlying MV/MLog access.

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

1. Any locking read on MV/MLog is rejected consistently with 8040.
2. No fast-path bypass remains.
3. Base-table locking reads behave unchanged.
4. Existing noop-function behavior precedence remains unchanged.
