# 管理页 GitHub Copilot Device Flow 授权设计

日期：2026-08-26

## 目标

管理员在管理页发起 GitHub Copilot 登录授权后，网关获得长期 GitHub OAuth token，并把它写入指定 `backend_type: g` 源的 `github_token`，随后通过既有配置热重载链路生效。本设计只覆盖 Device Flow 登录授权，不做 `copilot2api credentials.json` 导入、token 刷新或管理页账号体系。

## Zed 参照事实

- OAuth Client ID 使用 Zed 公开应用 ID `6e3a0413e62d19d75ff1`。
- scope 为 `read:user`。
- Device Code URL 为 `https://github.com/login/device/code`。
- Access Token URL 为 `https://github.com/login/oauth/access_token`。
- Token 请求 grant type 为 `urn:ietf:params:oauth:grant-type:device_code`。
- 启动响应缺省 interval 为 5 秒；轮询前先等待一个 interval。
- `authorization_pending` 继续轮询；`slow_down` 将 interval 增加 5 秒。
- `expired_token`、`access_denied` 与其他错误进入失败终态。
- 成功结果是长期 `ghu_...` token，直接作为 Copilot API Bearer token，不换 session token。

## 架构

Device Flow 客户端放在 `internal/copilotclient` 的 L1 客户端层，负责构造 GitHub 请求、解析 Device Flow 响应和按节奏轮询。它不依赖 admin、scheduler 或 backend。

`internal/admin` 增加一个小型授权会话管理器，只负责同一时间维护一个活跃会话、暴露状态、取消轮询，并在成功后调用既有配置写盘链路。管理页仍是被组合的旁路能力，不持有 scheduler、Backend 或熔断器内部状态。

配置生效保持单一链路：

```text
GitHub token -> 更新 config 快照中的 g 源 -> 原子写 config.yaml -> configwatch -> holder.Replace -> scheduler.Reload
```

## 管理 API

### POST /admin/api/copilot/auth/start

请求携带授权成功后的落盘目标和完整源草稿：

```json
{
  "source": {
    "name": "copilot",
    "backend_type": "g",
    "base_url": "",
    "api_key": "",
    "default_model": "",
    "supports_web_search": true,
    "disabled": false,
    "model_map": {},
    "headers": {}
  }
}
```

`source.name` 必填，`backend_type` 必须是 `g`。名称已存在且类型为 `g` 时，成功后更新该源并保留其原有 `github_token` 之外的字段按草稿覆盖；名称不存在时，成功后新增该源。请求校验失败立即返回，不启动 Device Flow。若已存在活跃会话，返回当前公开状态和冲突标识，不创建第二个会话。

### GET /admin/api/copilot/auth/status

返回可安全展示的公开状态：

```json
{
  "state": "awaiting_user",
  "user_code": "ABCD-1234",
  "verification_uri": "https://github.com/login/device",
  "interval_seconds": 5,
  "source_name": "copilot",
  "error": ""
}
```

`state` 包括 `idle`、`starting`、`awaiting_user`、`saving`、`authorized`、`cancelled`、`error`。`starting/awaiting_user/saving` 是活跃态；`authorized/cancelled/error` 是终态。前端按服务端返回的 interval 轮询。

### POST /admin/api/copilot/auth/cancel

取消当前活跃会话并进入 `cancelled`。取消只停止本次 Device Flow 和落盘动作，不禁用或删除旧 token。

## 用户流程

新建源时，管理员填写除 GitHub token 外的 Copilot 源信息，点击“使用 GitHub 授权”。前端把表单作为草稿发送到 start，然后展示 user code 与验证地址，并轮询 status。已有源重授权时也打开同一弹窗，草稿来自当前表单或源卡片数据。

用户在浏览器完成 GitHub 授权后，后端收到 token，先写入内存中待保存的目标，再走现有写盘和热重载逻辑。保存成功后状态变为 `authorized`；保存失败则进入 `error`，但错误响应与日志不得包含 token。

## 安全与错误处理

- `device_code` 只保存在服务端会话内。
- GitHub access token 不返回给前端，不写入日志、metrics、SSE 或管理页快照。
- 会话由互斥锁保护；进程重启即丢失进行中的 Device Flow，需要重新发起。
- GitHub HTTP 失败、响应解析失败和未知 OAuth error 进入 `error`。
- 配置校验、序列化或写盘失败进入 `error`，磁盘旧配置保持不变；写盘成功后的 reload 或模型目录同步失败按既有配置保存语义处理，并只暴露不含凭据的错误。
- 日志只使用结构化 `slog`，可包含 source name、state 和错误文本，不包含凭据。

## 测试策略

L1 客户端用 `httptest` 模拟 GitHub，覆盖请求形状、默认 interval、pending、slow down、成功、过期、拒绝和未知错误。会话管理器覆盖重复启动、取消、并发状态读取和成功落盘。Admin API 覆盖参数校验、新源新增、同名 `g` 源更新、非 `g` 名称冲突、token 不回显以及配置写盘触发 reload。涉及 goroutine 的路径运行 race 测试。
