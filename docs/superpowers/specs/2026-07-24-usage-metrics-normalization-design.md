# Usage 指标归一化设计

日期：2026-07-24

## 背景

观测台当前展示上游调用量、输入、输出、缓存创建、缓存命中和缓存
Token 命中率。三类上游的 usage 字段语义不同：

- Anthropic 的 `input_tokens` 不包含 `cache_read_input_tokens` 和
  `cache_creation_input_tokens`。
- Chat Completions 与 Responses 的 `input_tokens` / `prompt_tokens`
  已包含缓存 Token。
- DeepSeek 在 Chat usage 顶层提供 `prompt_cache_hit_tokens` 和
  `prompt_cache_miss_tokens`。

当前命中率分母已经按后端类型归一化，但总输入、供应商分组和历史记录仍保存
上游原始输入值，导致页面上的几个 Token 指标无法使用同一口径比较或验算。
此外，Chat 同时返回顶层缓存字段和空的 details 对象时，details 的零值会覆盖
有效命中量；指标 channel 满时丢弃事件也没有可见信号。

## 目标

1. 所有展示和聚合使用协议归一化后的完整输入 Token。
2. 缓存 Token 命中率统一为 `cache_read / normalized_input`。
3. 正确处理 Chat 官方 details 与 DeepSeek 顶层缓存字段共存的情况。
4. 保持指标采集不阻塞转发热路径，同时让丢弃事件可观测。
5. 明确请求量按上游尝试统计，而不是按客户端请求统计。

## 非目标

- 不增加持久化存储；指标仍是进程生命周期内的内存累计值。
- 不增加请求维度的缓存命中率。
- 不把 DeepSeek 的 cache miss 解释为缓存创建。
- 不改变 Backend 对客户端 Responses usage 的协议输出。
- 不改变 scheduler 的重试和选源行为。

## 方案

### 输入 Token 归一化

在 `metrics.Collector` 消费 `KindUpstream` 事件时计算完整输入：

```text
backend_type=a:
  normalized_input = input_tokens
                   + cache_read_input_tokens
                   + cache_creation_input_tokens

backend_type=c/r:
  normalized_input = input_tokens
```

缺省 `BackendType` 继续按 Anthropic 处理，保持现有兼容行为。

归一化值用于：

- `Snapshot.TotalInput`
- `GroupStat.InputTokens`
- 上游类型的 `RequestRecord.InputTokens`
- `Snapshot.CacheHitRate` 的分母

客户端汇总历史记录继续使用最终上游事件的数据，但同样通过 Collector 的归一化
函数处理，避免同一请求的 client/upstream 两条历史记录口径不同。

归一化只在 Collector 边界做一次。Backend 继续上报协议原始字段，避免协议层
提前混合原始输入和缓存明细，也便于未来诊断。

### 缓存命中率

命中率定义为：

```text
cache_hit_rate = total_cache_read / total_normalized_input
```

没有有效输入样本时保持 JSON `null`，管理页显示 `-`。缓存创建 Token 已包含在
完整输入分母中，但不进入命中分子。

### Chat 缓存字段优先级

Chat 转换层保留两类字段：

- 官方/OpenAI-compatible：`prompt_tokens_details.cached_tokens`、
  `prompt_tokens_details.cache_write_tokens`
- DeepSeek：`prompt_cache_hit_tokens`

字段优先级按“有效字段值”而不是“父对象存在”判断：

1. `prompt_tokens_details.cached_tokens > 0` 时作为缓存读取量。
2. 否则回退到 `prompt_cache_hit_tokens`。
3. `cache_write_tokens` 独立映射为缓存创建量。

`prompt_cache_miss_tokens` 仅用于理解上游 shape，不映射为缓存创建量，也不替代
`prompt_tokens`。DeepSeek 官方定义 `prompt_tokens = hit + miss`，正常情况下
现有输入分母已经完整。

### 丢弃事件可观测性

保留 `Record` 的 `select + default` 非阻塞投递。在 default 分支通过
`atomic.Uint64` 增加丢弃计数，不写同步日志。

`Snapshot` 新增：

```json
{
  "dropped_events": 0
}
```

管理页在丢弃量大于零时显示一张告警色指标卡；值为零时仍展示 `0`，让用户明确
统计链路是否完整。该计数不因 snapshot 读取清零。

### 管理页语义

- “请求量”改为“上游调用量”。
- “总输入”表示协议归一化后的完整输入 Token。
- “缓存命中率”继续明确为“缓存 Token 命中率”。
- 新增“指标丢弃量”卡片。

README 同步说明统计粒度、输入口径、命中率公式和进程生命周期范围。

## 数据流

```text
上游 usage
  → Backend 上报原始 Input/Output/CacheRead/CacheCreate + BackendType
  → scheduler.UpstreamEvent
  → server.recordUpstream
  → metrics.Record（非阻塞，失败则 dropped_events++）
  → Collector.apply 归一化 Input
  → Snapshot / admin SSE / 管理页
```

## 测试

### Collector

- Anthropic 总输入包含 input、cache read、cache create。
- Chat/Responses 总输入不重复增加缓存字段。
- 混合后端总输入和命中率可验算。
- 分组和历史记录使用同一归一化口径。
- 空输入命中率为 `null`。
- channel 满时 `DroppedEvents` 增加且 `Record` 不阻塞。

### Chat 转换

- 仅 DeepSeek 顶层命中字段时正确映射。
- 仅 details 字段时正确映射读取和创建。
- 顶层命中字段与空 details 共存时不丢失顶层值。
- details 中存在非零 `cached_tokens` 时优先使用 details。

### 回归门禁

依次运行：

```bash
go test ./internal/metrics ./internal/chatstreamconv ./internal/backend ./internal/admin -count=1
task test-race
task check
```

若 `task check` 被与本次改动无关的既有格式问题阻塞，单独列明文件和证据，不修改
无关工作树内容。

## 兼容性

`total_input`、分组和历史中的 `input_tokens` 数值会在 Anthropic 缓存请求上变大，
这是有意的语义修正。API 字段名与 JSON 类型不变。

新增 `dropped_events` 是向后兼容字段。管理页与旧 snapshot 兼容：字段缺失时按
零处理。
