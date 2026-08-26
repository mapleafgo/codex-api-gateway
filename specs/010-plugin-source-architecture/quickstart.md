# Quickstart: 插件式上游源架构

## 前置

- Go 1.26.5、Taskfile CLI。
- 已注册四个内置插件；`config.example.yaml` 使用新形状。
- 需要 Copilot 或真实上游的验证必须有有效凭据，否则跳过对应场景。

## 1. 配置加载与迁移错误

```bash
cat > /tmp/bad.yaml <<'EOF'
sources:
  - name: old
    backend_type: g
EOF
go run ./cmd/server -config /tmp/bad.yaml
```

期望：启动失败，错误包含 source、字段和可用 backend 列表。改用稳定 `backend` 后正常启动。

## 2. 加载三类内置源

编辑 `config.yaml`：

```yaml
sources:
  - { name: a, backend: anthropic,   base_url: "https://mock-a", api_key: "k", options: { cache_enabled: true } }
  - { name: c, backend: openai-chat,  base_url: "https://mock-c", api_key: "k" }
  - { name: r, backend: openai-responses, base_url: "https://mock-r", api_key: "k" }
```

```bash
task build && ./codex-api-gateway -config config.yaml -d
curl -s http://127.0.0.1:8383/v1/models
```

期望：三个源进入调度；`backend` 身份出现在 `/v1/models` 与日志。调整配置并 reload，旧运行状态不变。

## 3. Copilot 委托与回退

准备 mock Copilot endpoint（`/models` 返回 `supported_endpoints`）。配置：
- 目录可用：路由按 `/responses > /v1/messages > /chat/completions`；观测事件 `backend=github-copilot`。
- 目录不可用：回退 Responses 路径，记录诊断 WARN，不伪造成功、不替上游判定账户权限。

```yaml
sources:
  - name: copilot
    backend: github-copilot
    base_url: "https://mock-copilot"
    options:
      github_token: ${COPILOT_GITHUB_TOKEN}
```

验证：

- Responses 模型走 `/responses`；Messages 模型走 `/v1/messages`；Chat 模型走 `/chat/completions`。
- 目录不可用时仅回退到 Responses，不伪造成功、不替 Copilot 判断账号权限。
- 观测事件 `backend=github-copilot`；delegate route 只出现在结构化日志。

## 4. 管理页动态能力

```bash
curl -s http://127.0.0.1:8383/admin/api/source-plugins
```

期望：四类插件描述符，每个含 schema 与 actions。浏览器打开源编辑页：

- 普通源显示 base_url / api_key 必填。
- Copilot 显示授权动作、隐藏普通密钥必填。
- 敏感值 GET 为 `__codex_redacted__`；空提交保留、`__codex_clear__` 清空。
- Device Flow 显示 user code 与 verification URI，完成后刷新页面保持脱敏状态。

手动前往 `http://127.0.0.1:8383` 完成浏览器验收（新增、编辑、重新授权、保存刷新）。

## 5. 加法器测试源

在临时分支实现最小 SourcePlugin（含 Descriptor、ValidateSource、Backend），在 `cmd/server/main.go` 注册。期望：

- 注册后可在 config 中被引用并参与调度。
- 未注册时配置校验失败且旧状态不变。
- 管理页 Source 下拉自动出现该描述符，共享前端无需改动。

## 6. 门禁

协议回归矩阵必须覆盖四个内置源各自的一条真实请求，并分别验证流式终态、空流换源、4xx 归因和客户端取消；Copilot 还需验证目录可用与目录失败两种委托路由来源。矩阵中任一组合失败都视为 SC-001 未通过。

```bash
task check
task test-race

## 7. 四源协议回归矩阵（SC-001）

迁移前后都必须保留以下场景。除 Copilot 外，每个内置源至少覆盖一行；Copilot 行额外覆盖目录可用/目录失败。

| 源 | 流式终态 | 空流换源 | 4xx 归因 | 客户端取消 | 模型列表 | 连通性探测 | 管理保存 | 目录 |
|---|---|---|---|---|---|---|---|---|
| anthropic | completed/incomplete | 无内容时换源 | 首个 4xx 不重试 | canceled 不计上游失败 | `/v1/models` | `/v1/messages` 探测 | 写盘热重载 | n/a |
| openai-chat | completed/incomplete | 无内容时换源 | 首个 4xx 不重试 | canceled | Chat `/models` | `/chat/completions` 探测 | 写盘热重载 | n/a |
| openai-responses | completed/failed/incomplete 原样透传 | 首个事件即锁定 | 首个 4xx 不重试 | canceled | Responses `/models` | `/responses` 探测 | 写盘热重载 | n/a |
| github-copilot | 委托路径终态 | 目录失败仍回退 | 委托 4xx 归因 | canceled | Copilot 目录筛选 | 目录探测 | 授权写盘热重载 | 可用 r>a>c；失败回退 r |

矩阵中任一组合失败都视为 SC-001 未通过。
```

架构守护测试（`internal/plugin` 断言共享目录不 import `internal/plugins/*`）必须通过。所有断言见 contracts/ 与 data-model.md，不在此重复。
