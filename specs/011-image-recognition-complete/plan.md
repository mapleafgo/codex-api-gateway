# Implementation Plan: 图片识别三协议完全可用

**Branch**: `011-image-recognition-complete` | **Date**: 2026-08-31 | **Spec**: [spec.md](spec.md)

**Input**: Feature specification from `/specs/011-image-recognition-complete/spec.md`

**Note**: This template is filled in by the `$speckit-plan` command; its definition describes the execution workflow.

## Summary

在网关图片识别（`input_image`）现有映射基础上，补齐三协议（Anthropic `a` / Chat `c` /
Responses 透传 `r`）的完全可用性：新增统一的图片映射判定层 `internal/imagemapper`，
统一处理 URL / data-URI / file_id / detail 与所在角色，供 a/c 两路径消费；detail 在
有槽位协议保留、无槽位协议有损丢弃并登记矩阵；file_id 与系统/开发者指令图片在非透传
源按源级失败进入既有换源流程；日志对图片地址脱敏；协议覆盖矩阵同步登记。

## Technical Context

**Language/Version**: Go 1.26.5

**Primary Dependencies**: `github.com/openai/openai-go/v3` v3.50.0（Responses / Chat SDK）、
`github.com/anthropics/anthropic-sdk-go` v1.63.0（Anthropic SDK）。均为既有依赖，不新增。

**Storage**: N/A（网关无 session store；图片不在网关侧缓存）

**Testing**: Go 标准测试框架，`task test` / `task test-race` / `task check`

**Target Platform**: Linux / macOS / Windows 服务端（跨平台纯 Go）

**Project Type**: 协议转换网关（Go 服务，`cmd/server` 组装入口）

**Performance Goals**: 图片映射为纯内存判定，不引入网络或 I/O，不改变既有转发延迟

**Constraints**: 分层单向依赖（imagemapper 作为 L2 转换层共享包，只依赖 openai-go 与
slog）；wire 文本必须英文；协议常量对齐官方 SDK；热路径不做阻塞

**Scale/Scope**: 三条上游路径（a/c/r）的图片识别转换，单包内收敛映射判定

## Constitution Check

*GATE: Must pass before Phase 0 research. Re-check after Phase 1 design.*

- **协议事实源与官方 SDK（II）**: detail / file_id / system 图片支持均以官方 SDK 类型
  为准（已调研 openai-go v3.50.0 与 anthropic-sdk-go v1.63.0）；SDK 有字段时优先透传，
  无等价字段在矩阵登记准确状态。✓ 本设计全部满足。
- **产品边界与协议透传（I）**: 仅 file_id 图片在非透传源是协议不可映射（网关无 Files
  凭据），属允许拒绝场景；不替上游裁决模型能力。✓
- **转换保真：禁止有损降级（VIII，新增）**: 非透传源遇到无法无损承载的图片输入一律
  源级失败+换源，禁止丢弃后发残缺请求；detail 仅控制字段有损丢弃且图像本体保留。✓
- **热路径隔离与结构化可观测（VI）**: 图片日志脱敏、slog 结构化、WARN 只用于关键丢弃
  （本次 a/c 路径 file_id 与 system 图片改为源级失败，不再 WARN 后发残缺请求）。✓
- **测试、文档与最小增量（VII）**: 表驱动测试靠近实现、矩阵同步、`task check` 门禁。✓

## Project Structure

### Documentation (this feature)

```text
specs/011-image-recognition-complete/
├── plan.md              # This file ($speckit-plan command output)
├── research.md          # Phase 0 output ($speckit-plan command)
├── data-model.md        # Phase 1 output ($speckit-plan command)
├── quickstart.md        # Phase 1 output ($speckit-plan command)
├── contracts/           # Phase 1 output ($speckit-plan command)
└── tasks.md             # Phase 2 output ($speckit-tasks command - NOT created by $speckit-plan)
```

### Source Code (repository root)

```text
internal/
├── imagemapper/          # 新增：统一图片映射判定层（本次核心新增）
│   ├── decision.go       #   Decision / Kind / Inspect* 判定入口
│   ├── sanitize.go       #   SanitizeURL 日志脱敏
│   ├── decision_test.go  #   判定表驱动测试
│   └── sanitize_test.go  #   脱敏测试
├── convert/              # a 路径（Anthropic）：改造 input_image / system / tool 图片分支
│   └── request.go
├── chatconvert/          # c 路径（Chat）：改造 input_image / system / tool 图片分支
│   └── request.go
└── model/                # 仅复用既有常量（不新增类型）

docs/
└── protocol-coverage.md  # 图片识别 / detail / file_id / system 图片矩阵登记
```

**Structure Decision**: 按方案 B 提取统一职责层 `internal/imagemapper`，作为 L2 转换层
共享包，供 `convert`（a）与 `chatconvert`（c）共同消费；`r`（Responses 透传）不调用，
仅在需要时复用脱敏工具。依赖方向：`imagemapper` 只依赖 openai-go + slog，不依赖
anthropic SDK 或 chatconvert 类型，避免环依赖。

## Complexity Tracking

> **Fill ONLY if Constitution Check has violations that must be justified**

| Violation | Why Needed | Simpler Alternative Rejected Because |
|-----------|------------|-------------------------------------|
| 无违反 | - | - |
