# Quickstart: 单请求最大超时时长配置

## 前置

- 仓库根目录 `/home/mapleafgo/Projects/OpenProject/codex-api-gateway`
- Go 工具链与 Task 可用；本机 `config.yaml` 配置两个源（一个正常、一个可加速
  复现超时行为的模拟上游）

## 验证场景

### 1. 配置与默认值（单元测试）

```bash
go test ./internal/config -count=1 -run 'RequestTimeout'
```

预期：缺省/0 → 120s；负值报 `must be >= 0`；`sources[].breaker` 覆盖生效且
未覆盖源继承全局；env 覆盖 `CODEX_API_GATEWAY_BREAKER__REQUEST_TIMEOUT=90s`
生效。

### 2. 未出流的单笔超时 → 换源

配置全局 `breaker.request_timeout: 5s`，首源为"连接后只发一个状态事件然后静默"
的模拟上游，备用源正常。发起 `/v1/responses` 请求：

```bash
curl -N http://127.0.0.1:8383/v1/responses -H 'content-type: application/json' -d @req.json
```

预期：约 5s 首源被终止并按失败计入熔断，请求切换到备用源并正常完成；日志出现
`reason=timeout` 且 code 504；客户端最终收到合法终态，无 360s 挂起。

### 3. 已出流的单笔超时 → failed 终态收口

模拟上游正常出流但总时长超过配置值（如配置 3s、上游持续输出 >3s）。发起请求。

预期：约 3s 时流被终止，客户端保留已收到内容并收到 `response.failed` 收尾终态，
不再收到后续事件；源保持锁定、不换源。

### 4. 超时与客户端取消可区分

分别：a) 上游挂起直至超时；b) 发起后立刻 Ctrl-C 断开 curl。

预期：a) 日志/指标 `reason=timeout` + code 504 + status failed；b) 既有
`canceled` / 499 路径，两者互不串扰。

### 5. 每源覆盖

`sources[].breaker.request_timeout: 3s` 覆盖全局 120s，对覆盖源发起挂起请求。

预期：该源约 3s 终止；未覆盖源仍按 120s 执行；管理页
`GET /admin/api/config` 的 `breaker.request_timeout`（全局与每源）回显正确，
POST 保存后热更新生效（在途尝试旧值、新尝试新值）。

### 6. 全量门禁

```bash
task check
task test-race
```

预期：全部通过。

## 契约与数据模型

- Wire/终态契约：[contracts/contract.md](contracts/contract.md)
- 数据模型与校验规则：[data-model.md](data-model.md)
