**Date**: 2026-03-19

<callout emoji="💡" background-color="light-blue">

This v2 note updates the earlier attribution.

- Under **pessimistic auto-commit**, the extra TiDB CPU that mlog spends relative to a non-unique index is **mainly driven by the additional pessimistic lock path**
- When the same benchmark is switched to **optimistic** transactions, the mlog-vs-index TiDB CPU gap almost disappears
- In the merged CPU profile, the strongest hotspot shift is in `SendReqCtx`, with `mallocgc` moving in the same direction
- The generic table-insert path, row construction, and prewrite/commit work are still real, but they behave more like **background mlog overhead** than the main reason the pessimistic gap opens up

</callout>

## Why this update

The earlier analysis focused on code-path structure: index maintenance uses an index-specific fast path, while mlog performs an additional full-table insert into the `$mlog$...` table. That observation is still correct, and it still helps explain why mlog is not just another index write.

What changed is the quality of the attribution evidence. This round adds two things the earlier version did not have:

1. A transaction-mode control: baseline, index, and mlog are each tested in both pessimistic and optimistic modes
2. Merged CPU profiles from three TiDB nodes for all six cases

Together, they answer the narrower question more directly: why does mlog sit above index on TiDB CPU in pessimistic auto-commit?

With that broader evidence in place, the earlier "generic insert path is the main cause" framing becomes incomplete. The generic path still explains why mlog has overhead of its own. It does not, by itself, explain why the mlog-vs-index gap is pronounced in pessimistic mode and nearly gone in optimistic mode.

## Benchmark setup

- Base table: `bc_bet_records`
- Comparison target: 11-column non-unique index vs mlog tracking the same 11 columns
- Mlog mode: no shard
- `batch_size=1`
- 128 threads
- Rate limit: 18,000 rows/s
- 3 TiDB nodes, round-robin traffic

Case matrix:

| Case | Scenario | Transaction mode |
| --- | --- | --- |
| 1 | baseline | pessimistic |
| 2 | index | pessimistic |
| 3 | mlog-noshard | pessimistic |
| 4 | baseline | optimistic |
| 5 | index | optimistic |
| 6 | mlog-noshard | optimistic |

This note focuses on **TiDB CPU attribution**, not on giving a full performance report for every metric.

## The short version of what the data says

The cluster-level numbers already point in a clear direction:

| Mode | Baseline TiDB CPU | Index TiDB CPU | Mlog TiDB CPU | Mlog - Index gap | Index PessLock/s | Mlog PessLock/s |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| pessimistic | 36.0% | 41.5% (+15.3%) | 43.6% (+21.1%) | **+5.8pp** | 35,022 | 52,834 |
| optimistic | 30.3% | 35.7% (+17.8%) | 35.8% (+18.2%) | **+0.4pp** | 1 | 0 |

Three points stand out immediately:

- In pessimistic mode, mlog is meaningfully above index on TiDB CPU
- In optimistic mode, the same gap nearly vanishes
- The conspicuous request type that disappears with the gap is `kv_pessimistic_lock`

That does not prove that every remaining CPU second is caused by pessimistic locking. It does, however, make pessimistic locking the strongest first suspect even before we look at the profile.

The merged CPU profile tells the same story. The table below keeps only the functions most relevant to attribution, and all values are merged **cum time** across three TiDB nodes:

| Function (cum time) | BL-pess | Idx-pess | Mlog-pess | BL-opt | Idx-opt | Mlog-opt |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| Total samples | 1731s | 2038s | 2076s | 1494s | 1743s | 1696s |
| `handlePessimisticDML` | 255s | 315s | 260s | — | — | — |
| `pessimisticLockMutations` | 84s | 87s | 58s | — | — | — |
| `SendReqCtx` | 185s | 234s | 280s | 135s | 184s | 189s |
| `Prewrite.handleSingle` | 81s | 107s | 111s | 89s | 122s | 124s |
| `Commit.handleSingle` | 74s | 103s | 104s | 73s | 97s | 100s |
| `mallocgc` | 288s | 337s | 360s | 250s | 290s | 285s |
| `insertRows` | 139s | 195s | 167s | 135s | 183s | 160s |
| `buildInsert` | 175s | 188s | 186s | 165s | 173s | 174s |
| `startWorker.func1` | 171s | 234s | 332s | 178s | 241s | 248s |

Read together, these numbers separate the signals that move with transaction mode from the ones that remain relatively stable:

- `SendReqCtx` is the clearest hotspot shift: mlog sits **46s** above index in pessimistic mode, but only **5s** above it in optimistic mode
- `mallocgc` follows the same pattern, which strongly suggests extra request construction and serialization work around the pessimistic lock path
- `Prewrite.handleSingle`, `Commit.handleSingle`, `insertRows`, and `buildInsert` stay much closer across transaction modes, which makes them look more like persistent mlog overhead than the main reason the pessimistic gap opens up
- `pessimisticLockMutations` looks smaller for mlog at first glance, but that number is misleading on its own because a larger share of the lock work is shifted into worker goroutine stacks; the jump in `startWorker.func1` is the clue

