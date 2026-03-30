**日期**: 2026-03-19

## TL;DR

我们之前的写稿强调了 mlog 路径上额外的通用插入工作。这部分成本仍然存在，但在当前这组 benchmark 中，它看起来已经不是解释 **mlog 与非唯一索引之间 TiDB CPU 差距** 的最佳答案。

这一轮新增了事务模式对照实验，并合并了全部六个场景的 TiDB CPU profile。综合来看，新的证据指向了不同的主要结论：

- 在 **悲观 auto-commit** 下，mlog 相比 index 多出来的大部分 TiDB CPU，**主要由额外的悲观锁路径驱动**
- 当同一组 benchmark 切换到 **乐观** 事务时，mlog 与 index 之间那部分额外 CPU 距离几乎消失
- 在合并后的 pprof 中，最明显的热点变化出现在 `SendReqCtx`，`mallocgc` 也沿着相同方向变化；两者都与额外的 `kv_pessimistic_lock` RPC 相一致
- 通用插入路径、行构造以及 prewrite/commit 的开销依然真实存在，但它们更像是 **mlog 的背景性开销**，而不是悲观模式下差距拉开的主要原因

简言之，mlog 相比非唯一索引确实会引入额外工作。但在这组 benchmark 中，真正让它明显高于 index 的，主要是悲观锁路径，而不只是通用表插入路径本身。

## 为什么需要这次更新

之前的分析聚焦于代码路径结构：index 维护走的是 index-specific fast path，而 mlog 会向 `$mlog$...` 表额外执行一次完整的表插入。这个观察依然正确，也依然有助于解释为什么 mlog 不能被简单看成另一种 index 写入。

变化的是归因证据的力度。这一轮补上了旧版本没有的两样东西：

1. **事务模式对照**：baseline / index / mlog 在悲观和乐观两种模式下都各跑了一遍
2. **三台 TiDB 节点合并后的 CPU profile**：六个场景全部都有

把这两样证据放在一起后，我们就能更直接地回答那个真正关心、也更具体的问题：**为什么在悲观 auto-commit 下，mlog 的 TiDB CPU 会高于 index？**

在这些证据补齐之后，之前“通用插入路径是主要原因”的表述就显得不够完整了。通用路径仍然解释了为什么 mlog 本身会有额外开销，但它单独并不足以解释：为什么 mlog 与 index 之间的额外差距在悲观模式下很明显，而切到乐观模式后几乎消失。

## Benchmark 设置

这组 benchmark 矩阵刻意保持得很简单：

| Case | 场景 | 事务模式 |
| --- | --- | --- |
| 1 | baseline | pessimistic |
| 2 | index | pessimistic |
| 3 | mlog-noshard | pessimistic |
| 4 | baseline | optimistic |
| 5 | index | optimistic |
| 6 | mlog-noshard | optimistic |

共同条件：

- 基表：`bc_bet_records`
- 11 列非唯一索引 vs 追踪相同 11 列的 mlog
- mlog 表不开 shard
- `batch_size=1`
- 128 threads
- 限速：18,000 rows/s
- 3 台 TiDB 节点，流量轮询打入

这篇说明聚焦于 **TiDB CPU 归因**，不是为了给所有指标写一份完整性能报告。

## 数据给出的简明结论

集群层面的数字已经很明确。为了把 mlog-vs-index 差距里最有用的两种定义区分开来，下面这张表同时给出了原始 TiDB CPU 百分点差值，以及相对 baseline 的增幅差值。

| 模式 | Baseline TiDB CPU | Index TiDB CPU | Mlog TiDB CPU | 原始 CPU 差值 | 相对 Baseline 的增幅差值 | Index PessLock/s | Mlog PessLock/s |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| pessimistic | 36.0% | 41.5% (+15.3%) | 43.6% (+21.1%) | **+2.1pp** | **+5.8pp** | 35,022 | 52,834 |
| optimistic | 30.3% | 35.7% (+17.8%) | 35.8% (+18.2%) | **+0.1pp** | **+0.4pp** | 1 | 0 |

这两个 gap 指标回答的是略有不同的问题。原始 CPU 差值看的是 mlog 在绝对 TiDB CPU 使用量上比 index 高出多少；增幅差值看的是在对应 baseline 之上，mlog 比 index 额外带来了多少相对开销。后文的归因论证主要依赖第二种视角。

立刻能看到三点：

- 在悲观模式下，mlog 在 TiDB CPU 上仍然明显高于 index
- 在乐观模式下，这部分额外距离几乎消失
- 在写路径 RPC 计数器里，`kv_pessimistic_lock` 是那个会随着差距一起从明显信号掉到接近零的指标

再看一张简化后的 RPC 计数器表，把 baseline 对照更明确地摆出来：

