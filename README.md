# CMS 内容增量同步与版本化发布引擎

本仓库实现了一套面向外部内容源的增量同步与版本化发布引擎。上游内容会持续增删改，
本地把内容可靠地拉取到自己的草稿存储，校验通过后原子切换对外发布版本。上游与变更
事件均由本地回环或内存管道模拟，整条链路只在本进程内运行，不依赖任何外部服务，
结果可在本地稳定复现。

## 架构总览

```
┌─────────────┐   事件流/快照    ┌──────────────┐   草稿   ┌──────────────┐
│  upstream   │ ───────────────> │   syncx      │ ───────> │  publish     │
│ (模拟上游)  │ <─────────────── │  (同步引擎)  │          │  (版本发布)  │
└─────────────┘   失败/重试      └──────┬───────┘          └──────┬───────┘
                                       │                         │
                                       ▼                         ▼
                                ┌──────────────┐          ┌──────────────┐
                                │  store       │          │  resolve     │
                                │  草稿/游标   │          │  (多语言回退)│
                                └──────────────┘          └──────────────┘
                                       ▲
                                       │
                                ┌──────────────┐
                                │  schema      │
                                │  (模型演进)  │
                                └──────────────┘
```

核心包：

| 包 | 职责 |
|----|------|
| `internal/model` | 内容、字段、事件、快照等核心类型 |
| `internal/schema` | 内容模型注册、兼容性检查与可区分拒绝原因 |
| `internal/store` | 草稿存储、游标持久化、不可变版本存储 |
| `internal/upstream` | 模拟上游源：事件日志、全量快照、乱序/重复/延迟/故障注入 |
| `internal/syncx` | 同步引擎：游标、乱序缓冲、严格顺序应用、重建、有界退避重试 |
| `internal/publish` | 发布前校验、原子版本切换、历史版本查询 |
| `internal/resolve` | 多语言回退解析，区分"回退值"与"显式空值" |
| `internal/config` | 同步、重试、回退链配置 |

## 同步语义

### 基于游标的增量同步

- 上游为每次变更分配单调递增、连续的序列号（`Seq`）。
- 引擎维护游标 `cursor`，表示"已提交到哪个位置"。游标持久化到 `CursorStore`，
  中断后重启时从最近一次已提交的位置继续。
- 拉取路径：`SyncOnce()` 调用 `upstream.EventsAfter(cursor)` 拉取游标之后的全部事件。
- 推送路径：`Deliver(ev)` 注入单个事件（可乱序、重复、迟到）。

### 乱序、重复与迟到

引擎采用**缓冲 + 严格顺序应用**策略：

1. 事件到达时，若 `Seq <= cursor`，说明已处理过（重复或迟到），直接丢弃，无副作用。
2. 否则放入缓冲区 `buffer[Seq]`。
3. 当 `cursor+1` 对应的事件到达时，应用它并推进游标，然后继续检查后续连续事件，
   直到缓冲区出现缺口。

这样无论事件以何种顺序到达，应用顺序始终是 `1,2,3,...`，最终内容与一次完整同步
的结果完全一致。删除以墓碑（tombstone）形式保留，防止迟到的低序号事件让已删除内容
"复活"。

### 完整重建

`Rebuild()` 执行全量对账：

1. 从上游拉取完整快照（带重试）。
2. 合并：快照对 `Seq <= snapSeq` 的内容是权威的；本地草稿中 `LastSeq > snapSeq`
   的条目（快照点之后到达的变更）予以保留，不丢失。
3. 原子替换草稿，更新游标，发布新版本。

重建期间读取方继续读旧版本；重建前后对外内容对齐且可解释（`RebuildReport` 记录
版本号、游标、增/删/改差异）。

### 失败与重试

- 上游可注入瞬时失败（`FailTransient`）或部分失败（`FailPartial`）。
- 引擎对拉取事件和快照都执行**有界退避重试**：指数退避 + 随机抖动，设上限。
- 失败不产生部分写入：事件要么完整应用，要么不应用；重试从上一次游标继续。
- 重试耗尽后返回错误，不推进游标，不留下部分状态。

