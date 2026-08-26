# Implementation Plan: 插件式上游源架构

**Branch**: `010-plugin-source-architecture` | **Date**: 2026-08-26 | **Spec**: [spec.md](spec.md)

**Input**: Feature specification from `specs/010-plugin-source-architecture/spec.md`

## Summary

把固定 `backend_type=a/c/g/r` 短码分发改造为插件式源架构。新增共享契约包 `internal/plugin` 定义源插件接口与不可变注册表；Anthropic、OpenAI Chat、OpenAI Responses、GitHub Copilot 四类内置上游改为四个独立插件实现，Copilot 相关能力全部收进单一插件边界。调度、服务编排、配置核心、管理框架与健康框架只依赖插件契约与注册表，不再判断具体源身份或字段。配置做一次性破坏性升级：源用稳定 `backend` 标识 + 类型专属 `options`；旧 `backend_type` 表达、通用区专属字段和旧短码校验全部拒绝，热重载仍走唯一磁盘链路。

## Technical Context

**Language/Version**: Go 1.26.5（与 `go.mod` 一致）

**Primary Dependencies**: 标准库 `net/http`、`context`、`encoding/json`、`sync`、`time`、`log/slog`；`koanf`/`yaml.v3` 配置解析；`github.com/openai/openai-go/v3` 与 `github.com/anthropics/anthropic-sdk-go` 协议常量；现有 `convert`/`streamconv`/`chatconvert`/`chatstreamconv` 作为共享协议引擎保留。

**Storage**: `config.yaml` 仍是唯一持久化真相源；插件注册表进程内不可变，热重载链路保持磁盘 → `config.Load` → `holder.Replace` → `scheduler.Reload`。

**Testing**: Go 标准测试、同目录表驱动测试、`httptest.Server` mock；架构守护用 `go list -deps` 与 AST 扫描断言共享目录不 import 插件实现；并发/缓存/授权会话用 `task test-race`。

**Target Platform**: Linux/macOS/Windows 本地网关与浏览器管理页（与现有一致）。

**Project Type**: Go HTTP 网关 + 内嵌管理页。

**Performance Goals**: `/v1/*` 热路径只依赖契约接口与不可变注册表查找；插件内部缓存保持惰性 per-source，不阻塞启动或转发。

**Constraints**: 分层单向依赖，共享核心禁止 import 具体插件实现；凭据不入日志/指标/管理响应；协议透传与终态/failover 语义不变；管理页保持旁路。

**Scale/Scope**: 四个内置插件迁移；管理页从硬编码四类表单改为按描述符渲染；新增测试源插件只改自身包与组装入口。

## Constitution Check

*GATE: Must pass before Phase 0 research. Re-check after Phase 1 design.*

| 原则 | 状态 | 说明 |
|------|------|------|
| I. 产品边界与协议透传 | 通过 | 插件契约只做形状透传和能力声明；上游裁决保持不变，终态/failover 语义不动 |
| II. 协议事实源与官方 SDK | 通过 | 协议字面量仍来自 SDK 常量；Copilot 委托复用既有协议路径，不新增重复矩阵行 |
| III. 分层单向依赖与唯一组装入口 | 通过 | 共享核心只依赖 `internal/plugin` 契约；具体插件只在 `cmd/server` 注册；分发型插件通过宿主契约按 ID 委托 |
| IV. 配置单一真相源与原子生效 | 通过 | 破坏性升级仍走唯一写盘与原子替换链路；注册表通过注入的窄接口参与加载校验，不另立配置入口 |
| V. 调度可用性 | 通过 | scheduler 通过注册表获取 Backend 与流式模式；EventGate 按描述符的透传/转换模式判定 |
| VI. 热路径隔离与结构化可观测 | 通过 | 指标/管理旁路保持非阻塞；观测字段从短码 `backend_type` 改为稳定 `backend`，Token 归一化下沉到插件 |
| VII. 测试、文档与最小增量 | 通过 | 配置、协议矩阵、README、示例配置、架构守护测试同步；旧文档/代码迁移路径在 tasks 中显式登记 |

无 Constitution Check 违规，不需要 Complexity Tracking。

## Project Structure

### Documentation (this feature)

```text
specs/010-plugin-source-architecture/
├── plan.md              # 本文件
├── research.md          # Phase 0：选型与破坏性升级决策
├── data-model.md        # Phase 1：插件契约、注册表与配置模型
├── quickstart.md        # Phase 1：端到端验证指南
├── contracts/
│   ├── plugin-contract.md   # 源插件接口、宿主委托与注册表契约
│   ├── config-v2.md         # 新配置形状、校验注入与迁移错误契约
│   ├── admin-api.md         # 管理页动态 schema、脱敏与扩展动作 API
│   └── observability.md     # backend 观测字段、Token 归一化与敏感值约束
└── tasks.md             # Phase 2：$speckit-tasks 生成
```

### Source Code (repository root)

```text
internal/
├── plugin/
│   ├── descriptor.go    # Descriptor、Schema、Field、Action、StreamingKind
│   ├── registry.go      # Registry、窄接口、注入式校验
│   ├── backend.go       # Backend、UpstreamEvent、RequestPreparer、流式模式
│   ├── catalog.go       # Model、ModelCatalog、HealthProbe、探测结果
│   └── admin.go         # AdminExtension、ActionRequest、ActionResult、脱敏哨兵
├── plugins/
│   ├── anthropic/       # Anthropic Messages 插件
│   ├── openaichat/      # OpenAI Chat Completions 插件
│   ├── openairesponses/ # OpenAI Responses 透传插件
│   └── copilot/         # GitHub Copilot 插件：endpoint 发现、模型目录、路由、Device Flow、连通性探测、管理动作
├── config/              # 破坏性升级：Source.Backend + Options；移除 BackendType/GithubToken/AnthropicCfg 顶层项
├── scheduler/           # 只依赖 plugin.Registry 与 Descriptor；移除具体 backend 分支
├── server/              # 预检/日志/指标字段改为按能力描述符与稳定 backend
├── admin/               # 通用源表单、脱敏、动作与 catalog/probe 分派；移除 Copilot 专属路由与字段
├── health/              # 通过 HealthProbe 统一分派；移除 backend_type 探测分支
├── metrics/             # RequestEvent.Backend；移除短码归一化分支
├── convert|streamconv|chatconvert|chatstreamconv   # 共享协议引擎，被对应插件使用
└── model/               # 共享协议类型不变
cmd/server/main.go       # 唯一组装入口：构造插件、Registry、注入配置校验与宿主
config.example.yaml      # 新形状示例与迁移错误说明
docs/protocol-coverage.md# 源身份模型与观测字段同步
README.md                # 四类内置源的新配置说明
```

**Structure Decision**: 契约层放在 `internal/plugin`；具体实现收进 `internal/plugins/<id>`；`internal/copilotclient` 全部并入 `internal/plugins/copilot`；共享协议引擎保留为协议层，被对应插件组合。`cmd/server` 是唯一注册入口，并注入宿主让分发型插件按稳定 ID 委托其他 Backend。

## Complexity Tracking

无 Constitution Check 违规，不需要 Complexity Tracking。
