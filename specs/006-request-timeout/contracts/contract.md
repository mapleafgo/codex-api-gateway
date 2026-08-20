# Contract: 单笔源请求总时长超时行为

## 场景

`/v1/responses`（客户端）→ 网关 → 任一后端（a/c/r）。配置
`breaker.request_timeout` 决定单个源单笔上游调用的总时长上限（默认 120s，可
per-source 覆盖）。

## 配置契约

```yaml
breaker:
  request_timeout: 120s   # 全局默认；0/缺省 = 120s，负值拒绝
sources:
  - name: example
    breaker:
      request_timeout: 60s  # 可选覆盖；0/缺省继承全局
```

- 时长按单个源单笔上游调用计算，不按客户端请求整轮累计。
- 环境覆盖白名单：`CODEX_API_GATEWAY_BREAKER__REQUEST_TIMEOUT`。
- 管理页 `GET/POST /admin/api/config` 的 `breaker.view`/`sources[].breaker`
  均携带该字段。

## 行为契约（wire）

| 场景 | 结果 | 状态/码 |
|---|---|---|
| 超时到点，未向客户端写出任何事件 | 该源按失败计入熔断，按既有顺序换源；全部失败时客户端收到错误终态 | failed / 504（整轮无源成功时） |
| 超时到点，已写出事件（含已出流） | 保留已收内容，`response.failed` 收尾终态，源锁定不换源 | failed / 504 |
| 超时到点，上游中途静默（首事件后无内容） | 未锁定则失败+换源；已锁定（有内容）则 failed 收口 | 同上 |
| 客户端取消早于超时 | 既有 canceled 语义 | canceled / 499 |

规则：

- 服务端超时 MUST 与客户端取消在终态、日志（`reason=timeout`）与指标
  （Code=504）中可区分。
- r 透传不代补上游事件；网关主动中止导致的缺终态由 server 补发 `response.failed`
  （扩展现有 evCount==0 补发分支，条件改为"流内尚未出现终态事件"）。
- 首字节超时语义 MUST 不变：仍约束"等待首个事件"并触发该源失败切换；单笔总
  时长约束"该笔请求全程"，两个机制按先到先生效。
- 整轮时长不设上限（规格 Assumptions），随源数与每笔耗时累加。