| 模式 | 计数器 | Baseline | Index | Mlog |
| --- | --- | ---: | ---: | ---: |
| pessimistic | `PessLock/s` | 34,913 | 35,022 | 52,834 |
| pessimistic | `Prewrite/s` | 34,917 | 49,853 | 52,840 |
| pessimistic | `Commit/s` | 33,863 | 48,914 | 52,837 |
| optimistic | `PessLock/s` | 16 | 1 | 0 |
| optimistic | `Prewrite/s` | 34,864 | 50,116 | 52,899 |
| optimistic | `Commit/s` | 33,766 | 49,416 | 52,896 |

这并不能证明所有剩余的 CPU 秒数都来自悲观锁，但它确实让悲观锁成为最值得优先怀疑的对象。

下一个问题是：合并后的 profile 能不能在热点层面隔离出同样那种“随事务模式变化”的偏移？答案基本是可以的。下面这张表只保留了最和归因相关的函数，所有数值都是三台 TiDB 节点合并后的 **cum time**：

| 函数（cum time） | BL-pess | Idx-pess | Mlog-pess | BL-opt | Idx-opt | Mlog-opt |
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

这张表有助于把会随事务模式一起变化的信号，与不会随之变化的信号区分开来：

- `SendReqCtx` 是最清晰的热点偏移：在悲观模式下，mlog 比 index 高 **46s**；而在乐观模式下只高 **5s**
- `mallocgc` 沿着同样的方向变化，这很像是悲观锁路径周围额外的请求构造与序列化工作
- `Prewrite.handleSingle` 和 `Commit.handleSingle` 没有呈现出同样那种对事务模式敏感的偏移
- 插入相关函数只看原始 cum-time 表并不好读；更有信息量的视角是它们相对 baseline 的增幅，而这个增幅在悲观和乐观两种模式下都比较接近
- `pessimisticLockMutations` 乍看之下在 mlog 里反而更小，但这个数字单独看会误导，因为更大一部分锁相关工作被转移到了 worker goroutine 栈里；`startWorker.func1` 的跳升就是线索

综合来看，这些非锁热点更像是 mlog 的持续性背景开销，而不是悲观模式下 mlog 与 index 差距拉大的主要原因。

### 火焰图对比

下面这两张截图最适合围绕以 `startWorker.func1` 为根的 worker 侧栈来阅读，因为额外的锁相关分支在那里最清晰。

