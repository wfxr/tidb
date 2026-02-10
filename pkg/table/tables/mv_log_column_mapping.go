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
	"strings"

	"github.com/pingcap/errors"
	"github.com/pingcap/tidb/pkg/kv"
	"github.com/pingcap/tidb/pkg/meta/model"
	"github.com/pingcap/tidb/pkg/table"
	"github.com/pingcap/tidb/pkg/tablecodec"
	"github.com/pingcap/tidb/pkg/types"
)

// mvLogColumnMapping holds precomputed column mapping between a base table and its mlog table.
type mvLogColumnMapping struct {
	// loggedColOffsets contains offsets of logged columns in the base table row.
	loggedColOffsets []int
	// loggedColInfos contains the mlog table column objects for the logged columns.
	loggedColInfos []*table.Column
	// dmlTypeCol is the column info for _MLOG$_DML_TYPE in the mlog table.
	dmlTypeCol *table.Column
	// oldNewCol is the column info for _MLOG$_OLD_NEW in the mlog table.
	oldNewCol *table.Column
}

// newMVLogColumnMapping builds the mapping from mlog logged columns to base table column offsets.
func newMVLogColumnMapping(base, mlog table.Table) (*mvLogColumnMapping, error) {
	mlogMeta := mlog.Meta()
	mlogInfo := mlogMeta.MaterializedViewLog
	if mlogInfo == nil {
		return nil, errors.Errorf("table %s is not a materialized view log table", mlogMeta.Name.O)
	}
	baseMeta := base.Meta()
	if mlogInfo.BaseTableID != baseMeta.ID {
		return nil, errors.Errorf("mlog base table ID mismatch: mlog references %d, base table is %d",
			mlogInfo.BaseTableID, baseMeta.ID)
	}

	// Build a map from base table column name (lowercase) to offset.
	baseCols := base.Cols()
	baseColMap := make(map[string]int, len(baseCols))
	for i, col := range baseCols {
		baseColMap[col.Name.L] = i
	}

	loggedCols := mlogInfo.Columns
	m := &mvLogColumnMapping{
		loggedColOffsets: make([]int, 0, len(loggedCols)),
		loggedColInfos:   make([]*table.Column, 0, len(loggedCols)),
	}

	// Map each logged column name to a base table offset and mlog table column.
	mlogCols := mlog.Cols()
	mlogColMap := make(map[string]*table.Column, len(mlogCols))
	for _, col := range mlogCols {
		mlogColMap[col.Name.L] = col
	}

	for _, colName := range loggedCols {
		offset, ok := baseColMap[colName.L]
		if !ok {
			return nil, errors.Errorf("logged column %s not found in base table %s", colName.O, baseMeta.Name.O)
		}
		mlogCol, ok := mlogColMap[colName.L]
		if !ok {
			return nil, errors.Errorf("logged column %s not found in mlog table %s", colName.O, mlogMeta.Name.O)
		}
		m.loggedColOffsets = append(m.loggedColOffsets, offset)
		m.loggedColInfos = append(m.loggedColInfos, mlogCol)
	}

	// Locate the two system columns (keys are lowercase in col.Name.L).
	dmlTypeColKey := strings.ToLower(model.MaterializedViewLogDMLTypeColumnName)
	dmlTypeCol, ok := mlogColMap[dmlTypeColKey]
	if !ok {
		return nil, errors.Errorf("column %s not found in mlog table %s",
			model.MaterializedViewLogDMLTypeColumnName, mlogMeta.Name.O)
	}
	m.dmlTypeCol = dmlTypeCol

	oldNewColKey := strings.ToLower(model.MaterializedViewLogOldNewColumnName)
	oldNewCol, ok := mlogColMap[oldNewColKey]
	if !ok {
		return nil, errors.Errorf("column %s not found in mlog table %s",
			model.MaterializedViewLogOldNewColumnName, mlogMeta.Name.O)
	}
	m.oldNewCol = oldNewCol

	return m, nil
}

// extractLoggedValues extracts logged column values from a base table row.
// Returns an error if any logged column offset is out of bounds (column pruning).
func (m *mvLogColumnMapping) extractLoggedValues(r []types.Datum) ([]types.Datum, error) {
	vals := make([]types.Datum, len(m.loggedColOffsets))
	for i, offset := range m.loggedColOffsets {
		if offset >= len(r) {
			return nil, errors.Errorf("row length %d is too short for logged column at offset %d", len(r), offset)
		}
		r[offset].Copy(&vals[i])
	}
	return vals, nil
}

// extractLoggedValuesWithBackfill is like extractLoggedValues but reads from KV
// when the row is pruned (DELETE column pruning). The KV read happens before
// the row is deleted, so the data is still accessible.
func (m *mvLogColumnMapping) extractLoggedValuesWithBackfill(
	ctx table.MutateContext,
	txn kv.Transaction,
	baseTbl table.Table,
	h kv.Handle,
	r []types.Datum,
) ([]types.Datum, error) {
	vals, err := m.extractLoggedValues(r)
	if err == nil {
		return vals, nil
	}
	// Row is pruned — backfill from KV and retry.
	backfilled, berr := m.backfillRow(ctx, txn, baseTbl, h, r)
	if berr != nil {
		return nil, berr
	}
	return m.extractLoggedValues(backfilled)
}

// backfillRow reads the full row from KV and extends r with decoded values for
// offsets that are out of bounds. This handles DELETE column pruning.
func (m *mvLogColumnMapping) backfillRow(
	ctx table.MutateContext,
	txn kv.Transaction,
	baseTbl table.Table,
	h kv.Handle,
	r []types.Datum,
) ([]types.Datum, error) {
	key := tablecodec.EncodeRecordKey(baseTbl.RecordPrefix(), h)
	value, err := txn.Get(context.TODO(), key)
	if err != nil {
		return nil, errors.Trace(err)
	}

	baseMeta := baseTbl.Meta()
	baseCols := baseTbl.Cols()
	decoded, _, err := DecodeRawRowData(ctx.GetExprCtx(), baseMeta, h, baseCols, value)
	if err != nil {
		return nil, errors.Trace(err)
	}

	// Find the max offset we need.
	maxOffset := 0
	for _, offset := range m.loggedColOffsets {
		if offset > maxOffset {
			maxOffset = offset
		}
	}
	if maxOffset >= len(r) {
		extended := make([]types.Datum, maxOffset+1)
		copy(extended, r)
		r = extended
	}
	for _, offset := range m.loggedColOffsets {
		if offset < len(decoded) {
			r[offset] = decoded[offset]
		}
	}
	return r, nil
}

// buildLogRow constructs a datum row for the mlog table from pre-extracted logged values.
func (m *mvLogColumnMapping) buildLogRow(loggedVals []types.Datum, dmlType MVLogDMLType, oldNew int64) []types.Datum {
	row := make([]types.Datum, 0, len(loggedVals)+2)
	row = append(row, loggedVals...)
	row = append(row, types.NewStringDatum(string(dmlType)), types.NewIntDatum(oldNew))
	return row
}

// anyLoggedColTouched returns true if any logged column is in the touched set.
// touched[] is the planner's SET-clause bitmap — a superset of "value actually changed".
// This is an O(logged_cols) boolean check with no allocation, replacing the previous
// per-datum value comparison that required collation.
func (m *mvLogColumnMapping) anyLoggedColTouched(touched []bool) bool {
	for _, offset := range m.loggedColOffsets {
		if offset < len(touched) && touched[offset] {
			return true
		}
	}
	return false
}
