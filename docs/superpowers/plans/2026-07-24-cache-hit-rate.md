# 缓存命中率修复实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**目标：** 按不同上游协议的 usage 语义统一采集并计算缓存 token 命中率。

**架构：** 转换层把 OpenAI、DeepSeek 与 Anthropic 的缓存 usage 字段归一到
`ResponseUsage`，Backend 再上报统一的 `CacheRead`/`CacheCreate`。Collector 按
`BackendType` 归一化完整输入，供总量、分组、历史和缓存命中率共同使用；无有效
输入时 API 返回 `null`。

**技术栈：** Go、标准库 `testing`。

## 全局约束

- `TotalInput`、历史记录和供应商分组统一改为完整输入 Token 语义。
- 不增加配置项或第三方依赖。
- 缺省 `BackendType` 继续按 Anthropic 兼容处理。

---

### Task 1：按协议归一化缓存命中率分母

**文件：**
- 修改：`internal/metrics/metrics.go`
- 测试：`internal/metrics/metrics_test.go`

**接口：**
- 消费：`RequestEvent.BackendType`、`InputTokens`、`CacheRead`、`CacheCreate`
- 产出：`Snapshot.CacheHitRate`

- [x] **Step 1：编写失败测试**

增加表驱动用例，分别验证 Anthropic、OpenAI Chat、Responses 和混合聚合：

```go
func TestCollectorCacheHitRateUsesBackendTokenSemantics(t *testing.T)
```

- [x] **Step 2：运行测试并确认失败**

运行：

```bash
go test ./internal/metrics -run TestCollectorCacheHitRateUsesBackendTokenSemantics -count=1
```

预期：Anthropic 或混合口径断言失败。

- [x] **Step 3：实现最小修复**

在 Collector 总量中增加缓存分母累计值；消费 `KindUpstream` 时，`a` 或空类型累加三项之和，`c/r` 只累加 `InputTokens`。`Snapshot()` 使用该累计值计算 `CacheHitRate`。

- [x] **Step 4：运行定向测试**

```bash
go test ./internal/metrics -count=1
```

预期：全部通过。

- [x] **Step 5：运行仓库门禁**

```bash
task check
```

结果：`go test ./...`、`go vet ./...`、`task test-race` 通过；`fmt-check`
被三个与本次改动无关的既有文件阻塞。

---

### Task 2：补齐兼容 usage 字段与空样本语义

**文件：**
- 修改：`internal/model/responseobject.go`
- 修改：`internal/chatstreamconv/converter.go`
- 修改：`internal/backend/responses.go`
- 修改：`internal/metrics/metrics.go`
- 修改：`internal/admin/assets/index.html`
- 测试：`internal/chatstreamconv/converter_test.go`
- 测试：`internal/backend/responses_test.go`
- 测试：`internal/metrics/metrics_test.go`

**接口：**
- 消费：OpenAI `cached_tokens` / `cache_write_tokens`，DeepSeek
  `prompt_cache_hit_tokens`
- 产出：`CacheReadInputTokens`、`CacheCreationInputTokens`、
  可空的 `Snapshot.CacheHitRate`

- [x] **Step 1：编写失败测试**

增加以下回归用例：

```go
func TestUsageDeepSeekCacheTokensMapped(t *testing.T)
func TestUsageCacheWriteTokensMapped(t *testing.T)
func TestParseUsageFromEvent_CacheWriteTokens(t *testing.T)
func TestCollectorCacheHitRateIsNullWithoutInput(t *testing.T)
```

- [x] **Step 2：运行测试并确认失败**

```bash
go test ./internal/chatstreamconv ./internal/backend ./internal/metrics -count=1
```

预期：新增字段或空样本断言失败。

- [x] **Step 3：实现最小修复**

Chat 优先读取官方 `prompt_tokens_details`，并接受 DeepSeek 顶层命中字段；
Responses 读取 `input_tokens_details.cache_write_tokens`。Collector 仅在有效
分母存在时返回非空命中率；管理页文案明确为缓存 token 命中率。

- [x] **Step 4：运行定向测试**

```bash
go test ./internal/chatstreamconv ./internal/backend ./internal/metrics ./internal/admin -count=1
```

预期：全部通过。

- [x] **Step 5：运行仓库门禁**

```bash
task test-race
```

结果：`go vet ./...`、`task test-race`、`go test ./...` 通过；`task check`
仍被三个与本次改动无关的既有文件格式问题阻塞。
