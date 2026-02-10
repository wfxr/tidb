// Copyright 2025 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package tables

import (
	"context"

	"github.com/pingcap/errors"
	"github.com/pingcap/tidb/pkg/kv"
	"github.com/pingcap/tidb/pkg/table"
	"github.com/pingcap/tidb/pkg/types"
)

// MVLogDMLType represents the type of DML operation recorded in a materialized view log.
type MVLogDMLType string

const (
	// MVLogDMLTypeInsert represents an INSERT operation.
	MVLogDMLTypeInsert MVLogDMLType = "I"
	// MVLogDMLTypeDelete represents a DELETE operation.
	MVLogDMLTypeDelete MVLogDMLType = "D"
	// MVLogDMLTypeUpdate represents an UPDATE operation.
	MVLogDMLTypeUpdate MVLogDMLType = "U"
	// MVLogDMLTypeReplace represents a REPLACE operation.
	MVLogDMLTypeReplace MVLogDMLType = "R"
	// MVLogDMLTypeLoadData represents a LOAD DATA operation.
	MVLogDMLTypeLoadData MVLogDMLType = "L"
)

// materializedViewLogTable wraps a base table with a materialized view log table.
// It intercepts DML operations (AddRecord, UpdateRecord, RemoveRecord) and
// synchronously writes change records to the mlog table within the same transaction.
//
// Design note: mlog logging intentionally trades precision for simplicity — it may
// write redundant entries when logged column values haven't actually changed. This is
// acceptable because the materialized view refresh process already deduplicates and
// cleans up mlog rows; the extra writes only cost a small amount of transactional I/O.
type materializedViewLogTable struct {
	table.Table                // embedded base table (delegates all non-overridden methods)
	logTbl         table.Table // the mlog table to write into
	mapping        *mvLogColumnMapping
	defaultDMLType MVLogDMLType // statement-level DML type (I/D/U/R/L)

	// pendingOldRows stores extracted logged values from RemoveRecord calls that are
	// part of a handle-changed UPDATE or REPLACE. The subsequent AddRecord call
	// consumes these to decide whether to write log entries.
	pendingOldRows [][]types.Datum
}

// WrapTableWithMaterializedViewLog wraps a base table with its materialized view log table.
// If the base table has no logged columns, the original table is returned unchanged.
func WrapTableWithMaterializedViewLog(base, mlog table.Table, dmlType MVLogDMLType) (table.Table, error) {
	baseMeta := base.Meta()
	mlogMeta := mlog.Meta()

	// Validate cross-references.
	if baseMeta.MaterializedViewBase == nil || baseMeta.MaterializedViewBase.MLogID != mlogMeta.ID {
		return nil, errors.Errorf("base table %s does not reference mlog table %s",
			baseMeta.Name.O, mlogMeta.Name.O)
	}
	if mlogMeta.MaterializedViewLog == nil || mlogMeta.MaterializedViewLog.BaseTableID != baseMeta.ID {
		return nil, errors.Errorf("mlog table %s does not reference base table %s",
			mlogMeta.Name.O, baseMeta.Name.O)
	}

	// Reject partitioned tables.
	if baseMeta.GetPartitionInfo() != nil {
		return nil, errors.Errorf("materialized view log is not supported on partitioned table %s", baseMeta.Name.O)
	}

	// Build column mapping.
	m, err := newMVLogColumnMapping(base, mlog)
	if err != nil {
		return nil, err
	}

	// If no logged columns, no need to wrap.
	if len(m.loggedColOffsets) == 0 {
		return base, nil
	}

	return &materializedViewLogTable{
		Table:          base,
		logTbl:         mlog,
		mapping:        m,
		defaultDMLType: dmlType,
	}, nil
}

// AddRecord intercepts row insertion and writes log entries to the mlog table.
//
// Three cases:
//  1. opt.IsUpdate() — handle-changed UPDATE/ODKU: pops pending old row,
//     writes OLD+NEW unconditionally.
//  2. Pending old rows exist — REPLACE/LOAD conflict: pops pending old row,
//     writes OLD+NEW unconditionally.
//  3. Otherwise — pure INSERT/LOAD: writes a NEW row unconditionally.
//
// Cases 1 & 2 always write both OLD and NEW without comparing values. Unlike
// UpdateRecord (which has access to the planner's touched[] array), AddRecord
// has no cheap way to know which columns changed — the only option would be
// per-datum comparison with collation, which is expensive. Unconditional logging
// is simpler and the redundant rows are harmless (cleaned up during refresh).
func (t *materializedViewLogTable) AddRecord(ctx table.MutateContext, txn kv.Transaction, r []types.Datum, opts ...table.AddRecordOption) (kv.Handle, error) {
	opt := table.NewAddRecordOpt(opts...)

	h, err := t.Table.AddRecord(ctx, txn, r, opts...)
	if err != nil {
		return h, err
	}

	newVals, err := t.mapping.extractLoggedValues(r)
	if err != nil {
		return h, errors.Trace(err)
	}

	mutateCtx := opt.Ctx()
	lazyCheck := opt.PessimisticLazyDupKeyCheck()

	if opt.IsUpdate() {
		// Handle-changed UPDATE/ODKU path.
		oldVals, ok := t.popPendingOldRow()
		if !ok {
			return h, errors.New("missing pending old row for handle-changed update")
		}
		if err := t.appendLogRow(ctx, txn, oldVals, MVLogDMLTypeUpdate, -1, mutateCtx, lazyCheck); err != nil {
			return h, err
		}
		return h, t.appendLogRow(ctx, txn, newVals, MVLogDMLTypeUpdate, +1, mutateCtx, lazyCheck)
	}

	// Check for pending REPLACE/LOAD old rows.
	if oldVals, ok := t.popPendingOldRow(); ok {
		if err := t.appendLogRow(ctx, txn, oldVals, t.defaultDMLType, -1, mutateCtx, lazyCheck); err != nil {
			return h, err
		}
		return h, t.appendLogRow(ctx, txn, newVals, t.defaultDMLType, +1, mutateCtx, lazyCheck)
	}

	// Pure INSERT / LOAD DATA INSERT.
	return h, t.appendLogRow(ctx, txn, newVals, t.defaultDMLType, +1, mutateCtx, lazyCheck)
}

