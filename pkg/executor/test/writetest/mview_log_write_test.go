// Copyright 2026 PingCAP, Inc.
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

package writetest

import (
	"io"
	"testing"

	"github.com/pingcap/tidb/pkg/executor"
	"github.com/pingcap/tidb/pkg/lightning/mydump"
	"github.com/pingcap/tidb/pkg/parser/mysql"
	"github.com/pingcap/tidb/pkg/sessionctx"
	"github.com/pingcap/tidb/pkg/testkit"
)

func TestMaterializedViewLogWriteReplacePKAndUniqueConflict(t *testing.T) {
	store := testkit.CreateMockStore(t)
	tk := testkit.NewTestKit(t, store)
	tk.MustExec("use test")

	tk.MustExec("drop table if exists t_mlog_replace")
	tk.MustExec("create table t_mlog_replace (id int primary key, uk int unique, v int)")
	// Seed rows before creating mlog so that seed inserts won't be logged.
	tk.MustExec("insert into t_mlog_replace values (1,10,100), (2,20,200)")
	tk.MustExec("create materialized view log on t_mlog_replace (id, uk, v)")

	// This row conflicts with (1,10,100) on PK and with (2,20,200) on unique index.
	tk.MustExec("replace into t_mlog_replace values (1,20,999)")

	tk.MustQuery("select id, uk, v from t_mlog_replace order by id").Check(
		testkit.Rows("1 20 999"),
	)

	tk.MustQuery(
		"select id, uk, v, `_MLOG$_DML_TYPE`, `_MLOG$_OLD_NEW` " +
			"from `$mlog$t_mlog_replace`",
	).Sort().Check(testkit.Rows(
		"1 10 100 R -1",
		"1 20 999 R 1",
		"2 20 200 R -1",
	))
}

func TestMaterializedViewLogWriteInsertIgnore(t *testing.T) {
	store := testkit.CreateMockStore(t)
	tk := testkit.NewTestKit(t, store)
	tk.MustExec("use test")

	tk.MustExec("drop table if exists t_mlog_insert_ignore")
	tk.MustExec("create table t_mlog_insert_ignore (id int primary key, uk int unique, v int)")
	tk.MustExec("insert into t_mlog_insert_ignore values (1,10,100)")
	tk.MustExec("create materialized view log on t_mlog_insert_ignore (id, uk, v)")

	// (1,11,111) conflicts on PK, (2,10,222) conflicts on unique index, only the last row is inserted.
	tk.MustExec(
		"insert ignore into t_mlog_insert_ignore values " +
			"(1,11,111), (2,10,222), (3,30,333)",
	)

	tk.MustQuery("select id, uk, v from t_mlog_insert_ignore order by id").Check(
		testkit.Rows("1 10 100", "3 30 333"),
	)
	tk.MustQuery(
		"select id, uk, v, `_MLOG$_DML_TYPE`, `_MLOG$_OLD_NEW` " +
			"from `$mlog$t_mlog_insert_ignore`",
	).Check(testkit.Rows(
		"3 30 333 I 1",
	))
}

func TestMaterializedViewLogWriteInsertOnDuplicateUpdate(t *testing.T) {
	store := testkit.CreateMockStore(t)
	tk := testkit.NewTestKit(t, store)
	tk.MustExec("use test")

	tk.MustExec("drop table if exists t_mlog_insert_dup")
	tk.MustExec("create table t_mlog_insert_dup (id int primary key, uk int unique, v int)")
	tk.MustExec("insert into t_mlog_insert_dup values (1,10,100)")
	tk.MustExec("create materialized view log on t_mlog_insert_dup (id, uk, v)")

	tk.MustExec(
		"insert into t_mlog_insert_dup values (1,10,101) " +
			"on duplicate key update v=values(v)",
	)

	tk.MustQuery("select id, uk, v from t_mlog_insert_dup order by id").Check(
		testkit.Rows("1 10 101"),
	)
	tk.MustQuery(
		"select id, uk, v, `_MLOG$_DML_TYPE`, `_MLOG$_OLD_NEW` " +
			"from `$mlog$t_mlog_insert_dup`",
	).Sort().Check(testkit.Rows(
		"1 10 100 U -1",
		"1 10 101 U 1",
	))
}

