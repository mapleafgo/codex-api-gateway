# Research: 管理页 Copilot Device Flow 授权

## 1. OAuth 参数与流程

### Decision
采用 Zed 当前实现：client ID `6e3a0413e62d19d75ff1`，scope `read:user`，device code URL `https://github.com/login/device/code`，access token URL `https://github.com/login/oauth/access_token`，grant type `urn:ietf:params:oauth:grant-type:device_code`。

### Rationale

- 用户明确要求“参照 Zed 做”，不使用 copilot2api 的 VSCode 应用参数。
- Zed 在启动请求发送 JSON Accept 和 form body，响应缺省 interval 为 5 秒。
- Zed 先等待一个 interval 再发起首次 token 请求；pending 继续，`slow_down` 每次 +5 秒，`expired_token` / `access_denied` / 未知错误进入终态。
- 参考版本为 `zed-industries/zed` commit `55007f518bc1d49e6b3291c5eaa1aabf649b36fd`，时间 `2026-08-25T23:21:19Z`，关键文件 `crates/copilot_chat/src/copilot_oauth.rs`。

### Alternatives considered
- copilot2api 的 Client ID 与凭据导入：用户只要前者并要求参照 Zed，已排除。
- 同步阻塞直到授权完成：浏览器请求易超时且刷新页面会失去结果，已排除。
- 引导用户在外部工具取 token 后粘贴：等同现状，不符合需求，已排除。

## 2. Token 交付与保存

### Decision

access token 不返回给浏览器。授权 goroutine 收到 token 后，用 start 提交的目标草稿更新完整 config 快照，然后走现有原子写盘、configwatch、holder reload、scheduler reload 链路。

### Rationale

- 避免 long-lived GitHub 凭据进入管理页 JS 内存、HTTP response、console 或 telemetry。
- 新建源不需要先写入空 token 的半成品配置。
- 已有同名 `g` 源可以在旧 token 保持不变的前提下重新授权。

### Alternatives considered

- status 返回 token 让前端填表保存：扩大暴露面，已排除。
- 新建 pending `g` 源再补 token：中间配置可能无法通过校验，形成第二条半生效状态，已排除。

## 3. 会话生命周期

### Decision

进程内维护唯一会话。活跃态 starting / awaiting_user / saving，终态 authorized / cancelled / error；新 start 可替换终态，不可创建第二个活跃会话。

### Rationale

- GitHub device code 是一次性授权上下文，重复会话让 UI 状态难以归属。
- 页面刷新可由 status 恢复当前公开状态。
- 进程重启丢失未完成会话符合最小实现，不影响已保存凭据。

### Alternatives considered

- 多会话 map：复杂度增加且本地没有多管理员并发需求，已排除。
- 持久化 device flow：把敏感 device code 写入额外存储，重启重试更简单，已排除。

## 4. 目标源语义

### Decision

start 必须携带完整 Copilot 源草稿且不得携带 github_token。名称不存在时新增；同名且类型为 `g` 时更新；同名类型非 `g` 时拒绝；保存时重新读取当前 config 快照并复查冲突。

### Rationale

- 完整草稿避免部分字段被零值覆盖。
- 保存前复查处理 start 后其他管理写入或外部编辑造成的竞态。
- 拒绝非 `g` 名称防止把 Copilot token 写入错误 backend。

## 5. 可测试性与 endpoint 注入

### Decision

L1 授权客户端允许注入 HTTP client 与两个 GitHub endpoint；轮询函数每次只发一次 token 请求并返回下一次 interval，由 admin 会话控制休眠与取消。

### Rationale

- 测试无需真实 GitHub，也不需要真实 sleep 5 秒。
- admin 用 context 取消 pending 循环。
- 一次一请求让 slow down、过期、拒绝等分支容易表驱动验证。
