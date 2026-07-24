# Anthropic 转换参数配置设计

## 背景

Responses 客户端未传 `max_output_tokens` 时，Anthropic Messages 请求必须仍携带必填的 `max_tokens`。原实现固定使用 `4096`；当 `reasoning.effort=max` 时，thinking 与正文共享该额度，上游可能耗尽全部 token 后以 `stop_reason=max_tokens` 结束，只返回 reasoning 而没有正文或工具调用，客户端表现为长时间无可见回复。

现有顶层 `cache.ttl` 也只服务 Anthropic 路径，且网关总会注入缓存断点，无法按运行环境关闭。Anthropic 专属策略需要统一归入独立配置前缀和管理卡片。

## 目标

- Anthropic 专属策略统一放入顶层 `anthropic.*`。
- 支持默认输出额度、Prompt Cache 开关和 TTL。
- 未配置时使用内置默认值 `16384`。
- 客户端显式传入合法的 `max_output_tokens` 时始终优先使用客户端值。
- 日志能够区分客户端显式额度与网关默认额度。
- Chat `c` 与 Responses `r` 路径行为保持不变。

## 配置与校验

```yaml
anthropic:
  default_max_tokens: 16384
  cache_enabled: true
  cache_ttl: 5m
```

- `default_max_tokens`：客户端未传 `max_output_tokens` 时写入上游 `max_tokens`。省略或为 `0` 时补为 `16384`；负数拒绝加载。
- `cache_enabled`：是否自动注入 Anthropic prompt cache 断点。省略默认 `true`，显式 `false` 时顶层、system、tools 和 MCP toolset 均不注入。
- `cache_ttl`：仅允许 `5m` 或 `1h`，省略默认 `5m`。

旧 `cache.*` 不再参与运行时配置，加载时输出废弃字段警告。内置 `16384` 比原 `4096` 扩大四倍，为高 reasoning 请求保留正文空间，同时避免默认提高到 `32768` 或 `65536` 后被部分兼容源拒绝。

## 数据流

`config.Load` 补齐并校验 `AnthropicCfg`，热重载继续通过 `config.Load` → `holder.Replace` → `scheduler.Reload` 生效。`convert.ToAnthropic` 读取当前不可变配置快照：

1. `req.MaxOutputTokens` 有效且大于零：上游使用客户端值。
2. 客户端未传：上游使用 `anthropic.default_max_tokens`。
3. `convert.ToAnthropic` 不再自行硬编码 `4096`。

缓存关闭时 `applyAnthropicCacheControl` 直接返回；MCP 注入只移动已存在的 tools 缓存断点，不得重新创建。

## 日志

请求转换完成日志增加：

- `max_tokens`：最终写入上游的值。
- `max_tokens_source`：`client` 或 `anthropic_default`。
- `cache_enabled`、`cache_ttl`：本次转换的有效缓存策略。

Anthropic Client 与 SSE 扫描层统一使用请求上下文 logger，保留 `request_id` 关联；不记录新的大字段，不增加逐事件日志。

## 管理页

管理 API 增加与 YAML 同构的 `anthropic` 视图。H5 全局配置页新增独立 Anthropic 卡片，集中展示默认输出额度、缓存开关和缓存 TTL。

## 测试

- `internal/config`：三项默认值、显式覆盖、非法额度和 TTL 校验、旧前缀忽略。
- `internal/convert`：客户端值优先、全局默认值兜底、缓存关闭后不注入。
- `internal/anthropic`：MCP 注入在缓存关闭时不创建断点。
- `internal/backend` / `internal/server`：转换日志记录额度来源并保持请求关联。
- `internal/admin`：GET/POST 配置完整保留三项配置，内嵌页面包含独立卡片。
- `docs/protocol-coverage.md`：更新默认值和配置语义。

提交前运行 `task check`；配置与管理页涉及共享运行时快照但不新增 goroutine，无需额外并发机制，仍运行 `task test-race` 作为回归门禁。