func TestMaterializedViewLogWriteLoadDataIgnore(t *testing.T) {
	store := testkit.CreateMockStore(t)
	tk := testkit.NewTestKit(t, store)
	tk.MustExec("use test")

	tk.MustExec("drop table if exists t_mlog_load_ignore")
	tk.MustExec("create table t_mlog_load_ignore (id int primary key, uk int unique, v int)")
	// Seed rows before creating mlog so that seed inserts won't be logged.
	tk.MustExec("insert into t_mlog_load_ignore values (1,10,100)")
	tk.MustExec("create materialized view log on t_mlog_load_ignore (id, uk, v)")

	data := "1\t11\t111\n2\t10\t222\n3\t30\t333\n"
	var readerBuilder executor.LoadDataReaderBuilder = func(_ string) (r io.ReadCloser, err error) {
		return mydump.NewStringReader(data), nil
	}
	tk.Session().(sessionctx.Context).SetValue(executor.LoadDataReaderBuilderKey, readerBuilder)

	tk.MustExec(
		"load data local infile '/tmp/nonexistence.csv' ignore " +
			"into table t_mlog_load_ignore (id, uk, v)",
	)

	tk.MustQuery("select id, uk, v from t_mlog_load_ignore order by id").Check(
		testkit.Rows("1 10 100", "3 30 333"),
	)
	tk.MustQuery(
		"select id, uk, v, `_MLOG$_DML_TYPE`, `_MLOG$_OLD_NEW` " +
			"from `$mlog$t_mlog_load_ignore`",
	).Check(testkit.Rows(
		"3 30 333 L 1",
	))
}

func TestMaterializedViewLogWriteLoadDataReplacePKAndUniqueConflict(t *testing.T) {
	store := testkit.CreateMockStore(t)
	tk := testkit.NewTestKit(t, store)
	tk.MustExec("use test")

	tk.MustExec("drop table if exists t_mlog_load_replace")
	tk.MustExec("create table t_mlog_load_replace (id int primary key, uk int unique, v int)")
	tk.MustExec("insert into t_mlog_load_replace values (1,10,100), (2,20,200)")
	tk.MustExec("create materialized view log on t_mlog_load_replace (id, uk, v)")

	data := "1\t20\t999\n"
	var readerBuilder executor.LoadDataReaderBuilder = func(_ string) (r io.ReadCloser, err error) {
		return mydump.NewStringReader(data), nil
	}
	tk.Session().(sessionctx.Context).SetValue(executor.LoadDataReaderBuilderKey, readerBuilder)

	tk.MustExec(
		"load data local infile '/tmp/nonexistence.csv' replace " +
			"into table t_mlog_load_replace (id, uk, v)",
	)

	tk.MustQuery("select id, uk, v from t_mlog_load_replace order by id").Check(
		testkit.Rows("1 20 999"),
	)
	tk.MustQuery(
		"select id, uk, v, `_MLOG$_DML_TYPE`, `_MLOG$_OLD_NEW` " +
			"from `$mlog$t_mlog_load_replace`",
	).Sort().Check(testkit.Rows(
		"1 10 100 L -1",
		"1 20 999 L 1",
		"2 20 200 L -1",
	))
}

