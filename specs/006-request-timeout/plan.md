# Implementation Plan: 单请求最大超时时长配置

**Branch**: `006-request-timeout` | **Date**: 2026-08-20 | **Spec**: [spec.md](spec.md)

**Input**: Feature specification from `/specs/006-request-timeout/spec.md`

## Summary

为每个上游源的单笔请求增加总时长上限（默认 120s，可全局配置并按源覆盖），解决
"源跑 360s 无数据"的挂起问题。实现复用现有 `first_byte_timeout` 所在的
`BreakerCfg` 配置模型（全局默认 + per-source 覆盖 + 校验 + 环境覆盖 + 管理页
字段），在调度器每笔尝试上叠加一个**不会被首事件停止**的总时长定时器；到点后
以可区分的超时原因终止该笔上游调用：未出内容时按既有失败逻辑计入熔断并继续
换源，已出内容时源保持锁定、以 failed 终态收口流；超时与客户端取消在终态、
日志与指标上可区分。

## Technical Context

**Language/Version**: Go 1.26.5

**Primary Dependencies**: 无新增第三方依赖；使用标准库 `context.WithTimeoutCause`
与现有 internal 包

**Storage**: 无

**Testing**: Go 标准测试（表驱动 + 集成 + race），`task check` 门禁

**Target Platform**: Linux / Darwin / Windows 桌面网关进程（跨平台构建不受影响）

**Project Type**: Go 长驻 HTTP 网关服务（`cmd/server`）

**Performance Goals**: 每笔尝试新增一个定时器与一次 cause 判定，转发热路径
（每事件转发）零额外开销

**Constraints**: 总时长 >= 0（缺省/0 = 默认 120s，负值校验拒绝）；不违背首字节
超时既有语义；已出流后超时同样终止（规格澄清结论）；不引入整轮上限

**Scale/Scope**: 单机多源（源数量小）；并发请求各自独立计时

## Constitution Check

*GATE: 必须通过后方可进入 Phase 0，Phase 1 设计完成后复检。*

全部通过，无违规：

1. **配置单一真相源**：新增项走既有 `config.yaml` → `config.Load` →
   `holder.Replace` 原子替换链路；每笔尝试从 `holder.Current()` 快照取值，
   热更新后新发起尝试立即生效，不新增第二条配置入口。
2. **协议事实源**：无新增 wire 字面量；错误终态复用既有 `response.failed`
   （SDK 常量派生）；协议矩阵无需新增字段状态。
3. **分层约束**：配置字段/校验/合并落在 `internal/config`；总时长定时与超时
   判因落在 `internal/scheduler`（每笔尝试边界）；终态补发与状态映射落在
   `internal/server`（整轮收口方）；转换层零改动。
4. **热路径隔离**：每笔尝试一个 timer + 一次 context cause 判定，事件转发路径
   零开销；不影响 EventGate 缓冲、指标非阻塞投递或管理接口。
5. **观测**：超时以 code 504 + reason=timeout 记入日志与指标，与客户端取消
   （499/canceled）可区分；不记录任何凭据。
6. **失败终态语义**：未出内容 → 按既有失败逻辑计熔断并允许换源/整轮重试；
   已出内容 → 源锁定、failed 终态收口、不换源；r 透传不代补上游事件，但网关
   主动中止场景由 server 补发 failed 终态（扩展现有 evCount==0 补发先例）。
7. **测试/文档闭环**：config 默认值、校验、环境覆盖、per-source 合并、调度
   超时各分支、server 终态与 race 全覆盖；`README.md`、`config.example.yaml`、
   `docs/` 同步更新。

## Project Structure

### Documentation (this feature)

```text
specs/006-request-timeout/
├── plan.md              # 本文件（$speckit-plan 输出）
├── research.md          # Phase 0 输出（$speckit-plan 输出）
├── data-model.md        # Phase 1 输出（$speckit-plan 输出）
├── quickstart.md        # Phase 1 输出（$speckit-plan 输出）
├── contracts/           # Phase 1 输出（$speckit-plan 输出）
└── tasks.md             # Phase 2 输出（$speckit-tasks 输出，本阶段不创建）
```

### Source Code (repository root)

```text
cmd/server/main.go                 # 组装不变；HTTP server 配置不使用新字段
internal/config/config.go          # BreakerCfg.RequestTimeout(Duration) + 默认120s
                                   # + validateBreakerNonNegative + applyDefaults
                                   # + BreakerFor 合并 + applyEnvOverrides 白名单
internal/config/config_test.go     # 默认值/校验/环境覆盖/合并测试
internal/scheduler/scheduler.go    # trySourceGeneric 叠加总时长定时器 + 超时判因
                                   # ExecuteGeneric 日志记录超时归因
internal/backend/helpers.go        # ErrUpstreamTimeout 哨兵 + IsServerTimeout
                                   # + IsClientCanceled 排除服务端超时
internal/backend/*_test.go         # a/c/r 已锁定/未锁定超时分支测试
internal/server/server.go          # 超时终态补发（failed）与 504/reason 映射
internal/server/*_test.go          # 超时 vs 取消、空流超时、已出流超时测试
internal/admin/admin.go            # breakerView/input 增加 request_timeout 字段
internal/admin/assets/index.html   # 断路器表单增加 request_timeout 输入
config.example.yaml                # 全局 + per-source 示例与注释
README.md                          # breaker 表格新增一行 + 说明
docs/                              # protocol-coverage.md 变更记录 / 相关设计说明
```

**Structure Decision**: 单仓库 Go 服务，沿用现有分层，不新增包、不新增目录；
所有改动落在既有文件与既有模式（per-source 覆盖合并、admin 视图、env 白名单、
EventGate/终态补发先例）上。

## Complexity Tracking

无 Constitution Check 违规，无需填写。
