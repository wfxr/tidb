# SELECT ... FOR UPDATE 对 mview/mlog 的锁行为验证

## 背景

TiDB 已通过 `CheckMViewUpdatable` 拦截了对 mview 和 mlog 表的 DML（INSERT / UPDATE / DELETE / REPLACE / LOAD / IMPORT），但 `SELECT ... FOR UPDATE` 是否也应被拦截？用户持有的行锁是否会阻塞 REFRESH？阻塞后数据是否正确？

## 测试环境

- TiDB: v8.5.4 feature/release-8.5-materialized-view 分支
- 部署: tiup playground (1 TiDB + 1 TiKV + 1 PD + 1 TiFlash)
- 自动化脚本: `bench/mlog-perf/test_mview_lock.sh`

## 测试结果

| # | 用户操作 | 被测操作 | 是否阻塞 | 等待时间 | 结果正确 |
|---|---------|----------|---------|---------|---------|
| 1 | `SELECT * FROM mview FOR UPDATE` | COMPLETE refresh | 阻塞 | ~8s (等锁释放) | 正确 |
| 2 | `SELECT * FROM mlog FOR UPDATE` | COMPLETE refresh | 不阻塞 | 0s | 正确 |
| 3a | `SELECT * FROM mview WHERE a=1 FOR UPDATE` (非冲突行) | FAST refresh | 不阻塞 | 0s | 正确 |
| 3b | `SELECT * FROM mview WHERE a=1 FOR UPDATE` (冲突行) | FAST refresh | 阻塞 | ~8s (等锁释放) | 正确 |
| 4 | `SELECT * FROM mlog FOR UPDATE` | FAST refresh | 不阻塞 | 0s | 正确 |

补充验证：`SELECT ... FOR UPDATE` 和 `SELECT ... FOR UPDATE NOWAIT` 对 mview 和 mlog 均不被拦截。

## 分析

### 为什么锁 mview 会阻塞 REFRESH

- **COMPLETE refresh** 在悲观事务中执行 `DELETE FROM mview` + `INSERT INTO mview SELECT ...`，DELETE 需要获取目标行的悲观锁。
- **FAST refresh** 对 mview 表执行增量 UPDATE/INSERT/DELETE，同样需要获取目标行的悲观锁。
- 当用户持有 FOR UPDATE 锁时，REFRESH 的内部 session 等待行锁释放（默认超时 50s），锁释放后正常完成。

### 为什么锁 mlog 不阻塞 REFRESH

- **COMPLETE refresh** 不读写 mlog，只操作 mview 表（全量 DELETE + INSERT）。
- **FAST refresh** 通过快照读（snapshot read at `for_update_ts`）读取 mlog，不获取行锁，因此不与用户的 FOR UPDATE 锁冲突。

### 数据正确性

所有场景中 REFRESH 完成后，mview 数据均与 `SELECT a, sum(b), count(1) FROM base_table GROUP BY a` 一致。锁只影响延迟，不影响正确性。

## 结论

1. **功能正确性没有问题**：无论是否被阻塞，REFRESH 最终结果都正确。
2. **存在权限缺口**：`SELECT ... FOR UPDATE` 对 mview/mlog 不被拦截。既然 DML 已被禁止，FOR UPDATE（本质是获取写意图锁）逻辑上也应该被禁止，否则用户可以通过长时间持有 FOR UPDATE 锁来延迟 REFRESH 执行。
3. **建议**：在 planner 层对 mview/mlog 表的 `SELECT ... FOR UPDATE` 进行拦截，与现有 DML 拦截保持一致。具体位置在 `LogicalLock` 构建路径中检查 `TableInfo.MaterializedView` / `TableInfo.MaterializedViewLog`。