func TestMaterializedViewLogWriteUpdateAndDelete(t *testing.T) {
	store := testkit.CreateMockStore(t)
	tk := testkit.NewTestKit(t, store)
	tk.MustExec("use test")

	tk.MustExec("drop table if exists t_mlog_update_delete")
	tk.MustExec("create table t_mlog_update_delete (id int primary key, uk int unique, v int)")
	tk.MustExec("insert into t_mlog_update_delete values (1,10,100)")
	tk.MustExec("create materialized view log on t_mlog_update_delete (id, uk, v)")

	tk.MustExec("update t_mlog_update_delete set v=101 where id=1")
	tk.MustQuery(
		"select id, uk, v, `_MLOG$_DML_TYPE`, `_MLOG$_OLD_NEW` " +
			"from `$mlog$t_mlog_update_delete`",
	).Sort().Check(testkit.Rows(
		"1 10 100 U -1",
		"1 10 101 U 1",
	))

	tk.MustExec("delete from t_mlog_update_delete where id=1")
	tk.MustQuery(
		"select id, uk, v, `_MLOG$_DML_TYPE`, `_MLOG$_OLD_NEW` " +
			"from `$mlog$t_mlog_update_delete`",
	).Sort().Check(testkit.Rows(
		"1 10 100 U -1",
		"1 10 101 D -1",
		"1 10 101 U 1",
	))
}

func TestMaterializedViewLogWriteSkipWhenUpdateUntrackedColumns(t *testing.T) {
	store := testkit.CreateMockStore(t)
	tk := testkit.NewTestKit(t, store)
	tk.MustExec("use test")

	tk.MustExec("drop table if exists t_mlog_update_skip")
	tk.MustExec("create table t_mlog_update_skip (id int primary key, v int, extra int)")
	tk.MustExec("insert into t_mlog_update_skip values (1,100,1000)")
	tk.MustExec("create materialized view log on t_mlog_update_skip (id, v)")

	tk.MustExec("update t_mlog_update_skip set extra=2000 where id=1")
	tk.MustQuery("select * from `$mlog$t_mlog_update_skip`").Check(testkit.Rows())

	tk.MustExec("update t_mlog_update_skip set v=101 where id=1")
	tk.MustQuery(
		"select id, v, `_MLOG$_DML_TYPE`, `_MLOG$_OLD_NEW` " +
			"from `$mlog$t_mlog_update_skip`",
	).Sort().Check(testkit.Rows(
		"1 100 U -1",
		"1 101 U 1",
	))
}

func TestMaterializedViewLogWritePartialColumnsMappingAndFilter(t *testing.T) {
	store := testkit.CreateMockStore(t)
	tk := testkit.NewTestKit(t, store)
	tk.MustExec("use test")

	tk.MustExec("drop table if exists t_mlog_partial")
	tk.MustExec("create table t_mlog_partial (id int primary key, a int, b int, c int)")
	// Track columns in a different order to verify mapping by column name.
	tk.MustExec("create materialized view log on t_mlog_partial (c, a)")

	tk.MustExec("insert into t_mlog_partial values (1,10,20,30)")
	tk.MustQuery(
		"select c, a, `_MLOG$_DML_TYPE`, `_MLOG$_OLD_NEW` " +
			"from `$mlog$t_mlog_partial`",
	).Check(testkit.Rows(
		"30 10 I 1",
	))

	tk.MustExec("delete from `$mlog$t_mlog_partial`")
	tk.MustExec("update t_mlog_partial set b=21 where id=1")
	tk.MustQuery("select * from `$mlog$t_mlog_partial`").Check(testkit.Rows())

	tk.MustExec("update t_mlog_partial set a=11 where id=1")
	tk.MustQuery(
		"select c, a, `_MLOG$_DML_TYPE`, `_MLOG$_OLD_NEW` " +
			"from `$mlog$t_mlog_partial`",
	).Sort().Check(testkit.Rows(
		"30 10 U -1",
		"30 11 U 1",
	))

	tk.MustExec("delete from `$mlog$t_mlog_partial`")
	tk.MustExec("update t_mlog_partial set c=31 where id=1")
	tk.MustQuery(
		"select c, a, `_MLOG$_DML_TYPE`, `_MLOG$_OLD_NEW` " +
			"from `$mlog$t_mlog_partial`",
	).Sort().Check(testkit.Rows(
		"30 11 U -1",
		"31 11 U 1",
	))
}

