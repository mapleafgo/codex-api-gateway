# Quickstart: GitHub Copilot 原生后端接入

## 前置条件

- 已有 GitHub OAuth token（`gho_...` 或 `ghu_...`），具有 Copilot 访问权限
- 网关可访问 `api.github.com`（GraphQL 发现）和 Copilot API endpoint

## 配置

在 `config.yaml` 的 `sources` 中添加一个 `g` 源：

```yaml
sources:
  - name: copilot
    backend_type: g
    github_token: ${COPILOT_GITHUB_TOKEN}  # 或直接写 gho_... / ghu_...
    default_model: gpt-5.3-codex
    # model_map: { gpt-5: gpt-5.3-codex }
    # supports_web_search: true
    # base_url: https://api.githubcopilot.com  # 可选：固定 endpoint 并跳过 GraphQL 发现
```

## 验证步骤

### 1. 启动网关

```bash
task run
```

启动本身不访问 GitHub。发送首个请求后，日志中应出现 GraphQL endpoint 发现记录（或回退到默认 endpoint 的 WARN）。

### 2. 发送流式请求（r 路径）

```bash
curl -N http://localhost:8383/v1/responses \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-5.3-codex","input":[{"type":"message","role":"user","content":"Hello"}],"stream":true}'
```

预期：收到完整 Responses SSE 事件流（`response.created` → `response.output_text.delta` → `response.completed`）。

### 3. 验证模型路由

用不同模型测试路由分发：
- 支持 `/responses` 的模型 → 走 r 路径（日志 `backend_type=g, route=r`）
- 只支持 `/v1/messages` 的模型（如 Claude）→ 走 a 路径（日志 `backend_type=g, route=a`）
- 只支持 `/chat/completions` 的模型 → 走 c 路径（日志 `backend_type=g, route=c`）

### 4. 验证筛选

启用 debug 日志 (`logging.level: debug`)，检查被过滤的模型（`model_picker_enabled: false`）不出现在可用列表中。

### 5. 测试

```bash
task check          # 格式 + vet + 全部测试
task test-race      # 并发安全检查（模型缓存）
```
