# Implementation Plan: 管理页 Copilot Device Flow 授权

**Branch**: `009-admin-copilot-device-flow` | **Date**: 2026-08-26 | **Spec**: [spec.md](spec.md)

**Input**: Feature specification from `specs/009-admin-copilot-device-flow/spec.md`

## Summary

在管理页新增 GitHub Copilot Device Flow 登录授权。低层客户端按 Zed 当前实现请求 device code、展示 user code、按外部 interval 轮询 access token；admin 旁路维护唯一活跃会话，成功后把 token 写入目标 `g` 源，并复用配置原子写盘与热重载链路。前端只接收可公开状态，不接收 device code 或 access token。

## Technical Context

**Language/Version**: Go 1.26.5；管理页沿用现有 Alpine.js 单文件资产

**Primary Dependencies**: 标准库 `net/http`、`context`、`encoding/json`、`sync`、`time`、`log/slog`；现有 `internal/copilotclient`、`internal/admin`、`internal/config`

**Storage**: `config.yaml`（唯一持久化凭据位置）；活跃 Device Flow 会话仅在内存

**Testing**: Go 标准测试框架、同目录表驱动测试、`httptest.Server` mock GitHub 与 admin API；并发路径使用 `task test-race`

**Target Platform**: Linux/macOS/Windows 本地网关与浏览器管理页

**Project Type**: Go HTTP 服务 + 内嵌 H5 管理页

**Performance Goals**: 不影响 `/v1/*` 转发路径；Device Flow 只在后台 goroutine 轮询，状态查询为锁内快照读取

**Scale/Scope**: 本地管理员单人操作为主，但必须安全处理并发 API 调用和页面刷新

## Constitution Check

*GATE: Must pass before Phase 0 research. Re-check after Phase 1 design.*

| 原则 | 状态 | 说明 |
|------|------|------|
| I. 产品边界与协议透传 | ✅ 通过 | 只新增认证入口，不新增协议转换、能力裁决或非流式路径 |
| II. 协议事实源与官方 SDK | ✅ 通过 | OAuth wire 契约以 Zed 当前源码为事实源；不属于 OpenAI/Anthropic SDK 字段矩阵范围 |
| III. 分层单向依赖 | ✅ 通过 | Device Flow 放在 `internal/copilotclient` L1；admin 只组合 L1 并编排会话/写盘 |
| IV. 配置单一真相源 | ✅ 通过 | token 通过完整 config 快照校验后原子写盘，再触发既有 reload |
| V. 调度可用性 | ✅ 通过 | 授权失败不改旧配置；写盘 reload 后由既有调度器加载新源/token |
| VI. 热路径隔离 | ✅ 通过 | admin 保持旁路；轮询 goroutine 不进入 `/v1/*`，状态接口被 recover 包裹 |
| VII. 测试、文档与最小增量 | ✅ 通过 | 新增 focused client/session/API/UI 测试；README 和规格同步，无新增配置项 |

无违规，不需要 Complexity Tracking。

## Project Structure

### Documentation (this feature)

```text
specs/009-admin-copilot-device-flow/
├── plan.md              # 本文件
├── research.md          # Zed 授权调研与设计决策
├── data-model.md        # 会话与目标源模型
├── quickstart.md        # 浏览器与自动化验证指南
├── contracts/
│   └── admin-copilot-auth.md # 管理 API 与内部授权客户端契约
└── tasks.md             # $speckit-tasks 生成
```

### Source Code (repository root)

```text
internal/copilotclient/
├── auth.go              # Zed 式 Device Flow 请求与一次轮询
└── auth_test.go         # httptest 表驱动覆盖所有 OAuth 结果
internal/admin/
├── copilot_auth.go      # 唯一活跃会话、取消、公开状态、成功落盘
├── copilot_auth_test.go # 会话/API/落盘/并发测试
├── admin.go             # 挂载三个 auth 路由并组合会话依赖
└── assets/index.html    # g 源授权按钮、用户码面板、轮询、双语文案
README.md                # 管理 Copilot 源的授权说明
```

**Structure Decision**: 复用 `008` 已建立的 L1 `copilotclient` 与 L5 admin 边界。授权 HTTP 细节留在 L1；admin 只负责生命周期和调用既有配置保存方法。

## Complexity Tracking

无 Constitution Check 违规，不需要 Complexity Tracking。
