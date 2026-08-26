# Contract: Admin API

所有管理接口固定在 `/admin/api/*`，JSON、错误体 `{ "error": "...", "detail": "..." }`，并统一被 recover 中间件包裹。

## GET /admin/api/source-plugins

返回注册表描述符，供前端渲染通用区与 options 表单、扩展动作。

```json
{
  "plugins": [
    {
      "id": "github-copilot",
      "title": "GitHub Copilot",
      "summary": "认证、模型目录与协议分发层",
      "capabilities": ["anthropic-messages", "responses"],
      "streaming": "converted",
      "schema": [
        { "name": "github_token", "label": "GitHub Token", "type": "text", "required": true,
          "sensitive": true, "applies_to": "options" }
      ],
      "actions": [
        { "id": "device_auth", "label": "重新授权", "kind": "device_code_status" }
      ]
    }
  ]
}
```

约束：

- `schema` 是声明式数据，共享前端不解析字段名语义。
- 前端仅实现通用渲染：text/password/boolean/integer/select/string-map、必填标记、敏感占位、扩展动作按钮。
- exchange 后不写死某个源专属 HTML 片段。

## GET /admin/api/config

`sources[].backend` 替代 `backend_type`。敏感字段固定输出：

```json
{ "name": "copilot", "backend": "github-copilot", "options": { "github_token": "__codex_redacted__" } }
```

明文凭据绝不进入响应。

## POST /admin/api/config

全量覆盖式更新，继续用 `writeMu` 与临时文件 + rename 原子写盘。校验顺序：JSON 解码 → `Registry.ValidateSource` → 通用平台校验 → 注入的 Registry 校验 → 原子写盘 → reload。

敏感 options 语义：

| 提交值 | 结果 |
|---|---|
| `__codex_redacted__` | 拒绝（用户应留空表示保留） |
| 空串或缺省 | 保留同名同类型源已有值 |
| `__codex_clear__` | 显式清空该敏感值 |
| 其他 | 替换为新值 |

空 `api_key` 与敏感 options 若目标源已存在则保留旧值；新建源时为空则按 schema 可空性校验。

## Actions

统一入口：

```text
POST /admin/api/source-plugins/{backend}/actions/{action}
```

`device_code_status` 契约（Copilot Device Flow 的通用形态）：

```json
// 200 start 响应
{ "state": "awaiting_user", "user_code": "ABCD-1234",
  "verification_uri": "https://github.com/login/device",
  "interval_seconds": 5, "source_name": "copilot" }
```

```text
GET  /admin/api/source-plugins/github-copilot/auth/status
POST /admin/api/source-plugins/github-copilot/auth/cancel
```

共享页面只理解：awaiting_user 展示 user code / verification URI / interval；starting/saving 显示处理中；authorized/cancelled/error 为终态；冲突返回 409 与当前公开状态。共享核心不理解 GitHub、Copilot 文案。

rule：

- 任意时刻进程内至多一个活跃会话。
- device code 与 access token 不进入公开状态。
- 授权成功后动作回调 admin 注入的配置写盘函数，复用原子写盘 + reload；失败保持旧配置。
- action 返回 501 表示插件不支持该动作；不支持某项 catalog/probe 能力的插件返回明确能力缺失结果。

## Other Endpoints

- `GET /admin/api/models?source=<name>`：仍返回 `{ source, models: [...], capabilities: [...] }`。
- `GET /admin/api/upstream-models`：支持未落盘草稿；敏感 options 先做保留值合并。
- `POST /admin/api/sources/test`、`/sources/disabled`、`/sources/reorder`、`/sources/delete`：继续用既有语义，仅把 backend 字段名与敏感合并替换为契约行为。风险与错误信息保持现有归因。
