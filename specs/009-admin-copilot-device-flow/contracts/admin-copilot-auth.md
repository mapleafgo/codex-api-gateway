# Contract: Admin Copilot Authorization

所有接口挂在 `/admin/api/*` 下，接受与返回 `application/json`。错误体沿用 `{ "error": "...", "detail": "..." }`。

## POST /admin/api/copilot/auth/start

```json
{
  "source": {
    "name": "copilot",
    "backend_type": "g",
    "base_url": "",
    "api_key": "",
    "default_model": "",
    "model_map": {},
    "headers": {},
    "disabled": false,
    "supports_web_search": true,
    "github_token": ""
  }
}
```

规则：

- `source.name` 必填，`source.backend_type` 规范化后必须是 `g`。
- `source.github_token` 非空返回 400。
- 目标不存在时成功保存后新增；同名 `g` 源成功后更新；同名非 `g` 源立即 409。
- 无活跃会话时启动并返回 200 公开状态。
- 活跃会话存在时返回 409 和当前公开状态。
- device-code 请求失败返回 200，公开状态 state 为 `error`。

## GET /admin/api/copilot/auth/status

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

始终 200；终态保持到下一次会话；不返回 device code 或 access token。

## POST /admin/api/copilot/auth/cancel

- 可取消的 starting / awaiting_user 会话取消轮询并返回当前状态。
- saving 已进入不可中断的本地保存阶段，返回 409。
- 终态或 idle 幂等返回当前状态。

## 内部 L1 契约

```go
func StartDeviceFlow(ctx context.Context, hc *http.Client) (*DeviceFlow, error)
func PollDeviceFlow(ctx context.Context, hc *http.Client, flow *DeviceFlow) (token string, nextInterval time.Duration, err error)
```

- StartDeviceFlow 使用固定 GitHub URL 和可注入 HTTP client。
- PollDeviceFlow 只发一次请求。
- pending 时 token 为空、err 为 nil，interval 保持。
- slow down 时 interval = 旧 interval + 5 秒。
- 成功返回 token；其余结果返回不含凭据的错误。
