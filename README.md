# cms-content-sync-go

面向外部内容源的**增量同步与版本化发布引擎**。上游内容持续增删改，引擎把变更可靠地拉取到本地存储，校验通过后按版本原子发布。上游与变更事件均由内存实现模拟（`internal/upstream`），整条链路只在本进程内运行，不依赖任何外部服务，结果在本地稳定复现。

## 快速开始

```bash
go build ./...        # 构建
go test ./...         # 运行全部测试
go test -race ./...   # 含竞态检测
go run ./cmd/cms-sync-demo   # 确定性端到端演示（多次运行输出一致）
```

## 架构

```
upstream.MemorySource          syncer.Engine                 store.Store
┌──────────────────┐  Fetch   ┌─────────────────┐  Tx/ReplaceDraft ┌───────────────┐
│ 变更日志(seq 递增) │ ───────▶ │ 游标 + 有界退避   │ ───────────────▶ │ 草稿(draft)    │
│ 全量快照 Snapshot │ ◀─────── │ 检查点 Checkpoint │                  │  → Publish →  │
│ 失败注入(可复现)   │          │ 增量/重建/恢复     │                  │ 版本0..N(不可变)│
└──────────────────┘          └─────────────────┘                  └──────┬────────┘
                                                                          │ atomic 切换
                                                     读取方 ◀── Current() / Version(n)
```

- `internal/model` — 内容模型（schema）注册、兼容性校验、内容校验。
- `internal/store` — 草稿/发布分离的版本化存储，原子发布，历史可查。
- `internal/syncer` — 游标增量同步、有界退避重试、检查点恢复、完整重建。
- `internal/upstream` — 内存模拟的上游内容源与变更流，支持失败注入。
- `internal/locale` — 多语言值的确定性回退解析。

## 同步语义

**游标（Cursor）**：`Cursor.Seq` 是上游变更日志的全局单调位点，表达“已提交处理到的位置”。游标只有在一批变更**完整校验、落库并发布后**才推进并写入 `CheckpointStore`；崩溃后用同一检查点重建引擎即可从最近提交位置继续。

**乱序 / 重复 / 迟到**：每条变更携带该内容的单调修订号（`Revision`）。存储层按修订号做最后写入胜出：

- 重复到达（修订号相同）→ 跳过，无副作用（幂等）；
- 乱序到达（更旧的修订号晚到）→ 跳过；
- 删除后迟到的旧更新 → 被删除墓碑拦截，不会复活内容；
- 删除之后更新的修订号 → 视为重新创建，正常生效。

因此无论变更以何种顺序、重复多少次到达，最终内容都收敛到一次完整同步的结果（测试：`TestIncrementalEqualsFullRebuild`、`TestOutOfOrderDuplicateLate`）。

**失败语义**：

- 上游瞬时失败按**有界指数退避**重试（见配置）；超过 `MaxAttempts` 报错，游标不前进、不产生任何写入。
- 一批变更在**一个事务**中先整体校验再提交：任何一条校验失败（含毒消息），整批丢弃，不留部分写入。
- 上游部分失败（返回部分数据 + 错误）同样整批丢弃并重试。
- 重复处理同一批变更因修订号去重而无副作用，因此“提交后、检查点保存前崩溃”的恢复路径是安全的。

**完整重建（Rebuild）**：以上游快照为基底整体替换草稿，随后**回放快照位点之后到达的变更**，再原子发布。重建期间旧版本持续对外可读；重建期间到达的新变更通过回放与游标语义保证不丢失。重建后版本的 `AtSeq` 等于其覆盖的上游位点，可与上游 `Head` 对照解释对齐关系。重建与增量同步在写路径上互斥，读取全程无锁。

## 发布语义

- 同步只写**草稿**；读取方只能看到**已发布版本**。
- `Publish` 把当前草稿固化为**不可变** `Version` 并通过原子指针切换：任何读取方在发布过程中要么读到旧版本、要么读到新版本，**不会出现字段级混合**。
- 每个 `Version` 是完整快照，携带版本号与 `AtSeq`；历史版本全部保留，可通过 `Store.Version(n)` 查询。
- 只有内容实际发生变化（有变更生效）时才产生新版本。

## 内容模型演进

- 模型以 `(名称, 版本)` 注册；新版本相对旧版本做**向后兼容**检查：
  - ✅ 新增可选字段；
  - ❌ 删除必需字段（`ReasonRemovedRequiredField`）；
  - ❌ 疑似改名必需字段（`ReasonRenamedRequiredField`，删除必需字段且出现同类型新字段）；
  - ❌ 已有字段类型变更（`ReasonFieldTypeChanged`）；
  - ❌ 新增必需字段（`ReasonAddedRequiredField`，存量内容将无法满足）。
  拒绝时返回 `*model.IncompatibleError`，原因可通过 `Reason` 字段程序化区分。
- 内容中出现模型**未声明的字段**是允许的，并以 `json.RawMessage` 原样保留，同步往返不丢失（新增字段的前向兼容）。
- 不同模型版本的内容按各自版本的 schema 校验，在同一份存储、同一发布版本中**共存**。

## 多语言回退

`locale.Values`（`map[locale]value`）按 `Chain`（如 `["de-AT","de","en"]`）确定性地解析：

- 键**存在**即视为显式赋值——**显式空串优先于回退**，结果 `Explicit=true`；
- 键缺失才逐级回退，结果 `Explicit=false` 并记录实际来源语言；
- 整条链无值时 `Found=false`。

## 配置

`syncer.Backoff`：

| 字段 | 含义 | 默认/约束 |
|---|---|---|
| `Base` | 初始退避间隔 | 由调用方设置 |
| `Max` | 单次退避上限（指数翻倍后封顶） | ≤0 时取 `Base` |
| `MaxAttempts` | 最大尝试次数（含首次） | ≤0 时取 1 |
| `Sleep` | 可注入的睡眠函数 | nil 使用真实计时器；测试注入空实现保证可复现 |

`syncer.New(src, store, checkpoints, backoff, batchSize)`：`batchSize` 为每批拉取的变更条数上限，≤0 表示不限。

## 测试覆盖

- 乱序、重复、迟到收敛（`store`、`syncer`）
- 增量同步 ≡ 完整重建（`TestIncrementalEqualsFullRebuild`）
- 瞬时失败重试、重试耗尽不留部分写入（`TestTransientFailureRetryAndCursorStability`）
- 部分失败整批丢弃（`TestPartialFetchLeavesNoPartialWrites`）
- 中断后从检查点恢复、重复处理幂等（`TestCrashRecoveryFromCheckpoint`）
- 重建期间新变更不丢失、重建前后对齐可解释（`TestRebuildPicksUpChangesArrivingDuringRebuild`）
- 增量/重建/读取并发（`TestConcurrentRebuildSyncAndReads`，`-race` 验证）
- 模型演进：兼容接受、四类不兼容拒绝、未知字段往返保留、新旧模型共存
- 多语言回退：回退顺序、显式空值与缺失的区分