// UpdateRecord intercepts row update and writes OLD+NEW rows to the mlog table
// if any logged column is touched by the SET clause.
//
// We use the planner-provided touched[] (which marks columns in the SET clause)
// instead of comparing old/new datum values. touched[] is a superset of "actually
// changed" — e.g. `SET b = b` marks b as touched even though the value is the same —
// so this may produce redundant mlog rows, but avoids per-datum comparison with
// collation. When no logged column is touched, we skip extractLoggedValues entirely,
// making the common case (updating non-logged columns) zero-overhead.
func (t *materializedViewLogTable) UpdateRecord(ctx table.MutateContext, txn kv.Transaction, h kv.Handle, oldData, newData []types.Datum, touched []bool, opts ...table.UpdateRecordOption) error {
	if !t.mapping.anyLoggedColTouched(touched) {
		return t.Table.UpdateRecord(ctx, txn, h, oldData, newData, touched, opts...)
	}

	oldVals, err := t.mapping.extractLoggedValues(oldData)
	if err != nil {
		return errors.Trace(err)
	}
	newVals, err := t.mapping.extractLoggedValues(newData)
	if err != nil {
		return errors.Trace(err)
	}

	if err := t.Table.UpdateRecord(ctx, txn, h, oldData, newData, touched, opts...); err != nil {
		return err
	}

	opt := table.NewUpdateRecordOpt(opts...)
	mutateCtx := opt.Ctx()
	lazyCheck := opt.PessimisticLazyDupKeyCheck()
	dmlType := MVLogDMLTypeUpdate
	if t.defaultDMLType == MVLogDMLTypeLoadData {
		dmlType = MVLogDMLTypeLoadData
	}
	if err := t.appendLogRow(ctx, txn, oldVals, dmlType, -1, mutateCtx, lazyCheck); err != nil {
		return err
	}
	return t.appendLogRow(ctx, txn, newVals, dmlType, +1, mutateCtx, lazyCheck)
}

// RemoveRecord intercepts row deletion and either logs immediately or stashes
// old values for a subsequent AddRecord.
//
//   - DELETE (defaultDMLType=D): logs OLD row immediately.
//   - Otherwise (UPDATE/INSERT/REPLACE/LOAD): stashes for AddRecord to consume.
//
// The logged values are extracted BEFORE base.RemoveRecord to ensure KV data is
// still accessible (handles DELETE column pruning via backfillRow).
func (t *materializedViewLogTable) RemoveRecord(ctx table.MutateContext, txn kv.Transaction, h kv.Handle, r []types.Datum, opts ...table.RemoveRecordOption) error {
	// Extract logged values BEFORE deletion (KV data still readable).
	loggedVals, err := t.mapping.extractLoggedValuesWithBackfill(ctx, txn, t.Table, h, r)
	if err != nil {
		return errors.Trace(err)
	}

	if err := t.Table.RemoveRecord(ctx, txn, h, r, opts...); err != nil {
		return err
	}

	if t.defaultDMLType == MVLogDMLTypeDelete {
		// DELETE: log OLD row immediately, no subsequent AddRecord will follow.
		return t.appendLogRow(ctx, txn, loggedVals, MVLogDMLTypeDelete, -1, nil, table.DupKeyCheckInAcquireLock)
	}

	// UPDATE/INSERT/REPLACE/LOAD: stash for the subsequent AddRecord.
	t.pushPendingOldRow(loggedVals)
	return nil
}

// appendLogRow writes a single log entry to the mlog table.
func (t *materializedViewLogTable) appendLogRow(
	ctx table.MutateContext,
	txn kv.Transaction,
	loggedVals []types.Datum,
	dmlType MVLogDMLType,
	oldNew int64,
	mutateCtx context.Context,
	lazyCheck table.PessimisticLazyDupKeyCheckMode,
) error {
	logRow := t.mapping.buildLogRow(loggedVals, dmlType, oldNew)
	logOpts := []table.AddRecordOption{
		table.DupKeyCheckSkip,
		lazyCheck,
	}
	if mutateCtx != nil {
		logOpts = append(logOpts, table.WithCtx(mutateCtx))
	}
	_, err := t.logTbl.AddRecord(ctx, txn, logRow, logOpts...)
	return errors.Trace(err)
}

func (t *materializedViewLogTable) pushPendingOldRow(row []types.Datum) {
	t.pendingOldRows = append(t.pendingOldRows, row)
}

func (t *materializedViewLogTable) popPendingOldRow() ([]types.Datum, bool) {
	if len(t.pendingOldRows) == 0 {
		return nil, false
	}
	row := t.pendingOldRows[0]
	t.pendingOldRows = t.pendingOldRows[1:]
	return row, true
}