## 发布语义

### 草稿与已发布分离

- 草稿存储（`DraftStore`）是可变工作副本，增量同步写入草稿。
- 发布（`Publish`）对当前草稿做深拷贝，生成一个不可变的 `Version`，然后原子切换
  当前版本指针。

### 原子切换

- 读取方通过 `Current()` 获取当前版本，之后从不可变快照中读取。
- 任何读取方在发布过程中只能读到某个已提交的完整版本，**不会出现字段级混合**：
  版本内所有内容来自同一时刻的草稿快照。
- 历史版本通过 `Get(version)` 仍可查询。

### 发布前校验

发布前校验所有非删除内容：已知字段类型必须匹配，必需字段必须存在。校验失败则
拒绝发布，不生成新版本。未知字段（新模型字段）原样保留，不阻止发布。

## 内容模型演进

- 每个内容项记录 `SchemaID` 与 `SchemaVersion`。
- 注册新模型版本时做兼容性检查：
  - **新增字段**：兼容，原样保留。
  - **删除必需字段**：拒绝，原因 `required_field_removed`。
  - **删除/改名必需字段**：拒绝（旧名消失即视为删除），原因同上。
  - **字段类型不兼容变更**：拒绝，原因 `type_changed`。
  - **枚举值删除**：拒绝，原因 `enum_value_removed`。
- 被拒绝时返回 `*IncompatibilityError`，携带可机器区分的 `Reason`、`Field` 等信息。
- 旧模型与新模型的内容在同一份存储中共存，互不影响。

## 多语言回退

- 回退链由配置决定：`[locale, language, default]`。例如 `zh-CN` → `zh` → `en`。
- `ResolveField(id, locale, field)` 沿回退链查找，返回第一个找到的值。
- **回退值 vs 显式空值**：字段值用 `FieldValue{Set, Value}` 表示。
  - `Set=false`：该语言无译文，触发回退。
  - `Set=true, Value=""`：显式填写的空值，**不**再回退。
- `Resolution` 结构携带 `Found`、`FellBack`、`Locale`、`Explicit` 等信息，
  调用方可区分"没找到"、"回退得到"与"显式空值"。

## 配置

`internal/config.Config`：

| 字段 | 默认值 | 说明 |
|------|--------|------|
| `MaxRetries` | 5 | 最大重试次数 |
| `RetryBase` | 10ms | 退避基准 |
| `RetryMax` | 500ms | 退避上限 |
| `RetryJitter` | 0.2 | 抖动系数 |
| `AutoPublish` | true | 同步后自动发布 |
| `PublishInterval` | 100ms | 后台同步间隔 |
| `Locales` | zh-CN, zh, en | 语言列表 |
| `DefaultLocale` | en | 默认（回退终点）语言 |

## 运行与测试

```bash
# 运行演示
go run .

# 运行全部测试
go test ./...

# 带竞态检测
go test -race ./...
```

测试覆盖：

- 乱序投递（`TestOutOfOrderDelivery`）
- 重复投递幂等（`TestDuplicateDelivery`）
- 迟到到达（`TestLateArrival`）
- 删除乱序（`TestDeleteOutOfOrder`）
- 拉取路径网络重排（`TestPullPathWithReordering`）
- 模型兼容演进（`TestCompatibleModelEvolution`）
- 模型不兼容演进与可区分原因（`TestIncompatibleModelEvolution`）
- 新旧模型共存（`TestOldAndNewModelCoexist`）
- 多语言回退与显式空值区分（`TestLocaleFallback`、`TestExplicitEmptyNoFallback`）
- 并发重建不丢变更（`TestConcurrentRebuild`）
- 中断恢复与游标续传（`TestInterruptRecovery`）
- 有界退避重试（`TestRetryBoundedBackoff`、`TestRebuildRetry`）
- 重试耗尽无部分写入（`TestRetryExhausted`）
- 原子发布与历史版本（`TestAtomicPublish`）
- 重建对齐与差异解释（`TestRebuildAlignment`、`TestRebuildWithChanges`）
- 发布前校验（`TestPublishInvalidContent`）
