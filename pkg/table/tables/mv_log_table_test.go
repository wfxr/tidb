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

package tables_test

import (
	"testing"

	"github.com/pingcap/tidb/pkg/testkit"
	"github.com/stretchr/testify/require"
)

func TestMLogInsert(t *testing.T) {
	store := testkit.CreateMockStore(t)
	tk := testkit.NewTestKit(t, store)
	tk.MustExec("use test")
	tk.MustExec("create table t (a int primary key, b int, c int)")
	tk.MustExec("create materialized view log on t (a, b)")
	// INSERT: should produce one NEW row (dml_type=I, old_new=1)
	tk.MustExec("insert into t values (1, 10, 100)")
	rows := tk.MustQuery("select a, b, `_MLOG$_DML_TYPE`, `_MLOG$_OLD_NEW` from `$mlog$t` order by a, `_MLOG$_OLD_NEW`").Rows()
	require.Len(t, rows, 1)
	require.Equal(t, "1", rows[0][0])
	require.Equal(t, "10", rows[0][1])
	require.Equal(t, "I", rows[0][2])
	require.Equal(t, "1", rows[0][3])
}

func TestMLogDelete(t *testing.T) {
	store := testkit.CreateMockStore(t)
	tk := testkit.NewTestKit(t, store)
	tk.MustExec("use test")
	tk.MustExec("create table t (a int primary key, b int, c int)")
	tk.MustExec("create materialized view log on t (a, b)")
	tk.MustExec("insert into t values (1, 10, 100)")
	tk.MustExec("delete from `$mlog$t`") // clear the INSERT log
	// DELETE: should produce one OLD row (dml_type=D, old_new=-1)
	tk.MustExec("delete from t where a = 1")
	rows := tk.MustQuery("select a, b, `_MLOG$_DML_TYPE`, `_MLOG$_OLD_NEW` from `$mlog$t` order by a").Rows()
	require.Len(t, rows, 1)
	require.Equal(t, "1", rows[0][0])
	require.Equal(t, "10", rows[0][1])
	require.Equal(t, "D", rows[0][2])
	require.Equal(t, "-1", rows[0][3])
}

func TestMLogUpdateRecordedColChanged(t *testing.T) {
	store := testkit.CreateMockStore(t)
	tk := testkit.NewTestKit(t, store)
	tk.MustExec("use test")
	tk.MustExec("create table t (a int primary key, b int, c int)")
	tk.MustExec("create materialized view log on t (a, b)")
	tk.MustExec("insert into t values (1, 10, 100)")
	tk.MustExec("delete from `$mlog$t`")
	// UPDATE: recorded col b changed → OLD+NEW rows (dml_type=U)
	tk.MustExec("update t set b = 20 where a = 1")
	rows := tk.MustQuery("select a, b, `_MLOG$_DML_TYPE`, `_MLOG$_OLD_NEW` from `$mlog$t` order by `_MLOG$_OLD_NEW`").Rows()
	require.Len(t, rows, 2)
	// OLD row (old_new=-1)
	require.Equal(t, "1", rows[0][0])
	require.Equal(t, "10", rows[0][1])
	require.Equal(t, "U", rows[0][2])
	require.Equal(t, "-1", rows[0][3])
	// NEW row (old_new=1)
	require.Equal(t, "1", rows[1][0])
	require.Equal(t, "20", rows[1][1])
	require.Equal(t, "U", rows[1][2])
	require.Equal(t, "1", rows[1][3])
}

func TestMLogUpdateNonRecordedColOnly(t *testing.T) {
	store := testkit.CreateMockStore(t)
	tk := testkit.NewTestKit(t, store)
	tk.MustExec("use test")
	tk.MustExec("create table t (a int primary key, b int, c int)")
	tk.MustExec("create materialized view log on t (a, b)")
	tk.MustExec("insert into t values (1, 10, 100)")
	tk.MustExec("delete from `$mlog$t`")
	// UPDATE: only non-recorded col c changed → no mlog rows
	tk.MustExec("update t set c = 200 where a = 1")
	rows := tk.MustQuery("select count(*) from `$mlog$t`").Rows()
	require.Equal(t, "0", rows[0][0])
}

func TestMLogUpdateHandleChanged(t *testing.T) {
	store := testkit.CreateMockStore(t)
	tk := testkit.NewTestKit(t, store)
	tk.MustExec("use test")
	tk.MustExec("create table t (a int primary key, b int, c int)")
	tk.MustExec("create materialized view log on t (a, b)")
	tk.MustExec("insert into t values (1, 10, 100)")
	tk.MustExec("delete from `$mlog$t`")
	// UPDATE: handle (PK a) changed, recorded col a changed → OLD+NEW (dml_type=U)
	tk.MustExec("update t set a = 2 where a = 1")
	rows := tk.MustQuery("select a, b, `_MLOG$_DML_TYPE`, `_MLOG$_OLD_NEW` from `$mlog$t` order by `_MLOG$_OLD_NEW`").Rows()
	require.Len(t, rows, 2)
	require.Equal(t, "1", rows[0][0])  // OLD
	require.Equal(t, "-1", rows[0][3])
	require.Equal(t, "2", rows[1][0])  // NEW
	require.Equal(t, "1", rows[1][3])
}

