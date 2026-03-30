# Mlog vs Non-Unique Index: Root Cause Analysis of TiDB CPU Difference

## TL;DR

Mlog consumes slightly more TiDB CPU than a non-unique index (44.4% vs 41.7%). Analysis shows the primary cause is **not** the extra allocation overhead from mlog lacking reserved row IDs, but rather the **inherent structural overhead of the mlog write path itself** — mlog writes are not lightweight index-style KV appends; instead, each mlog write performs a full `Table.AddRecord` on the `$mlog$...` table, including handle allocation, row encoding, mem-buffer staging, record key generation, and the entire generic table insert pipeline. Although `ReservedRowIDAlloc` reset/restore exists, the underlying allocator has a local cache, making the actual overhead negligible.

## Key Benchmark Data

Test conditions: `bc_bet_records` base table, 11-column non-unique index vs mlog tracking the same 11 columns, both no-shard, pessimistic auto-commit, `batch_size=1`, 128 threads / 18k rows/s.

| Metric | Baseline | + Index | + Mlog |
| --- | ---: | ---: | ---: |
| TiDB CPU | 35.8% | 41.7% (+16.5%) | 44.4% (+24.0%) |
| TiKV CPU | 26.3% | 39.4% | 38.4% |
| Disk KB/row | 9.57 | 16.73 | 13.71 |

Two key signals:

1. Mlog has higher TiDB CPU, but TiKV CPU is on par or even slightly lower
2. Mlog actually writes less data to disk than the index

In absolute terms, the TiDB CPU gap between the two is only 2.7 percentage points (44.4% vs 41.7%) — neither overhead is particularly large. What matters is not the magnitude of the gap itself, but the execution path difference it reveals — this rules out the possibility that the difference is caused by storage-layer payload size or TiKV-side encoding costs, pointing instead to **structural differences in the TiDB-side execution path itself**.

## Path Comparison

### Non-unique index: index-specific fast path

```
base TableCommon.AddRecord
  └─ addIndices
       └─ FetchValues        // r[ic.Offset] direct projection
       └─ GenIndexKey/Value  // encode index KV
       └─ MemBuffer.Set      // write
```

Index maintenance does not enter the generic table insert path; it simply projects from the existing row and encodes a single index entry ([`index.go`][index-create]).

### Mlog: second full table insert

```
mlogTable.AddRecord
  ├─ base Table.AddRecord    // normal base table insert
  ├─ writeMLogRow            // construct mlog row (datum projection)
  └─ mlog Table.AddRecord    // second generic table insert
```

Mlog DDL creates a separate physical table (tracked columns + `_MLOG$_DML_TYPE` / `_MLOG$_OLD_NEW`). During writes, the executor layer wraps the base table as an [`mlogTable`][builder-wrap], whose [`AddRecord`][mlog-addrecord] calls [`Table.AddRecord`][mlog-write-addrecord] on the mlog table after the base table insert completes.

Both writes complete within the same transaction — no extra 2PC round. The additional CPU mainly comes from the generic write pipeline in the table layer.

### What mlog does beyond what index does

The additional work for an index is concentrated in the `FetchValues → GenIndexKey/Value → write mem-buffer` index-specific path, which does not enter the generic table write path. In practice, it also includes assertion logic on the index side, but the overall cost is still significantly less than a full `Table.AddRecord`. Mlog's `Table.AddRecord` enters the `TableCommon.addRecord` generic pipeline, with each stage participating as follows:

| Stage | Description | Source |
| --- | --- | --- |
| Handle / RowID allocation | `AllocHandle` → `AllocHandleIDs(1)` | [`tables.go:763`][tables-reserve] |
| Initialize `encodeRowBuffer` | Allocate buffer for row encoding | [`tables.go:787`][tables-encode-buf] |
| Establish mem-buffer staging | `txn.GetMemBuffer().Staging()` | [`tables.go:789`][tables-staging] |
| Iterate over writable columns | `checkDataForModifyColumn`, column state checks, `canSkip`, `AddColVal` | [`tables.go:792-846`][tables-col-iter] |
| Row-level constraint check | Row-level constraint validation (no-op, mlog has no constraints defined) | [`tables.go:848`][tables-constraint] |
| Dup key check | Check if record key is duplicate (skipped, mlogOpts sets `DupKeyCheckSkip`) | [`tables.go:853`][tables-dup-check] |
| Generate record key | `t.RecordKey(recordID)` | [`tables.go:851`][tables-record-key] |
| Encode full row and write to mem-buffer | `WriteMemBufferEncoded` | [`tables.go:887`][tables-encode-write] |
| Set assertion | `SetAssertNotExist` | [`tables.go:903`][tables-assertion] |
| Maintain indexes | `addIndices` (no-op, mlog has no index definitions) | [`tables.go:913`][tables-addindices] |
| Update statistics | `UpdatePhysicalTableDelta` | [`tables.go:929`][tables-stats] |

