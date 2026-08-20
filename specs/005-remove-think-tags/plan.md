# Implementation Plan: 剔除正文思维标签处理

**Branch**: `005-remove-think-tags` | **Date**: 2026-08-20 | **Spec**: [spec.md](spec.md)

**Input**: Feature specification from `specs/005-remove-think-tags/spec.md`

## Summary

c 路径（Chat 上游 → Responses SSE）不再解析正文中的 `<thinking>` / `</thinking>` 思维
标签：删除 `chatstreamconv` 的标签状态机（`feedContentThink`、`flushThinkEnd`、
`resetThinkEnd`、标签前缀匹配 helpers 及 struct 状态字段），content 一律按普通正文
原样透传；`delta.reasoning_content` 等独立推理字段的既有映射保持不变；同步更新
`docs/protocol-coverage.md` 变更记录与出站矩阵，删除 `think_test.go`。

## Technical Context

**Language/Version**: Go（module `codex-api-gateway`，标准工具链）

**Primary Dependencies**: `github.com/openai/openai-go/v3`（协议常量）、标准库
`encoding/json` / `strings` / `log/slog`

**Storage**: N/A（无状态转换层）

**Testing**: Go 标准测试；`task check`（gofmt + go vet + 全量测试）、
`task test-race`、`golangci-lint run ./...`

**Target Platform**: Linux 常驻服务（`cmd/server`）

**Project Type**: 协议网关（Go 服务）

**Performance Goals**: `/v1/*` 热路径延迟敏感；本变更删除解析逻辑，不引入额外开销

**Constraints**: 只影响 c 路径出站正文；`a`/`r` 路径不动；保留 `reasoning_content`
字段级映射；避免无关重构

**Scale/Scope**: 单包 `internal/chatstreamconv` 删除型改动 + 矩阵文档同步

## Constitution Check

*GATE: Must pass before Phase 0 research. Re-check after Phase 1 design.*

| 章程原则 | 符合情况 |
|---|---|
| I 产品边界与协议透传 | 正文原样透传属于形状透传，不替上游裁决能力；满足 |
| II 协议事实源与官方 SDK | 变更同步更新 `docs/protocol-coverage.md` 矩阵与变更记录；满足 |
| III 分层单向依赖 | 只在 L2 转换层 `chatstreamconv` 内删除，无跨层引用；满足 |
| VII 测试、文档与最小增量 | 同步删测试/文档，只做删除型最小增量，无无关重构；满足 |
| 静默跳过约定 | 删除后正文不再被改写，不存在静默丢弃面；满足 |

## Project Structure

### Documentation (this feature)

```text
specs/005-remove-think-tags/
├── plan.md              # This file
├── research.md          # Phase 0 output
├── data-model.md        # Phase 1 output
├── quickstart.md        # Phase 1 output
├── contracts/           # Phase 1 output
└── tasks.md             # Phase 2 output（$speckit-tasks 生成）
```

### Source Code (repository root)

```text
internal/chatstreamconv/
├── converter.go         # 删除标签状态机与 helpers；content 直接 feedText
├── converter_test.go    # 保留原生 reasoning 相关测试，不改
└── think_test.go        # 整文件删除（标签解析测试）

docs/
└── protocol-coverage.md # 变更记录新增 2026-08-20 条目；出站矩阵保持“原样透传”语义
```

**Structure Decision**: 单层单文件删除型改动，沿用既有包结构，不新建目录。

## Complexity Tracking

> 无章程违规，无需记录。