func TestMLogUpdateHandleChangedRecordedUnchanged(t *testing.T) {
	store := testkit.CreateMockStore(t)
	tk := testkit.NewTestKit(t, store)
	tk.MustExec("use test")
	// Use non-clustered PK so handle change does not change recorded cols.
	tk.MustExec("create table t (a int primary key nonclustered, b int, c int)")
	tk.MustExec("create materialized view log on t (b)")
	tk.MustExec("insert into t values (1, 10, 100)")
	tk.MustExec("delete from `$mlog$t`")
	// UPDATE: nonclustered PK, so _tidb_rowid is the handle and doesn't change.
	// Goes through UpdateRecord; logged col b is not touched → no mlog rows.
	tk.MustExec("update t set a = 2 where a = 1")
	rows := tk.MustQuery("select count(*) from `$mlog$t`").Rows()
	require.Equal(t, "0", rows[0][0])
}

func TestMLogODKU(t *testing.T) {
	store := testkit.CreateMockStore(t)
	tk := testkit.NewTestKit(t, store)
	tk.MustExec("use test")
	tk.MustExec("create table t (a int primary key, b int, c int)")
	tk.MustExec("create materialized view log on t (a, b)")

	// ODKU: insert branch → dml_type=I
	tk.MustExec("insert into t values (1, 10, 100) on duplicate key update b = b + 1")
	rows := tk.MustQuery("select `_MLOG$_DML_TYPE`, `_MLOG$_OLD_NEW` from `$mlog$t`").Rows()
	require.Len(t, rows, 1)
	require.Equal(t, "I", rows[0][0])
	require.Equal(t, "1", rows[0][1])

	tk.MustExec("delete from `$mlog$t`")

	// ODKU: update branch → dml_type=U
	tk.MustExec("insert into t values (1, 10, 100) on duplicate key update b = b + 1")
	rows = tk.MustQuery("select `_MLOG$_DML_TYPE`, `_MLOG$_OLD_NEW` from `$mlog$t` order by `_MLOG$_OLD_NEW`").Rows()
	require.Len(t, rows, 2)
	require.Equal(t, "U", rows[0][0]) // OLD
	require.Equal(t, "-1", rows[0][1])
	require.Equal(t, "U", rows[1][0]) // NEW
	require.Equal(t, "1", rows[1][1])
}

func TestMLogReplaceRecordedColChanged(t *testing.T) {
	store := testkit.CreateMockStore(t)
	tk := testkit.NewTestKit(t, store)
	tk.MustExec("use test")
	tk.MustExec("create table t (a int primary key, b int, c int)")
	tk.MustExec("create materialized view log on t (a, b)")
	tk.MustExec("insert into t values (1, 10, 100)")
	tk.MustExec("delete from `$mlog$t`")
	// REPLACE: recorded col b changed → OLD+NEW (dml_type=R)
	tk.MustExec("replace into t values (1, 20, 200)")
	rows := tk.MustQuery("select a, b, `_MLOG$_DML_TYPE`, `_MLOG$_OLD_NEW` from `$mlog$t` order by `_MLOG$_OLD_NEW`").Rows()
	require.Len(t, rows, 2)
	require.Equal(t, "R", rows[0][2])
	require.Equal(t, "-1", rows[0][3]) // OLD
	require.Equal(t, "R", rows[1][2])
	require.Equal(t, "1", rows[1][3])  // NEW
}

func TestMLogReplaceNonRecordedColOnly(t *testing.T) {
	store := testkit.CreateMockStore(t)
	tk := testkit.NewTestKit(t, store)
	tk.MustExec("use test")
	tk.MustExec("create table t (a int primary key, b int, c int)")
	tk.MustExec("create materialized view log on t (a, b)")
	tk.MustExec("insert into t values (1, 10, 100)")
	tk.MustExec("delete from `$mlog$t`")
	// REPLACE: only non-logged col c changed → OLD+NEW logged (redundant but acceptable)
	tk.MustExec("replace into t values (1, 10, 200)")
	rows := tk.MustQuery("select a, b, `_MLOG$_DML_TYPE`, `_MLOG$_OLD_NEW` from `$mlog$t` order by `_MLOG$_OLD_NEW`").Rows()
	require.Len(t, rows, 2)
	require.Equal(t, "R", rows[0][2])
	require.Equal(t, "-1", rows[0][3]) // OLD
	require.Equal(t, "R", rows[1][2])
	require.Equal(t, "1", rows[1][3])  // NEW
}

func TestMLogPartitionTableRejected(t *testing.T) {
	store := testkit.CreateMockStore(t)
	tk := testkit.NewTestKit(t, store)
	tk.MustExec("use test")
	// Partitioned table: mlog DML should fail because WrapTableWithMaterializedViewLog rejects it.
	tk.MustExec("create table tp (a int primary key, b int) partition by hash(a) partitions 4")
	tk.MustExec("create materialized view log on tp (a, b)")
	err := tk.ExecToErr("insert into tp values (1, 10)")
	require.Error(t, err)
	require.Contains(t, err.Error(), "materialized view log is not supported on partitioned table")
}