Index mainly projects from the existing row and encodes index KV; mlog needs to go through the entire generic table insert pipeline above. This is the structural source of the TiDB CPU difference.

## Secondary Factors

### Datum Deep Copy

[`writeMLogRow()`][mlog-writemlogrow] calls [`Datum.Copy()`][mlog-deep-copy] for each tracked datum, which produces real memory allocations and copies for string/bytes, `MyDecimal`, and `MysqlTime` types. Among the 11 tracked columns in this benchmark, there are 3 string columns, 3 decimal columns, and 1 date column — all affected.

However, this only affects the mlog row construction phase and does not change the heavier [`Table.AddRecord`][mlog-write-addrecord] pipeline that follows. Therefore, even if deep copy is optimized away, it cannot bring mlog down to the same cost curve as a non-unique index.

### ReservedRowIDAlloc

Before mlog writes, the base table's [`ReservedRowIDAlloc`][mlog-rowid-reset] is temporarily reset to prevent accidentally consuming row IDs reserved for the base table:

```go
base, maxv := alloc.Current()
defer alloc.Reset(base, maxv)
alloc.Reset(0, 0)
```

This logic appears to add a state switch per row, but it is not a major performance factor for the following reasons:

**Direct overhead is minimal.** [`ReservedRowIDAlloc`][stmtctx-alloc] has only two `int64` fields (`base`/`max`). `Current()` is two reads, `Reset()` is two writes, `Consume()` is a comparison plus increment — all constant-time integer operations.

**Indirect impact is also limited.** After reset, mlog's `AllocHandle` cannot `Consume()` from the base table's statement-level reservation and must fall through to the underlying rowid allocator's [`Alloc()`][autoid-alloc]. But the underlying allocator has a local cache ([`autoid.go:893`][autoid-cache]), and `Alloc(..., 1)` preferentially draws from the local range `[base, end]`, only calling `kv.RunInNewTxn()` to expand a new segment when the range is exhausted. The batch size for each expansion is dynamically adjusted by [`NextStep`][autoid-nextstep] — targeting approximately 10 seconds of IDs per batch (`defaultConsumeTime`), with the result clamped to `[30000, 2000000]`:

```go
// autoid.go:560
func NextStep(curStep int64, consumeDur time.Duration) int64 {
	consumeRate := defaultConsumeTime.Seconds() / consumeDur.Seconds() // 10s / actual consumption duration
	res := int64(float64(curStep) * consumeRate)
	if res < minStep { return minStep }  // 30000
	if res > maxStep { return maxStep }  // 2000000
	return res
}
```

Taking this benchmark's 18k rows/s as an example, the initial `minStep=30000` is consumed in about 1.7 seconds, the next batch expands to ~176k, and after a few rounds it stabilizes at roughly 10 seconds per batch. Therefore, the vast majority of `Alloc` calls are just local integer increments, and the refill frequency is extremely low.

Overall, `ReservedRowIDAlloc` reset/restore is state isolation logic required for correctness, but under the current benchmark conditions it does not constitute an observable performance difference.

## Conclusion

The TiDB CPU gap of `+24.0%` (mlog) vs `+16.5%` (index) in the benchmark is highly consistent with the current implementation structure: index takes the index-specific fast path (projection + index KV encoding), while mlog takes the second generic table insert path (the full [`TableCommon.addRecord`][tables-addrecord] pipeline). Local optimizations (eliminating deep copy, optimizing rowid alloc) can yield marginal improvements, but they are unlikely to change this structural difference.

## Key Source Code Entry Points

| Location | Description |
| --- | --- |
| [`pkg/executor/builder.go:2936`][builder-wrap] | `wrapTableWithMLogIfExists` |
| [`pkg/table/tables/mview_log.go:101`][mlog-wrap] | `WrapTableWithMaterializedViewLog` |
| [`pkg/table/tables/mview_log.go:192`][mlog-addrecord] | `mlogTable.AddRecord` |
| [`pkg/table/tables/mview_log.go:337`][mlog-writemlogrow] | `writeMLogRow` + second table insert |
| [`pkg/table/tables/tables.go:697`][tables-AddRecord] | `TableCommon.AddRecord` (public) |
| [`pkg/table/tables/tables.go:703`][tables-addrecord] | `TableCommon.addRecord` (private, actual pipeline) |
| [`pkg/table/tables/tables.go:913`][tables-addindices] | `addIndices` |
| [`pkg/table/tables/index.go:194`][index-create] | `index.create` |
| [`pkg/ddl/executor.go:1047`][ddl-mlog] | `CreateMaterializedViewLog` |