### Flamegraph comparison

[Insert screenshot: pessimistic `index` flamegraph]

[Insert screenshot: pessimistic `mlog` flamegraph]

The two flamegraphs should tell a consistent visual story.

In the pessimistic **index** case, the profile is dominated by the expected shared write path: prewrite, commit, planning and execution overhead, allocation, and the normal worker-goroutine stacks. `SendReqCtx` is present, but it stays within the range we would expect from prewrite and commit traffic alone.

In the pessimistic **mlog** case, the overall shape is similar, but two areas widen much more noticeably. The first is the gRPC send path around `SendReqCtx`, which is the strongest profile-level signal for the extra pessimistic lock work. The second is the worker-side stack rooted at `startWorker.func1`, which matches the batch-dispatch behavior for the additional lock batches. The allocation-related area also becomes broader, which fits the higher `mallocgc` cum time.

Visually, the difference is not that mlog suddenly grows an entirely different execution engine. It is that the pessimistic mlog case adds a heavier lock-related layer on top of the familiar write path.

## Evidence chain

### The control experiment changes the answer

The simplest falsifiable question is this: if the main reason mlog sat above index were merely "mlog does a second generic table insert," why would changing only the transaction mode almost erase the mlog-vs-index gap? The generic insert path still exists in both pessimistic and optimistic transactions.

But that is not what the benchmark shows.

The TiDB CPU gap shrinks from **5.8 percentage points** in pessimistic mode to **0.4 percentage points** in optimistic mode. That is a large shift for a test where the schema, rate limit, row shape, and write amplification pattern are otherwise the same.

This does not mean the generic insert path has no cost. It means that generic insert cost is not the factor that best explains the **extra distance between mlog and index** under pessimistic auto-commit.

### The request counters point to pessimistic lock first

In pessimistic mode:

- Index stays roughly flat on `PessLock/s` relative to baseline
- Mlog jumps to **52,834 PessLock/s**, about **51%** above baseline

In optimistic mode:

- `PessLock/s` is essentially zero for all three cases; for attribution purposes, those residual values are better treated as background measurement noise than as a meaningful lock signal

At the same time:

- `Prewrite/s` remains elevated for both index and mlog in both transaction modes
- `Commit/s` remains elevated for both index and mlog in both transaction modes

That separates two classes of work:

- **Transaction-mode-sensitive work**: pessimistic lock RPCs
- **Transaction-mode-insensitive work**: prewrite, commit, and the rest of the write pipeline

The first class lines up with the TiDB CPU gap change. The second class mostly does not.

### The strongest profile signal is `SendReqCtx`

`SendReqCtx` is the shared send path for TiKV RPCs, including pessimistic lock, prewrite, and commit. In the merged profile, it is the single clearest hotspot shift.

Two comparisons matter most:

- In pessimistic mode, mlog is **46s** above index (`280s - 234s`)
- In optimistic mode, mlog is only **5s** above index (`189s - 184s`)

So when pessimistic locking disappears, roughly **41s** of the pessimistic mlog-vs-index `SendReqCtx` gap also disappears.

That is the cleanest attribution signal in the current data set.

It would be too strong to say that all 46s come from pessimistic lock RPCs. `SendReqCtx` still includes shared prewrite and commit traffic, and a small residual gap remains in optimistic mode. But the evidence supports a more careful statement:

> The bulk of the extra `SendReqCtx` cost that mlog pays over index in pessimistic mode is most consistent with the extra pessimistic lock path.

### `mallocgc` moves in the same direction

The memory-allocation signal is less direct than `SendReqCtx`, but it points the same way.

Relative to baseline:

- Pessimistic mlog: **+25.0%**
- Optimistic mlog: **+14.0%**

The simplest explanation is that pessimistic mlog performs extra request construction, buffering, and serialization work around the pessimistic lock RPC path. When that path goes away, the allocation hotspot falls back toward the index range.

### Why `pessimisticLockMutations` looks misleading

At first glance, one result looks contradictory:

- Mlog sends many more pessimistic lock RPCs
- Yet `pessimisticLockMutations` shows **lower** cum time than baseline or index

This is a stack-shape problem, not evidence that pessimistic locking is cheaper for mlog.

In client-go, pessimistic lock batches are sent differently depending on how many batches remain after the primary batch is handled. In the mlog case, the remaining lock batches are much more likely to be dispatched through worker goroutines. Once that happens, a large share of the CPU samples no longer sits under the caller stack rooted at `pessimisticLockMutations`; it moves under `startWorker.func1`.

