# Data Model: 管理页 Copilot Device Flow 授权

## DeviceFlow

一次 GitHub device authorization 的内部表示。

| 字段 | 类型 | 约束 |
|------|------|------|
| UserCode | string | 展示给管理界面的短码 |
| VerificationURI | string | 展示地址 |
| deviceCode | string | 私有，仅参与 token 轮询 |
| Interval | duration | 最小 1s，缺省 5s |

由 admin 会话持有；不进入配置或响应。

## AuthSession

进程内唯一活跃授权上下文。

| 字段 | 说明 |
|------|------|
| ID | 递增标识，隔离旧 goroutine 迟到结果 |
| State | starting / awaiting_user / saving / authorized / cancelled / error / idle |
| TargetName | 保存目标 |
| Draft | 完整 `g` 源定义，不含 token |
| Flow | L1 DeviceFlow |
| PublicError | 不含凭据的错误 |
| Context / Cancel | 取消轮询 |

活跃态 starting / awaiting_user / saving；终态 authorized / cancelled / error。idle 表示无会话。新 start 替换终态，不替换活跃态。

## 公开授权状态

| 字段 | 说明 |
|------|------|
| state | 会话状态 |
| user_code | awaiting_user 起返回 |
| verification_uri | awaiting_user 起返回 |
| interval_seconds | 前端轮询间隔 |
| source_name | 目标源 |
| error | 终态错误，可为空 |

禁止字段：device code、access token、authorization header、原始响应敏感值。

## Copilot 源草稿

沿用管理页 source view 完整形状：name、base_url、api_key、backend_type、model_map、default_model、breaker、disabled、headers、supports_web_search。

校验：

- name 去空白必填。
- backend_type 规范化后为 `g`。
- base_url 对 `g` 可选。
- headers 走现有保留头过滤和键名校验。
- 请求中 github_token 必须为空；token 由 Device Flow 提供。
- 保存前重新对比同名源 backend_type。

## 状态迁移

```text
idle --start--> starting --device-code-ok--> awaiting_user
starting --request-error--> error
awaiting_user --cancel--> cancelled
awaiting_user --token--> saving --save-ok--> authorized
saving --save/reload/sync-error--> error
awaiting_user --expired/denied/network/unknown--> error
authorized|cancelled|error --new-start--> starting
```
