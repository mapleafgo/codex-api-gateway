# Implementation Plan: GitHub Copilot 原生后端接入

**Branch**: `008-copilot-backend` | **Date**: 2026-08-25 | **Spec**: [spec.md](spec.md)

**Input**: Feature specification from `specs/008-copilot-backend/spec.md`

## Summary

新增 `backend_type: g`（GitHub Copilot）作为第四种上游协议适配类型。`g` 后端是一个协议分发器：通过 `internal/copilotclient` 执行 GraphQL endpoint 发现并拉取/筛选模型目录（参照 Zed），然后按模型的 `supported_endpoints` 以优先级 r > a > c 委托给已有的 `ResponsesBackend` / `AnthropicBackend` / `ChatBackend` 执行协议转换与流式转发。认证参照 Zed 实现——直接用 GitHub OAuth token 作为 Bearer，不换 Copilot session token。

## Technical Context

**Language/Version**: Go 1.26.5（与当前 go.mod 一致）

**Primary Dependencies**: 标准库 `net/http`、`log/slog`、`encoding/json`；`golang.org/x/sync/singleflight`（模型目录缓存去重，go.mod 直接依赖）；现有 `internal/backend`、`internal/copilotclient`、`internal/config`、`internal/responsesclient`、`internal/upstreamhttp`

**Storage**: N/A（无持久化；模型目录缓存在内存，token 来自配置）

**Testing**: Go 标准测试框架，表驱动 + `httptest.Server` mock 上游；涉及缓存的用 `task test-race`

**Target Platform**: Linux/macOS/Windows 本地服务（与现有网关一致）

**Project Type**: Go web-service（HTTP 网关）

**Performance Goals**: `/v1/*` 热路径不阻塞——GraphQL 发现和模型目录拉取是 per-source 惰性初始化，不阻塞启动；缓存命中时路由决策为内存查找

**Constraints**: 分层单向依赖（`backend` 不得 import `scheduler`/`server`）；凭据不入日志；仅流式

**Scale/Scope**: 单 `g` 源为主，支持多 `g` 源共存（per-source 独立状态）

## Constitution Check

*GATE: Must pass before Phase 0 research. Re-check after Phase 1 design.*

| 原则 | 状态 | 说明 |
|------|------|------|
| I. 产品边界与协议透传 | ✅ 通过 | `g` 后端是分发器，委托已有的 r/a/c 转换路径，不新增协议转换逻辑；形状透传原则不变 |
| II. 协议事实源与官方 SDK | ✅ 通过 | 协议事实参照 Zed `copilot_chat.rs`；模型目录字段名是 Copilot wire 事实 |
| III. 分层单向依赖 | ✅ 通过 | endpoint 发现与模型目录放在 `internal/copilotclient`（L1）；`CopilotBackend` 放在 `internal/backend`（L2.5）委托同层 Backend；admin 旁路只使用 L1 client，不持有 Backend/scheduler |
| IV. 配置单一真相源 | ✅ 通过 | `github_token` 通过 `config.Source` 字段配置，走 holder 热重载链路 |
| V. 调度可用性 | ✅ 通过 | `g` 源参与 scheduler 的源选择/熔断/故障转移，与 a/c/r 同等 |
| VI. 热路径隔离 | ✅ 通过 | GraphQL 发现和模型缓存是惰性 per-source，缓存命中时为内存查找 |
| VII. 测试、文档与最小增量 | ✅ 通过 | 新增 L1 client 与 L2.5 分发器文件，测试同目录；复用已有模式；config 同步更新 |

无违规，不需要 Complexity Tracking。

## Project Structure

### Documentation (this feature)

```text
specs/008-copilot-backend/
├── plan.md              # 本文件
├── research.md          # Phase 0: Zed 协议契约调研
├── data-model.md        # Phase 1: 数据模型
├── quickstart.md        # Phase 1: 验证指南
├── contracts/           # Phase 1: 接口契约
│   └── copilot-api.md   # Copilot API 认证/模型/端点契约
└── tasks.md             # Phase 2: 任务列表（$speckit-tasks 生成）
```

### Source Code (repository root)

```text
internal/
├── config/
│   └── config.go              # 新增 BackendGitHubCopilot 常量、Source.GithubToken 字段、NormalizeBackendType 校验
├── backend/
│   ├── copilot.go             # 【新增】CopilotBackend：模型能力路由 + r/a/c 分发
│   └── copilot_test.go        # 【新增】单元测试
├── copilotclient/
│   ├── client.go              # 【新增】per-source endpoint/model 状态与公开目录 API
│   ├── endpoint.go            # 【新增】GraphQL endpoint 发现
│   ├── endpoint_test.go       # 【新增】发现、fallback 与 per-source 缓存测试
│   ├── models.go              # 【新增】/models 拉取、筛选、singleflight 缓存
│   └── models_test.go         # 【新增】模型目录与并发缓存测试
├── scheduler/
│   └── scheduler.go           # 新增 copilotBackend 字段 + backendFor 分支
├── admin/
│   ├── admin.go / convert.go  # sourceView/新增源/全量保存保留 github_token，响应不回显 token
│   └── assets/index.html      # 管理页 g 类型、GitHub Token 字段与指标标签
├── metrics/
│   └── metrics.go             # backend_type 注释与观测口径纳入 g
├── server/
│   └── server.go              # 预转换分支加 g 处理（如有需要）
├── config.example.yaml         # 新增 g 源示例
└── docs/protocol-coverage.md   # 新增 g 专节与变更记录
```

**Structure Decision**: Copilot 的低层 HTTP 能力集中在 `internal/copilotclient`（L1），`CopilotBackend`（L2.5）组合该 client 与已有三个 Backend 做委托；admin 试拉模型也复用 L1 client，避免管理旁路持有转发组件。config 和 scheduler 的改动是最小增量（加常量/字段/分支）。

## Complexity Tracking

无 Constitution Check 违规，不需要。