<!-- GitHub permanent links (commit 85f88a3ff5) -->
[builder-wrap]: https://github.com/pingcap/tidb/blob/85f88a3ff5b33c566c40027f60bf455aee583159/pkg/executor/builder.go#L2936
[mlog-wrap]: https://github.com/pingcap/tidb/blob/85f88a3ff5b33c566c40027f60bf455aee583159/pkg/table/tables/mview_log.go#L101
[mlog-addrecord]: https://github.com/pingcap/tidb/blob/85f88a3ff5b33c566c40027f60bf455aee583159/pkg/table/tables/mview_log.go#L192
[mlog-writemlogrow]: https://github.com/pingcap/tidb/blob/85f88a3ff5b33c566c40027f60bf455aee583159/pkg/table/tables/mview_log.go#L337
[mlog-deep-copy]: https://github.com/pingcap/tidb/blob/85f88a3ff5b33c566c40027f60bf455aee583159/pkg/table/tables/mview_log.go#L354-L356
[mlog-rowid-reset]: https://github.com/pingcap/tidb/blob/85f88a3ff5b33c566c40027f60bf455aee583159/pkg/table/tables/mview_log.go#L360-L367
[mlog-write-addrecord]: https://github.com/pingcap/tidb/blob/85f88a3ff5b33c566c40027f60bf455aee583159/pkg/table/tables/mview_log.go#L369
[tables-AddRecord]: https://github.com/pingcap/tidb/blob/85f88a3ff5b33c566c40027f60bf455aee583159/pkg/table/tables/tables.go#L697
[tables-addrecord]: https://github.com/pingcap/tidb/blob/85f88a3ff5b33c566c40027f60bf455aee583159/pkg/table/tables/tables.go#L703
[tables-reserve]: https://github.com/pingcap/tidb/blob/85f88a3ff5b33c566c40027f60bf455aee583159/pkg/table/tables/tables.go#L763
[tables-encode-buf]: https://github.com/pingcap/tidb/blob/85f88a3ff5b33c566c40027f60bf455aee583159/pkg/table/tables/tables.go#L787
[tables-staging]: https://github.com/pingcap/tidb/blob/85f88a3ff5b33c566c40027f60bf455aee583159/pkg/table/tables/tables.go#L789
[tables-col-iter]: https://github.com/pingcap/tidb/blob/85f88a3ff5b33c566c40027f60bf455aee583159/pkg/table/tables/tables.go#L792-L846
[tables-constraint]: https://github.com/pingcap/tidb/blob/85f88a3ff5b33c566c40027f60bf455aee583159/pkg/table/tables/tables.go#L848
[tables-record-key]: https://github.com/pingcap/tidb/blob/85f88a3ff5b33c566c40027f60bf455aee583159/pkg/table/tables/tables.go#L851
[tables-encode-write]: https://github.com/pingcap/tidb/blob/85f88a3ff5b33c566c40027f60bf455aee583159/pkg/table/tables/tables.go#L887
[tables-dup-check]: https://github.com/pingcap/tidb/blob/85f88a3ff5b33c566c40027f60bf455aee583159/pkg/table/tables/tables.go#L853-L877
[tables-assertion]: https://github.com/pingcap/tidb/blob/85f88a3ff5b33c566c40027f60bf455aee583159/pkg/table/tables/tables.go#L903-L910
[tables-stats]: https://github.com/pingcap/tidb/blob/85f88a3ff5b33c566c40027f60bf455aee583159/pkg/table/tables/tables.go#L929-L931
[tables-addindices]: https://github.com/pingcap/tidb/blob/85f88a3ff5b33c566c40027f60bf455aee583159/pkg/table/tables/tables.go#L913
[index-create]: https://github.com/pingcap/tidb/blob/85f88a3ff5b33c566c40027f60bf455aee583159/pkg/table/tables/index.go#L194
[ddl-mlog]: https://github.com/pingcap/tidb/blob/85f88a3ff5b33c566c40027f60bf455aee583159/pkg/ddl/executor.go#L1047
[stmtctx-alloc]: https://github.com/pingcap/tidb/blob/85f88a3ff5b33c566c40027f60bf455aee583159/pkg/sessionctx/stmtctx/stmtctx.go#L110-L142
[autoid-alloc]: https://github.com/pingcap/tidb/blob/85f88a3ff5b33c566c40027f60bf455aee583159/pkg/meta/autoid/autoid.go#L712
[autoid-cache]: https://github.com/pingcap/tidb/blob/85f88a3ff5b33c566c40027f60bf455aee583159/pkg/meta/autoid/autoid.go#L893
[autoid-nextstep]: https://github.com/pingcap/tidb/blob/85f88a3ff5b33c566c40027f60bf455aee583159/pkg/meta/autoid/autoid.go#L560-L579