That is exactly what the merged profile shows:

- `startWorker.func1` is **332s** for pessimistic mlog
- The same function is **248s** for optimistic mlog
- Baseline stays roughly flat (**171s** vs **178s**)

So the right reading is not "mlog spends less CPU in pessimistic locking." The right reading is "the pessimistic lock work is distributed differently in the call graph, and caller cum time alone is not enough."

## What still remains true from the earlier analysis

The previous write-up should not be read as "completely wrong." Several parts of it still hold, and they remain useful for understanding why mlog is not free. What changes in this revision is their weight in the final attribution.

The most useful way to think about the split is:

- **Pessimistic lock path** explains most of the *extra gap* between mlog and index in this benchmark
- **Generic insert and row-construction work** explain the *background cost* of maintaining mlog in either transaction mode

The easiest place to see that split is in the hotspots that stay relatively stable across pessimistic and optimistic modes:

| Function | Pessimistic Mlog vs Baseline | Optimistic Mlog vs Baseline |
| --- | ---: | ---: |
| `Prewrite.handleSingle` | +37.0% | +39.3% |
| `Commit.handleSingle` | +40.5% | +37.0% |
| `insertRows` | +20.1% | +18.5% |
| `buildInsert` | +6.3% | +5.5% |

These are exactly the kinds of costs we would expect from "mlog exists and writes extra data," but they are not specific to pessimistic locking.

### Generic table-insert path

The earlier document was right to call out that mlog is not maintained through the same lightweight path as a non-unique index.

Index maintenance mainly stays inside an index-specific flow: project values from the base row, encode index KV, and write it into the mem-buffer. Mlog, by contrast, materializes an additional row and inserts it into a separate physical mlog table. That second write goes through the generic table-insert pipeline, including handle or rowid allocation, row encoding, mem-buffer staging, record-key generation, assertion setup, and statistics updates.

That structural difference still matters. It explains why mlog has a real CPU cost even when the transaction mode changes, and it also explains why mlog should never be thought of as just another index append.

What it does **not** explain well enough on its own is the specific pattern we now observe in the benchmark: the mlog-vs-index CPU gap is clear in pessimistic mode and nearly gone in optimistic mode, even though the generic insert path exists in both. So this path remains a valid part of the story, just not the leading one.

### Datum deep copy

The earlier write-up was also right to flag datum copying during mlog row construction.

When the mlog row is built, tracked datum values are copied rather than merely referenced. For string or bytes values, decimals, and temporal types, that means real allocation and copy work. In a schema like this one, where several tracked columns fall into those categories, the effect is not theoretical.

This is a real source of mlog overhead, and it likely contributes to the persistent allocation pressure that remains visible even outside the pessimistic lock effect.

But it is still better understood as **row-construction cost** than as the main explanation for the pessimistic-mode gap relative to index. The same copying logic is still present when we switch to optimistic transactions, while the mlog-vs-index CPU gap almost disappears. That makes datum deep copy an important secondary factor, not the strongest attribution for the gap we are trying to explain here.

### ReservedRowIDAlloc

`ReservedRowIDAlloc` remains a reasonable thing to ask about, and it is worth keeping in the document because readers naturally notice it.

Before writing the mlog row, TiDB temporarily resets and later restores the base table's reserved rowid state. That logic is needed so the mlog insert does not accidentally consume row IDs reserved for the base table itself. On the surface, that per-row state switch looks suspicious enough to deserve investigation.

The current assessment still stands: under this benchmark, it is unlikely to be a first-order performance factor.

Its **direct** cost is tiny. The state being reset is just a small pair of integer fields, so the reset/restore sequence itself is only a few reads and writes.

Its **indirect** cost also appears limited. Once the statement-level reservation is cleared, the mlog insert may fall back to the underlying rowid allocator more often. But that allocator uses a local cache and expands its range in batches, so the common case is still a cheap local increment rather than a full allocator refill. At this write rate, refill frequency should remain low enough that it does not plausibly dominate TiDB CPU.

So this remains the right way to frame it: `ReservedRowIDAlloc` reset/restore is correctness-isolation logic with some cost, but the current data does not support it as a major contributor to the observed mlog-vs-index CPU gap.

<callout emoji="✅" background-color="light-green">

**Conclusion**

- The TiDB CPU gap between mlog and a non-unique index is **not best explained by the generic table-insert path alone**
- The **main contributor** to the pessimistic mlog-vs-index CPU gap is the additional pessimistic lock path
- The older structural analysis still matters, but in a narrower role: it explains why mlog has background overhead of its own, not why the gap against index opens up specifically in pessimistic mode
- There is also a clear engineering implication: if mlog can avoid entering the pessimistic lock path, the extra TiDB CPU should fall noticeably; the implementation approach and exact benefit are separate questions outside the scope of this note

</callout>