func TestMaterializedViewLogPartitionedTableNotSupported(t *testing.T) {
	store := testkit.CreateMockStore(t)
	tk := testkit.NewTestKit(t, store)
	tk.MustExec("use test")

	tk.MustExec("drop table if exists t_mlog_part")
	tk.MustExec(
		"create table t_mlog_part (id int, v int) " +
			"partition by range (id) (" +
			"partition p0 values less than (10)," +
			"partition p1 values less than (maxvalue)" +
			")",
	)
	tk.MustExec("create materialized view log on t_mlog_part (id, v)")

	tk.MustGetErrCode(
		"insert into t_mlog_part values (1,100)",
		mysql.ErrNotSupportedYet,
	)
}

func TestMaterializedViewLogWriteUpdatePrimaryKey(t *testing.T) {
	store := testkit.CreateMockStore(t)
	tk := testkit.NewTestKit(t, store)
	tk.MustExec("use test")

	tk.MustExec("drop table if exists t_mlog_update_pk")
	tk.MustExec("create table t_mlog_update_pk (id int primary key, v int)")
	tk.MustExec("insert into t_mlog_update_pk values (1, 100)")
	tk.MustExec("create materialized view log on t_mlog_update_pk (id, v)")

	// Updating the primary key triggers the handle-changed path:
	// RemoveRecord(old) + AddRecord(new, IsUpdate).
	tk.MustExec("update t_mlog_update_pk set id = 2 where id = 1")

	tk.MustQuery("select id, v from t_mlog_update_pk order by id").Check(
		testkit.Rows("2 100"),
	)
	tk.MustQuery(
		"select id, v, `_MLOG$_DML_TYPE`, `_MLOG$_OLD_NEW` " +
			"from `$mlog$t_mlog_update_pk`",
	).Sort().Check(testkit.Rows(
		"1 100 U -1",
		"2 100 U 1",
	))
}

func TestMaterializedViewLogWriteIODKUChangePrimaryKey(t *testing.T) {
	store := testkit.CreateMockStore(t)
	tk := testkit.NewTestKit(t, store)
	tk.MustExec("use test")

	tk.MustExec("drop table if exists t_mlog_iodku_pk")
	tk.MustExec("create table t_mlog_iodku_pk (id int primary key, uk int unique, v int)")
	tk.MustExec("insert into t_mlog_iodku_pk values (1, 10, 100)")
	tk.MustExec("create materialized view log on t_mlog_iodku_pk (id, uk, v)")

	// IODKU that changes the primary key triggers handle-changed path:
	// RemoveRecord(old) + AddRecord(new, IsUpdate), all under MLogDMLTypeInsert.
	tk.MustExec(
		"insert into t_mlog_iodku_pk values (1, 10, 200) " +
			"on duplicate key update id = 3, v = values(v)",
	)

	tk.MustQuery("select id, uk, v from t_mlog_iodku_pk order by id").Check(
		testkit.Rows("3 10 200"),
	)
	tk.MustQuery(
		"select id, uk, v, `_MLOG$_DML_TYPE`, `_MLOG$_OLD_NEW` " +
			"from `$mlog$t_mlog_iodku_pk`",
	).Sort().Check(testkit.Rows(
		"1 10 100 U -1",
		"3 10 200 U 1",
	))
}

func TestMaterializedViewLogWriteReplaceIdenticalRow(t *testing.T) {
	store := testkit.CreateMockStore(t)
	tk := testkit.NewTestKit(t, store)
	tk.MustExec("use test")

	tk.MustExec("drop table if exists t_mlog_replace_identical")
	tk.MustExec("create table t_mlog_replace_identical (id int primary key, v int)")
	tk.MustExec("insert into t_mlog_replace_identical values (1, 100)")
	tk.MustExec("create materialized view log on t_mlog_replace_identical (id, v)")

	// REPLACE with an identical row: executor skips RemoveRecord + AddRecord.
	tk.MustExec("replace into t_mlog_replace_identical values (1, 100)")

	// Mlog should be empty because the base table was not mutated.
	tk.MustQuery("select * from `$mlog$t_mlog_replace_identical`").Check(testkit.Rows())
}

