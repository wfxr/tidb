# Mlog vs Non-Unique Index: TiDB CPU Gap 汇报提纲

**用途**: 口头汇报 / 评审会讲述提纲  
**对应主报告**: `results/report-index-vs-mlog-tidb-cpu-analysis.md`

## 1. 开场一句话

这次调查的结论可以先一句话讲清楚：

> `mlog` 在当前实现下，额外走的是“对 mlog 表再做一次通用 `Table.AddRecord`”，而 non-unique index 额外走的是“index-specific 的轻量维护路径”，所以 `mlog` 的 TiDB CPU 更高。

## 2. 先讲 benchmark 现象

建议先给评审一个非常短的数据印象，只讲最有区分度的几项：

- 基线：TiDB CPU `35.8%`
- `+ non-unique index`：TiDB CPU `41.7%`，相对基线 `+16.5%`
- `+ mlog`：TiDB CPU `44.4%`，相对基线 `+24.0%`
- 但 TiKV CPU 两者接近：index `39.4%`，mlog `38.4%`
- 磁盘写入反而是 mlog 更少：index `16.73 KB/row`，mlog `13.71 KB/row`

口头解释建议：

“这组数据说明问题不在 TiKV 更忙，也不在 mlog 写得更多字节，而是在 TiDB 端执行路径更重。”

## 3. 再讲核心源码结论

这一段建议只讲最核心的两条链路，不要一开始就铺太多函数细节。

### 3.1 Non-unique index 的附加路径

```text
base TableCommon.AddRecord
  -> addIndices
    -> FetchValues
    -> index.create
      -> GenIndexKey / GenIndexValue
      -> MemBuffer.Set
```

口头说法：

“普通非唯一索引的增量维护，本质上是从已有行里投影出索引列，然后编码一个 index KV 写进 mem-buffer。它没有第二次进入通用表插入流程。”

### 3.2 Mlog 的附加路径

```text
wrapped table AddRecord
  -> base Table.AddRecord
  -> writeMLogRow
  -> mlog Table.AddRecord
```

口头说法：

“mlog 不只是维护一个附加结构，而是在基表写完后，再对 `$mlog$...` 表执行一次普通 `AddRecord`。这就是两者 CPU 模型不同的根本原因。”

## 4. 为什么“第二次 AddRecord”会更重

这里建议不要泛泛说“更复杂”，而是点名它多做了哪些类型的工作。

可以直接讲这几类：

1. row ID / handle 处理
2. 按列遍历和行编码
3. record key 生成
4. mem-buffer staging 和 assertion
5. statistics update

口头说法：

“`TableCommon.addRecord()` 不是一个只做 KV 拼装的小函数，而是一条完整的表写流水线。mlog 现在是把这条流水线在同一个用户事务里再走一遍，所以 TiDB CPU 更高。”

## 5. 为什么不是 payload 或 TiKV 编码问题

这一段是为了堵 reviewer 常见追问。

建议只讲两个判断：

1. 如果主因是 payload 更大，通常 TiKV CPU 或磁盘写入应该更高
2. 但实际数据是 mlog 的 TiKV CPU 没更高，磁盘写入还更低

口头说法：

“所以这个差异更像是 TiDB SQL / table layer 的执行路径成本，而不是存储字节数成本。”

## 6. Reviewer 常问点：`ReservedRowIDAlloc` 重不重

这一段建议单独拎出来，因为 reviewer 往往会盯着 `reset/restore` 那几行。

先给结论：

> `ReservedRowIDAlloc` 本身不是热点，它只是一个次级因子。

讲法可以分成两层：

### 6.1 直接开销很小

- `ReservedRowIDAlloc` 只有 `base` / `max` 两个字段
- `Current()` 就是读两个整数
- `Reset()` 就是写两个整数
- `Consume()` 就是比较加自增

口头说法：

“从实现看，这段就是几次整数读写和一个 `defer`，本身不可能解释那几个点的 TiDB CPU gap。”

### 6.2 间接影响存在，但不是主导项

- 基表可以通过 statement-level reservation 预留 rowid
- mlog 写入前必须临时清空这段 reservation，避免误用基表 rowid
- mlog 当前也没有独立 reservation 机制
- 但底层 rowid allocator 自带本地 cache，不是每行都要去做 metadata txn
- 本次 benchmark 还是 `batch_size=1`，reservation 的摊薄空间本来就很有限

口头说法：

“所以这块的影响更多是一个次级损失，不是主因。真正重的还是后面那次完整 `mlog AddRecord`。”

## 7. Reviewer 常问点：deep copy 能优化多少

先给结论：

> 可以优化，但收益有限，不会改变主结论。

建议讲法：

- `writeMLogRow()` 以前对 tracked datums 做 deep copy
- 这部分会增加对象分配和内存拷贝
- 但它只发生在“构造 mlogRow”这一步
- 真正的大头仍然在后面的 `t.mlog.AddRecord(...)`

口头说法：

“这块属于值得做的局部优化，但即便做掉，也只是削掉前置小段成本，无法把 mlog 拉平到 non-unique index 的成本曲线。”

## 8. 结尾怎么收

建议最后用 4 句收尾：

1. benchmark 里的 TiDB CPU gap 是真实且稳定的现象
2. 根因不是 payload，也不是 TiKV 更忙
3. 根因是当前实现里 mlog 额外走了一次通用表插入路径
4. 后续如果要继续优化，优先级应放在“收窄 mlog 写路径”，而不是继续抠局部小开销

## 9. 一页版结论

如果评审时间很短，可以只讲下面这段：

> 同样追踪 11 列，non-unique index 的额外工作是 “从 base row 投影索引列，再生成 index KV”；  
> mlog 的额外工作是 “基表写完后，再对 `$mlog$...` 表执行一次普通 `Table.AddRecord`”。  
> 后者会重复走 row ID、行编码、record key、assertion、statistics 等整套通用表写流程。  
> 所以 mlog 的 TiDB CPU 更高，是当前实现结构决定的，不是偶发抖动，也不是 `ReservedRowIDAlloc` 或 deep copy 这类局部细节单独造成的。

