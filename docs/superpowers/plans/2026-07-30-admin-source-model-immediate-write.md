# 管理页供应商/模型即时落盘与侧栏导航 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 弹窗新增与删除供应商/模型立即写盘热重载；左侧菜单下增加供应商 TOC（滚动高亮、调序同步）；字段编辑仍走底部保存。

**Architecture:** 在 `internal/admin` 新增四条局部写盘 API（基于 `holder.Current()` + `writeMu` + `writeConfigYAMLLocked`），前端 `addSource`/`addModel`/`removeSource`/`removeModel` 改调这些 API 并 patch `cfgSnapshot`；sources 页在 `.ui-side` 菜单下与窄屏顶栏共用 TOC 状态。

**Tech Stack:** Go net/http admin handlers、Alpine.js 单页 `assets/index.html`、go:embed。

**Spec:** `docs/superpowers/specs/2026-07-30-admin-source-model-immediate-write-design.md`

## Global Constraints

- 局部写盘不得合并前端全量 dirty `cfg`
- 增删成功后同步 patch `cfgSnapshot`，避免误报 dirty
- 删除身份优先用 snapshot 已落盘 name/slug
- 不改 `/v1/*` 转发路径

---

### Task 1: 后端 sources 增删 API

**Files:**
- Modify: `internal/admin/admin.go`（路由 + handlers）
- Modify: `internal/admin/admin_test.go`

**Interfaces:**
- Produces: `POST /admin/api/sources`, `POST /admin/api/sources/delete`

- [ ] **Step 1: 写失败测试**（缺字段/重名/unknown/成功路径）
- [ ] **Step 2: 实现 `handleAddSource` / `handleDeleteSource`**，Mount 注册路由
- [ ] **Step 3: `go test ./internal/admin/ -count=1 -run 'SourceAdd|SourceDelete|SourceDisabled'`**
- [ ] **Step 4: Commit** `feat(admin): 供应商新增删除即时写盘 API`

实现要点：
- add：append 到 Sources 末尾；`model_map` 空；Validate + writeConfigYAMLLocked；返回 ok+health
- delete：按 name 过滤；unknown → 400

---

### Task 2: 后端 models 增删 API

**Files:**
- Modify: `internal/admin/admin.go`
- Modify: `internal/admin/admin_test.go`

**Interfaces:**
- Produces: `POST /admin/api/models`（body 含 slug 等）、`POST /admin/api/models/delete`
- 注意：已有 `GET/POST?` `/admin/api/models` 用于上游拉取——检查现有 `handleModels` 路由冲突

现有路由：`mux.HandleFunc("/admin/api/models", wrap("models", h.handleModels))` 是按 source 拉上游模型。
**冲突处理：** 新建管理配置模型用：
- `POST /admin/api/models/add`
- `POST /admin/api/models/delete`

（与 spec 略调：避免覆盖现有 `/admin/api/models` 上游列表接口。sources 仍用 `/admin/api/sources` 因该 path 目前未占用。）

- [ ] **Step 1: 测试 add/delete model**
- [ ] **Step 2: 实现 handlers**（拷贝 ModelOverrides map + ModelSlugOrder slice）
- [ ] **Step 3: 跑测试**
- [ ] **Step 4: Commit** `feat(admin): 模型新增删除即时写盘 API`

---

### Task 3: 前端弹窗新增/删除即时落盘

**Files:**
- Modify: `internal/admin/assets/index.html`

- [ ] **Step 1: `addSource`/`addModel` 调 API，成功再 push + patchSnapshot**
- [ ] **Step 2: `removeSource`/`removeModel` 确认后调 delete API**
- [ ] **Step 3: 文案与 i18n 更新**
- [ ] **Step 4: 结构字符串测试**（indexHTML 含新 API path 与删除文案）
- [ ] **Step 5: Commit** `feat(admin): 弹窗新增与删除即时落盘`

---

### Task 4: 供应商侧栏 TOC + 窄屏 chips

**Files:**
- Modify: `internal/admin/assets/index.html`（CSS + markup + JS）

- [ ] **Step 1: 源卡片锚点 + 侧栏 TOC + 顶栏 chips**
- [ ] **Step 2: activeSourceId、scroll spy、click jump；调序后顺序跟随 cfg.sources**
- [ ] **Step 3: 结构测试含 toc class**
- [ ] **Step 4: Commit** `feat(admin): 供应商侧栏滚动导航`

---

### Task 5: 回归与门禁

- [ ] `go test ./internal/admin/ -count=1`
- [ ] `task check`（或 gofmt + go vet + go test ./...）
- [ ] 需要时 `go test -race ./internal/admin/`

## Spec coverage

| Spec 项 | Task |
|---|---|
| 新增源/模型即时落盘 | 1–3 |
| 删除即时落盘 | 1–3 |
| 字段编辑仍草稿 | 3（不改 saveConfig 语义） |
| TOC + 高亮 + 调序 | 4 |
| 窄屏 chips | 4 |
| 测试 | 1,2,5 |
