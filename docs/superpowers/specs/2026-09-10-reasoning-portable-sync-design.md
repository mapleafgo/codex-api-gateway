# 会话同步 reasoning 便携式清洗设计

> 日期：2026-09-10
> 状态：待评审
> 触发背景：切换第三方 provider 到 OpenAI 官方后，历史会话恢复被上游 400 拒绝；本设计在已有「明文 content 折叠进 summary」修复基础上，补齐 encrypted_content 与 id 两个边界。

## 背景与问题

网关代理客户端 OpenAI Responses 请求到多个上游。历史会话在本机 rollout JSONL 中由第三方 Provider（如 GLM）写入带明文 content 的 reasoning 项，以及 encrypted_content 与 id。切回官方 OpenAI 后，恢复会话会把整个 reasoning 项原样回发，依次触发三类上游 400：

1. array_above_max_length：reasoning.content 携带明文数组（OpenAI 服务端运行时校验要求该数组最大 0）；
2. invalid_encrypted_content：第三方签发密文与官方服务端密钥不匹配；
3. 无效 id：官方 store=false 只接受自己分发的 item id，第三方 id 被拒。

现有修复仅处理第 1 类（折叠 content 进 summary）。本设计补齐全部三层，使任意源切换后会话可续跑。

## 目标

- 会话同步后，所有 reasoning 项收敛为统一便携形态，无论目标上游是谁都不触发上述 400；
- 保持幂等：重复同步结果字节稳定；
- 保持最小侵入：只改 internal/codexsessions，不新增配置项，不改调度、后端、管理页。

## 非目标

- 不做「按源保留官方合法密文」的有条件清洗（当前无法可靠验证密文签发者，且 model_provider 标记切换后已被清除；留待后续真实需求出现再配置化）；
- 不在 /v1/* 转发路径增加动态改写（同步层即可保证 rollout 干净，新增转发层兜底的复杂度不值得）；
- 不处理官方会话结构兼容问题。

## 不同目标方向的影响

便携式形态对任意目标方向都是「安全无新增 400」的形态，代价是推理原文折损：

| 目标方向 | 影响 | 说明 |
| --- | --- | --- |
| OpenAI 官方 | 唯一可接受形态 | 明文 content / 三方密文 / 三方 id 均被拒绝，portable 是标准解法 |
| 第三方 Responses 兼容端点（GLM 等） | 可接受，暂无拒绝先例 | 校验普遍较宽松，LiteLLM、OpenRouter 以 summary-only 形态对接；该接受度无公开源码可直接验证，留白待实测 |
| Anthropic / Chat 转换路径 | 更易映射 | reasoning 本无明文槽位，去除 id/密文后转换层按既有规则丢弃或转文本，不新增风险 |
| 信息损失 | 完整推理链不可恢复 | 明文已折入 summary，消息状态不受影响；若日后需按目标端恢复完整推理，走「非目标」配置化扩展 |

## 统一便携形态

同步后 reasoning 项收敛为：

```json
{"type":"reasoning","summary":[{"type":"summary_text","text":"..."}],"status":"completed"}
```

字段处置规则：

| 字段 | 处置 | 依据 |
| --- | --- | --- |
| content（明文，对象 part 或字符串简写） | 文本折入 summary，content 置 null | 对齐 LiteLLM 与 openai/codex 序列化纪律 |
| encrypted_content | 删除 | 对齐 codex-switch 默认剥离，消除第三方签名 400 |
| id | 删除 | 对齐 codex-switch 实测（无 id 始终被接受） |
| summary | 保留并合并折叠文本，去重 | 幂等 |
| status | 保留 | 官方 schema 允许，无风险 |
| type | 保持 reasoning | 协议判别符 |

边界行为：

- content 与 encrypted_content 并存：明文折入 summary，密文删除；
- content 为空或无文本：不追加空 summary 条目，仅剥离无效字段；
- JSON 解析失败或非 response_item / payload.type=reasoning 的行：原样保留；
- 重复同步：summary 去重后结果字节稳定。

## 影响范围

- 修改：internal/codexsessions/sessions.go（扩展清洗逻辑）与 sessions_test.go（补用例）；
- 新增：设计文档本文件；
- 明确不动：用户工作区其他未提交改动（docs/protocol-coverage.md、internal/backend/*、internal/plugin/helpers*、internal/scheduler/scheduler_test.go）。

## 错误处理与可观测性

- JSON 解析失败按行跳过，不 panic、不影响其他行；
- 无新增业务日志需求；清洗属会话重建前的常规降级路径，按仓库「静默跳过」约定不记日志；
- 若后续需要观测，走 slog.Debug，不新增输出通道。

## 测试计划

- 现有用例保持通过（幂等、字符串简写、密文相关用例更新为新行为）；
- 新增：encrypted_content 剥离、id 剥离、content+encrypted_content 并存、status 保留、无文本不追加、非 reasoning 行不变；
- 一次性用真实出错会话副本做外部验证（临时验证不提交），断言全部 reasoning 行收敛且其余行字节不变；
- 提交前：gofmt -w、go vet ./internal/codexsessions/、golangci-lint run ./internal/codexsessions/...、go test ./internal/codexsessions/。

## 验收标准

1. 含三类脏数据的会话经过 Sync 后，全部 reasoning 项无 content（或为 null）、无 encrypted_content、无 id，summary 完整，JSON 合法；
2. 干净会话重复同步前后字节一致（幂等）；
3. 非 reasoning 行与解析失败行零改动；
4. 切换任意 provider 后恢复不再出现三类 400。
