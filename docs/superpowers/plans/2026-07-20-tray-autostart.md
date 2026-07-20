# 托盘开机自启 Implementation Plan

> **For agentic workers:** 按任务顺序实现；每任务含测试。

**Goal:** 托盘增加可勾选「开机自启」，Linux/Windows/macOS 纯 Go 实现。

**Architecture:** `internal/autostart` 平台文件 + `internal/tray` 勾选菜单重建；`cmd/server` 注入 Spec。

**Tech Stack:** Go 标准库 + `golang.org/x/sys/windows/registry`（仅 Windows）；`gogpu/systray` AddCheckbox。

## Global Constraints

- `CGO_ENABLED=0` 全平台交叉编译不可破坏
- 日志只用 `log/slog`
- AppID 固定 `codex-api-gateway`，与 packaging desktop 同名
- 交互语言与注释用中文

## 任务

### Task 1: internal/autostart 核心 + Linux
### Task 2: Darwin + Windows
### Task 3: tray 菜单接入
### Task 4: main 注入 + README/packaging 对齐
### Task 5: task check + 本地验证