（火焰图截图请参考英文 v2 文档中的对应图片：[Mlog vs Non-Unique Index: TiDB CPU Difference Root Cause Analysis v2](https://pingcap.feishu.cn/docx/PMYpdBhEgoXsNDxR3Zoc9CbWnCd)）

把两张图并排来看，它们共同支持同一个判断。

在悲观 **index** 场景下，这个 worker 侧视角仍然主要由共享写路径主导：可见分支基本都是预期中的 prewrite 和 commit 路径，`SendReqCtx` 虽然也在，但没有异常地展开。

在悲观 **mlog** 场景下，同一片区域就明显更重了。`actionPessimisticLock.handleSingleBatch` 这条锁相关分支在 `startWorker.func1` 下更突出，`SendReqCtx` 周围的发送路径也更宽。

因此，这里的视觉差异并不是说 mlog 引入了一套完全不同的执行模型，而是在同一条熟悉的 worker 侧写路径上，又叠加了一层更重的锁相关开销。

## 证据链

### 1. 控制实验改变了答案

一个简单而可证伪的检查方法，是把其他条件都固定，只改变事务模式。如果 mlog 高于 index 的主要原因仅仅是它“多做了一次通用表插入”，那么我们不应该预期：仅靠切换事务模式这一件事，就能几乎抹平 **mlog 与 index 之间的额外差距**。因为通用插入路径在悲观和乐观事务中都同样存在。

但 benchmark 展现的并不是这样。

mlog-vs-index 相对 baseline 的 TiDB CPU 增幅差值，从悲观模式下的 **5.8 个百分点** 降到了乐观模式下的 **0.4 个百分点**。即便只看原始 TiDB CPU 百分点差值，也从 **2.1pp** 掉到了 **0.1pp**。对于一组 schema、限速、行形状和写放大模式都保持不变的实验来说，这个变化已经很大了。

这并不意味着通用插入路径没有成本，而是说：通用插入成本并不是解释 **悲观 auto-commit 下 mlog 与 index 额外差距** 的最佳因素。

### 2. 请求计数器首先指向悲观锁

从计数器视角看，最先跳出来的就是悲观锁。在悲观模式下，index 的 `PessLock/s` 与 baseline 基本持平（35,022 vs 34,913，也就是 **+0.3%**），而 mlog 上升到了 **52,834**（**+51.3%**）。在乐观模式下，`PessLock/s` 在三个场景里都接近于零（**16 / 1 / 0**）。就归因而言，这些值可以视为零，更适合被看作背景测量噪音，而不是有意义的锁信号。

与此同时：

- `Prewrite/s` 在两种事务模式下，对 index 和 mlog 都仍然显著高于 baseline
- `Commit/s` 在两种事务模式下，对 index 和 mlog 也都仍然显著高于 baseline

这很重要，因为它把工作分成了两类：

- **对事务模式敏感的工作**：悲观锁 RPC
- **对事务模式不敏感的工作**：prewrite、commit，以及写流水线的其余部分

第一类与 mlog-vs-index 的 TiDB CPU 增幅差值变化是对齐的；第二类大体不是。

### 3. 最强的 pprof 信号来自 `SendReqCtx`

`SendReqCtx` 是 TiKV RPC 的共享发送路径，悲观锁、prewrite 和 commit 都会经过这里。在合并后的 profile 里，它是最明显的热点偏移。

最有信息量的是两组对比：

- 在悲观模式下，mlog 比 index 高 **46s**（`280s - 234s`）
- 在乐观模式下，mlog 只比 index 高 **5s**（`189s - 184s`）

因此，当悲观锁消失时，悲观 mlog 相比 index 在 `SendReqCtx` 上多出来的大约 **41s** 也随之消失了。

这是当前数据集中最干净的归因信号。

如果直接说这 46s 全都来自悲观锁 RPC，会说得过头。`SendReqCtx` 里仍然包含共享的 prewrite 和 commit 流量，而且在乐观模式下也还残留着一个小 gap。但证据确实强烈支持一个更谨慎的表述：

> 在悲观模式下，mlog 相比 index 多付出的 `SendReqCtx` 成本，其大部分最符合“额外的悲观锁路径”这一解释。

### 4. `mallocgc` 也指向同样的结论

内存分配信号沿着同样的方向变化。相对 baseline：

- 悲观 mlog：**+25.0%**
- 乐观 mlog：**+14.0%**

这个信号没有 `SendReqCtx` 那么直接，但它依然指向同一个方向。最简单的解释是：悲观 mlog 在悲观锁 RPC 路径周围做了额外的请求构造、buffer 组织和序列化工作。当这条路径消失后，分配热点也随之回落到更接近 index 的范围。

### 5. 为什么 `pessimisticLockMutations` 看起来会误导人

乍看之下，有一个结果似乎自相矛盾：

- Mlog 发送了更多的悲观锁 RPC
- 但在 pprof 里，`pessimisticLockMutations` 的 cum time 却比 baseline 或 index 更低

这其实是调用栈形状的问题，并不意味着 mlog 的悲观锁更便宜。

在 `client-go` 里，悲观锁 batch 并不总是停留在同一条 caller stack 上：

```text
doActionOnGroupMutations()
  -> send primary batch synchronously
  -> forget primary
  -> dispatch remaining batches
     -> if one batch remains: stay on the synchronous path
     -> if multiple batches remain: fork worker goroutines
```

这里的区别很关键。在 baseline 和 index 场景里，剩余的悲观锁工作更容易继续留在同步路径里，所以更多 CPU sample 会继续挂在 `pessimisticLockMutations` 下面。而在 mlog 场景里，多出来的 lock key 会让剩余锁工作更容易被分发到 worker goroutine 上，因此更大一部分 sample 会转移到 `startWorker.func1` 之下。这就是为什么在这个对比里，单看 `pessimisticLockMutations` 的 caller cum time 会误导人。

合并后的 profile 与这个解释是一致的：

- 对悲观 mlog，`startWorker.func1` 是 **332s**
- 同一个函数在乐观 mlog 中是 **248s**
- baseline 基本不变（**171s** vs **178s**）

因此，正确的解读不是“mlog 在悲观锁上花的 CPU 更少”，而是：悲观锁相关工作在调用图中的分布方式变了，单靠 caller cum time 已经不够看清问题。

## 之前分析中仍然成立的部分

之前的分析仍然抓住了 mlog 的几类真实开销。变化的不是这些成本存不存在，而是它们在最终归因里应该占多大权重。

更合适的理解方式是：

- **悲观锁路径** 解释了这组 benchmark 里 mlog 相比 index 的大部分 **额外距离**
- **通用插入和行构造工作** 解释了无论在哪种事务模式下，维护 mlog 都会带来的 **背景性开销**

profile 数据支持这种拆分。在相对 baseline 的视角下，这种稳定性更容易看出来：

| 函数 | 悲观 Mlog vs Baseline | 乐观 Mlog vs Baseline |
| --- | ---: | ---: |
| `Prewrite.handleSingle` | +37.0% | +39.3% |
| `Commit.handleSingle` | +40.5% | +37.0% |
| `insertRows` | +20.1% | +18.5% |
| `buildInsert` | +6.3% | +5.5% |

这些恰恰就是那种“mlog 存在且会多写一份数据”所应有的成本，但它们并不是悲观锁特有的。

### 通用表插入路径

之前的文稿指出：mlog 的维护并不走和非唯一索引相同的轻量路径。这个判断依然成立。

index 维护主要停留在 index-specific flow 内：从基表行里投影值，编码 index KV，再把它写进 mem-buffer。相比之下，mlog 会构造一行额外记录，并把它插入一张独立的物理 mlog 表。这第二次写入会走完整的通用表插入流水线，包括 handle 或 rowid 分配、行编码、mem-buffer staging、record-key 生成、assertion 设置以及统计信息更新。

这个结构性差异依然重要。它解释了为什么即使事务模式发生变化，mlog 仍然会有真实的 CPU 成本；它也解释了为什么 mlog 永远不应该被理解成“只是又多了一条 index append”。

但单靠这一点，还不足以解释我们现在看到的特定模式：尽管通用插入路径在两种事务模式里都存在，mlog 与 index 之间的额外 CPU 距离却只在悲观模式下明显，在乐观模式下几乎消失。所以，这条路径仍然是故事的一部分，只是不是最主导的那部分。

### Datum 深拷贝

之前的文稿也正确指出了 mlog 行构造过程中 datum 拷贝的问题。

构造 mlog 行时，被追踪的 datum 值会被拷贝，而不是只保留引用。对于 string / bytes、decimal 和 temporal 这几类值来说，这意味着真实的内存分配与拷贝工作。在像这组 schema 这样的场景里，多个被追踪列都落在这些类型里，因此这个影响绝不是理论上的。

这是 mlog 的一项真实开销来源，也很可能解释了即便抛开悲观锁效应后依然可见的那部分持续分配压力。

但更合适的理解方式，仍然是把它看成 **行构造成本**，而不是解释悲观模式下 mlog 相比 index 距离拉开的主要原因。因为切换到乐观事务后，这套拷贝逻辑仍然完整存在，可 mlog 与 index 之间那部分额外 CPU 距离却几乎消失了。这说明 datum 深拷贝是一个重要的次级因素，但不是这里最强的归因解释。

### `ReservedRowIDAlloc`

`ReservedRowIDAlloc` 值得单独讨论，因为读者很容易注意到它；但当前判断仍然是：在这组 benchmark 下，它不太可能是一阶性能因素。

在写入 mlog 行之前，TiDB 会临时重置、随后再恢复基表的 reserved rowid 状态。这样做是为了避免 mlog 插入误用那些原本为基表本身预留的 row ID。从表面上看，这种逐行状态切换确实可疑，值得单独检查。

它的 **直接** 成本很小。被重置的状态本质上只是一对整数，因此 reset/restore 本身也只是少量读写操作。

它的 **间接** 成本看起来也比较有限。statement 级别的 reservation 被清掉之后，mlog 插入可能会更频繁地回退到底层的 rowid allocator。但这个 allocator 自带本地 cache，并且会按批扩展自己的区间，所以常见情况仍然只是一次便宜的本地自增，而不是每次都去做一次完整的 allocator refill。以当前这组写入速率来看，refill 的频率仍然低到不足以 plausibly dominate TiDB CPU。

因此，这里更准确的表述依然是：`ReservedRowIDAlloc` 的 reset/restore 是为了保证正确性而做的状态隔离逻辑，它当然不是零成本，但当前数据并不支持把它视为 mlog-vs-index CPU 差异中的主要贡献项。

## 结论

- 最新证据表明，mlog 与非唯一索引之间那部分额外的 TiDB CPU 距离，**并不能最好地由通用表插入路径单独解释**。那条路径确实存在，但它不是悲观 auto-commit 下差距拉大的主要原因。
- **主要贡献因素** 是额外的悲观锁路径。事务模式对照、`PessLock/s` 计数器、`SendReqCtx` 的偏移、`mallocgc` 的偏移，以及 worker goroutine 栈上的变化，都指向同一个方向。
- 之前的结构性分析仍然重要，但它在这版中的角色更窄：它解释了为什么 mlog 自身会有非零背景开销，而不是解释为什么它与 index 之间的距离会在悲观模式下特意拉开。
- 工程上的启示也很直接：如果 mlog 能绕开悲观锁路径，额外的 TiDB CPU 很可能会明显下降。至于具体实现方案，以及收益究竟有多大，则是这篇说明之外的另一个问题。