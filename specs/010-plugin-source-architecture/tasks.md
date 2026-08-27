# Tasks: 插件式上游源架构

**Input**: Design documents from `specs/010-plugin-source-architecture/`

**Prerequisites**: [plan.md](plan.md), [spec.md](spec.md), [research.md](research.md), [data-model.md](data-model.md), [contracts/](contracts/), [quickstart.md](quickstart.md)

**Tests**: 包含测试任务。实现前先写对应失败测试，确认失败原因与目标行为一致，再进入实现。

**Organization**: 按 user story 分阶段；US1 是 MVP，US2/US3 依赖基础契约，US4 验证扩展成本。

## Format

- `[P]`: 可与其他任务并行，涉及不同文件且不依赖未完成改动。
- `[US*]`: 对应 spec.md 的 user story。
- 所有 Go 测试放在被测包同目录，命名 `*_test.go`。

## Phase 1: Setup

**Purpose**: 建立插件边界和架构守护基线。

- [X] T001 [P] Create package directories and doc.go boundaries for `internal/plugin`, `internal/plugins/anthropic`, `internal/plugins/openaichat`, `internal/plugins/openairesponses`, and `internal/plugins/copilot`
- [X] T002 [P] Add a temporary architecture dependency test in `internal/plugin/architecture_test.go`; assert shared packages do not import concrete plugins after migration is complete
- [X] T003 Record the four-source regression matrix in `specs/010-plugin-source-architecture/quickstart.md`; each source must name its streaming-terminal, empty-stream failover, 4xx attribution, cancellation, model-list, health-probe, admin-save, and Copilot directory-available/directory-failed variants before migration

## Phase 2: Foundational (Blocking Prerequisites)

**Purpose**: Define contracts, immutable registry, config injection points, and platform-only scheduling behavior. This blocks all user stories.

### Contract Tests

- [X] T004 [P] Add failing descriptor/schema validation tests in `internal/plugin/descriptor_test.go` for duplicate plugin IDs, duplicate action IDs, nil backend, unknown streaming kind, invalid field type, sensitive fields, and required/default metadata
- [X] T005 [P] Add failing registry tests in `internal/plugin/registry_test.go` for construction immutability, Get, Descriptors ordering, ValidateSource delegation, unknown-backend errors, schema-foreign option rejection, and typed option validation errors
- [X] T006 [P] Add failing delegation-host tests in `internal/plugin/backend_test.go` for BackendByID lookup, missing-ID failure, and delegated event identity wrapping

### Plugin Foundation Implementation

- [X] T007 Implement `ID`, `Descriptor`, `Capability`, `StreamingKind`, `Field`, `Action`, and schema validation in `internal/plugin/descriptor.go` using contracts/plugin-contract.md
- [X] T008 Implement `Backend`, `UpstreamEvent.Backend`, optional `RequestPreparer`, EventGate mode selection support, and shared outcome helpers in `internal/plugin/backend.go`; migrate generic helpers from `internal/backend/helpers.go` without changing protocol semantics
- [X] T009 [P] Implement `Model`, `ModelCatalog`, `DraftModelCatalog`, `HealthProbe`, `ErrCapabilityNotSupported`, and probe result types in `internal/plugin/catalog.go`
- [X] T010 [P] Implement sensitive sentinels, action request/result types, and AdminExtension contract in `internal/plugin/admin.go`
- [X] T011 Implement immutable `Registry.New/Get/Descriptors/ValidateSource` and `config.SourceValidator` compatibility in `internal/plugin/registry.go`; make T004-T006 pass
- [X] T012 [P] Implement `DelegateHost`, `DelegateConsumer`, and an event wrapper that rewrites only observation identity to the delegating plugin ID in `internal/plugin/delegate.go`

### Config v2 Foundation

- [X] T013 Add failing Config v2 parser/validation tests in `internal/config/config_test.go` for `backend` + `options`, strict source decoding, rejected `backend_type`, rejected legacy top-level `github_token` and `anthropic`, recursive `${VAR}` interpolation in options, unique names, common header checks, and hot-reload failure preserving old holder state
- [X] T014 Implement Config v2 `Source.Backend`, `Source.Options map[string]any`, strict YAML decoding, removal of `BackendType`, `GithubToken`, and top-level `AnthropicCfg` in `internal/config/config.go`; preserve server/logging/breaker/models/common source fields
- [X] T015 Add `SourceValidator` injection to `config.Load` and admin/configwatch write paths in `internal/config/config.go`, `internal/admin/admin.go`, and `internal/configwatch/configwatch.go`; ensure every disk load/save uses the same injected registry validator
- [X] T016 Update YAML marshaling/write-back for ordered deterministic sources/options and no default-value noise in `internal/config/config.go`; make T013 pass

### Platform Foundation

- [X] T017 Add failing scheduler tests in `internal/scheduler/scheduler_test.go` proving registry-based dispatch, converted-vs-passthrough EventGate selection, existing failover/cancel/timeout semantics, and no concrete backend branches
- [X] T018 Refactor `Scheduler.New`, `backendFor`, model listing, and reload handling in `internal/scheduler/scheduler.go` to depend only on `plugin.Registry`, optional catalogs, descriptors, and holder; remove imports of concrete backends
- [X] T019 Add failing server preflight tests in `internal/server/server_test.go` for first-source RequestPreparer invocation, capability-based mixed warnings, stable `backend` logging/metrics identity, and unchanged SSE response shape
- [X] T020 Replace backend-type preconversion and warning branches with plugin capabilities and RequestPreparer in `internal/server/server.go`; remove direct imports of `internal/backend` and short-code comparisons
- [X] T021 [P] Rename metrics identity from `BackendType` / `backend_type` to `Backend` / `backend`, remove short-code input normalization, and update tests in `internal/metrics/metrics.go` and `internal/metrics/metrics_test.go`
- [X] T022 [P] Add generic health dispatch through optional `HealthProbe` in `internal/health/checker.go` and `internal/health/checker_test.go`; unsupported capability returns explicit not-supported result
- [X] T023 Extend runtime assembly seams so `cmd/server/main.go` can construct Registry, inject it into config loading/watching, scheduler, server, admin, and health without introducing global mutable registration

**Checkpoint**: Contracts, registry, Config v2 parsing, validator injection, scheduler/server/health abstractions compile; existing tests may temporarily fail only where story migrations are explicitly pending.

---

## Phase 3: User Story 1 - 用命名源类型配置上游 (Priority: P1) 🎯 MVP

**Goal**: Operators declare Anthropic, OpenAI Chat, OpenAI Responses, and GitHub Copilot sources with stable `backend` IDs and plugin-owned options.

**Independent Test**: Load minimal valid configs for all four built-ins; reject missing required options, unknown backend, schema-foreign options, and every legacy spelling without starting or replacing runtime state.

### Tests for User Story 1

- [X] T024 [P] [US1] Add failing table-driven config acceptance tests in `internal/config/config_test.go` covering four valid built-in configs and every FR-003/FR-004 rejection case from contracts/config-v2.md
- [X] T025 [P] [US1] Add failing per-plugin option validation tests in `internal/plugins/anthropic/options_test.go`, `internal/plugins/openaichat/options_test.go`, `internal/plugins/openairesponses/options_test.go`, and `internal/plugins/copilot/options_test.go`
- [X] T026 [P] [US1] Add failing protocol smoke tests in each built-in plugin package proving Execute delegates to the existing conversion/passthrough engine and preserves Responses SSE event order

### Implementation for User Story 1

- [X] T027 [US1] Implement Anthropic SourcePlugin with stable ID `anthropic`, options schema `default_max_tokens` / `cache_enabled`, RequestPreparer, Backend, ModelCatalog, HealthProbe, and normalized upstream token reporting in `internal/plugins/anthropic/` (schema+options 归一化、RequestPreparer、ModelCatalog、HealthProbe 均已落地)
- [X] T028 [US1] Implement OpenAI Chat SourcePlugin with stable ID `openai-chat`, common connection fields plus Chat-specific options, RequestPreparer semantics, Backend, ModelCatalog, HealthProbe, and web-search shape handling in `internal/plugins/openaichat/`
- [X] T029 [US1] Implement OpenAI Responses SourcePlugin with stable ID `openai-responses`, passthrough streaming kind, PrepareUpstreamBody-backed RequestPreparer, Backend, ModelCatalog, HealthProbe, and raw passthrough event semantics in `internal/plugins/openairesponses/`
- [X] T030 [US1] Create GitHub Copilot SourcePlugin boundary with stable ID `github-copilot`, options schema for `github_token`, endpoint/base-url rules, config validation, absorbed client state, and placeholder-safe capability declarations in `internal/plugins/copilot/`; full routing lands in US2
- [X] T031 [US1] Register exactly these four plugins and inject one immutable Registry through assembly in `cmd/server/main.go`; remove hardcoded scheduler backend construction
- [X] T032 [US1] Replace `config.example.yaml` with Config v2 examples for all four built-ins, including required options and explicit comments that old formats are rejected
- [X] T033 [US1] Run targeted `go test ./internal/config ./internal/plugin ./internal/plugins/... ./internal/scheduler ./internal/server`; fix regressions before US2

**Checkpoint**: Four named source kinds load, validate, execute through their own plugin, appear in models/probes with stable identities, and all legacy source spellings fail closed.

---

## Phase 4: User Story 2 - Copilot 行为由单一源插件承载 (Priority: P1)

**Goal**: Move all Copilot discovery, catalog filtering, protocol routing, headers, auth flow, probes, and UI-facing actions into `internal/plugins/copilot`; shared core has zero Copilot facts.

**Independent Test**: Use mock directory/discovery/auth endpoints to verify r > a > c delegation, fallback route, Zed-style headers, stable Copilot observability, single active Device Flow session, atomic credential persistence, and architecture isolation.

### Tests for User Story 2

- [X] T034 [P] [US2] Add failing Copilot routing tests in `internal/plugins/copilot/backend_test.go` for supported-endpoint r/a/c selection, missing-model fallback, catalog-fetch fallback to Responses, endpoint discovery failure diagnostics, token/header propagation, and no local entitlement rejection
- [ ] T035 [P] [US2] Add failing cache/concurrency tests in `internal/plugins/copilot/cache_test.go` for per-source state, five-minute TTL, singleflight refresh, token/endpoint change invalidation, and race-safe reads
- [X] T036 [P] [US2] Add failing Device Flow lifecycle/security tests in `internal/plugins/copilot/auth_test.go` for start/status/cancel, saving cannot cancel, concurrent-start conflict, stale-session protection, atomic write callback, redacted errors, and absence of device/access tokens from public state
- [ ] T037 [US2] Add failing architecture guard tests in `internal/plugin/architecture_test.go` asserting `scheduler`, `server`, `admin`, `health`, and `config` neither import nor textually contain concrete Copilot identifiers or legacy `github_token` handling

### Implementation for User Story 2

- [X] T038 [US2] Move and adapt GraphQL endpoint discovery, filtered model catalog, cache, defaults, and wire headers into `internal/plugins/copilot/endpoint.go`, `models.go`, `state.go`, and related tests; delete external use of `internal/copilotclient`
- [X] T039 [US2] Implement Copilot Backend routing and DelegateHost consumption in `internal/plugins/copilot/backend.go`; wrap delegated UpstreamEvent as `backend=github-copilot` while retaining route/endpoint only in safe structured logs or metadata
- [ ] T040 [US2] Implement Copilot HealthProbe and DraftModelCatalog in `internal/plugins/copilot/probe.go` and `catalog.go`, preserving ten-second management timeouts and explicit diagnostic messages
- [X] T041 [US2] Move Device Flow manager/session state and OAuth client into `internal/plugins/copilot/auth.go`; expose only AdminExtension actions and inject snapshot/write/reload callbacks from assembly
- [X] T042 [US2] Implement generic ActionRoute routing for registered AdminExtensions in `internal/admin/actions.go`, mount only descriptor-declared method/path pairs, reject conflicts and non-declared methods, enforce body limit/recover/JSON/sanitized-error conventions, and preserve action semantics inside plugins
- [X] T043 [US2] Remove Copilot branches, routes, fields, assets logic, saved-token merge logic, and imports from `internal/admin/admin.go`, `internal/admin/convert.go`, `internal/admin/copilot_auth.go`, and `internal/admin/assets/index.html`
- [X] T044 [US2] Delete obsolete `internal/backend/copilot.go`, `internal/backend/copilot_test.go`, and `internal/copilotclient/` after all references move; keep only truly shared backend helpers scheduled for their own migration task
- [X] T045 [US2] Run `task test-race` focused on plugin/cache/auth/scheduler/admin packages and fix data races before US3

**Checkpoint**: A Copilot request exercises only its plugin boundary; shared core contains no source-specific branch, and auth credentials remain disk-only.

---

## Phase 5: User Story 3 - 管理页按源能力渲染表单 (Priority: P2)

**Goal**: Render source forms, validation, sensitive handling, catalogs, probes, and extension actions from plugin descriptors without hardcoding any built-in source in shared admin code.

**Independent Test**: Fetch source-plugins metadata, switch among four types in the browser, verify required fields/actions change, submit masked sensitive values, exercise Device Flow, refresh, and confirm no plaintext credential appears in API responses.

### Tests for User Story 3

- [ ] T046 [P] [US3] Add failing `/admin/api/source-plugins` contract tests in `internal/admin/source_plugins_test.go` for descriptor JSON, schema/action visibility, sorted stable IDs, and absence of credentials
- [ ] T047 [P] [US3] Add failing config read/write tests in `internal/admin/admin_test.go` for `backend` + `options`, `__codex_redacted__`, empty-means-keep, `__codex_clear__`, literal-redacted rejection, same-name/type sensitive retention, and failed-save preservation
- [ ] T048 [P] [US3] Add failing generic action/model/test endpoint tests in `internal/admin/actions_test.go` and `internal/admin/upstream_models_test.go` for draft sensitive merge, unsupported capability responses, action conflict status, and no credential leakage
- [ ] T049 [US3] Add a browser/equivalent end-to-end test for source form switching, dynamic required fields, Device Flow panel states, save-refresh masking, and disabled/reorder controls; place implementation-specific harness under `internal/admin/` if automated

### Implementation for User Story 3

- [ ] T050 [US3] Implement descriptor collection endpoint and JSON view in `internal/admin/source_plugins.go`; make T046 pass
- [ ] T051 [US3] Rework source views and full-config write mapping around generic fields + options + schema-sensitive merge in `internal/admin/admin.go` and `internal/admin/convert.go`; make T047 pass
- [ ] T052 [US3] Wire saved/draft model catalogs and source tests to plugin interfaces in `internal/admin/admin.go`; return explicit capability-missing responses instead of backend-specific switches
- [ ] T053 [US3] Replace hardcoded form fields and Copilot visibility rules with schema-driven rendering in `internal/admin/assets/index.html`; retain localized labels supplied by descriptors or resource entries, not source-name conditionals
- [ ] T054 [US3] Update source add/edit/delete/reorder/disable/test flows in `internal/admin/assets/index.html` and `internal/admin/admin.go` to send `backend` + `options` and render plugin-declared actions generically
- [ ] T055 [US3] Remove remaining shared-admin special cases for `a/c/g/r`, `github_token`, and Copilot labels; verify with architecture guard tests
- [ ] T056 [US3] Run admin-focused tests and the browser/equivalent scenario from quickstart; capture pass evidence in the final implementation report

**Checkpoint**: The shared admin framework renders all built-ins and a hypothetical descriptor without source-name branches, while preserving atomic writes and credential safety.

---

## Phase 6: User Story 4 - 后续源以同一方式接入 (Priority: P2)

**Goal**: Prove a new source can join through its own package plus assembly registration only.

**Independent Test**: Compile/register a test plugin in tests; configure it, execute a mock request, inspect admin metadata, then unregister it and confirm reload fails while old runtime remains active.

### Tests for User Story 4

- [X] T057 [P] [US4] Add failing test-source fixture implementing Descriptor, options validation, Backend, optional catalog/probe/action behaviors in `internal/plugin/testsource_test.go`
- [~] T058 [US4] 契约级已完成（testsource_test.go 覆盖注册/元数据/校验/分发/未注册拒绝）；server/admin 端到端集成（含热重载旧状态保留）待补，依赖 US3 管理面描述符端点稳定后接入

### Implementation for User Story 4

- [ ] T059 [US4] Close any extension gaps in `internal/plugin`, `internal/scheduler`, `internal/server`, or `internal/admin` discovered by T057-T058 without adding concrete test-source references outside tests
- [ ] T060 [US4] Add a concise new-source checklist to `README.md` and link it from `docs/architecture.md` or the closest existing architecture section; include only plugin package + `cmd/server` registration steps
- [ ] T061 [US4] Assert the simulated addition requires no edits to shared scheduler/service/admin/health source files; record residual gaps if any and resolve them or update spec with explicit approval

**Checkpoint**: Extension path is executable and measurable; future source work stays inside its package and the single assembly entry point.

---

## Phase 7: Polish & Cross-Cutting Concerns

**Purpose**: Remove legacy paths, align all documentation, and run full gates.

- [ ] T062 Delete obsolete concrete adapters, constants, normalize functions, tests, and generic leftovers from `internal/backend/` once all callers use `internal/plugin`; retain only genuinely shared code moved to its proper layer
- [ ] T063 [P] Replace every operational `backend_type` mention in README, docs, admin copy, metrics payloads, logs, and tests with stable `backend` identity; historical changelog context may remain clearly historical
- [ ] T064 [P] Update `docs/protocol-coverage.md` source identity model, Copilot delegation section, route naming, observability keys, and cross-references to specs/010-plugin-source-architecture/contracts/observability.md
- [ ] T065 [P] Audit the shared sanitizer across logs, upstream/plugin/Device Flow errors, metrics snapshots, admin API responses, SSE error events, and test fixtures for Authorization/API key/GitHub token/device code/access token leakage
- [ ] T066 [P] Verify Config v2 example round-trips through admin save/load without losing comments-critical fields, source order, model order, sensitive sentinels, or breaker overrides
- [ ] T067 Run `gofmt -w cmd/ internal/`, `go vet ./...`, and resolve staticcheck/revive/unused findings introduced by migration
- [ ] T068 Run `task check` and `task test-race`
- [ ] T069 Build with `task build` and execute every runnable scenario in `specs/010-plugin-source-architecture/quickstart.md`; record command results and failures
- [ ] T070 Perform a final spec traceability review mapping FR-001..FR-015 and SC-001..SC-006 to passing tasks/tests; open explicit follow-ups for any approved gap

---

## Dependencies & Execution Order

### Phase Dependencies

```text
Setup T001-T003
  -> Foundational T004-T023
    -> US1 T024-T033 (MVP)
      -> US2 T034-T045
        -> US3 T046-T056
Foundational -> US4 T057-T061 (can start after foundation, but finalize after US1-US3)
All stories -> Polish T062-T070
```

US1 must precede US2 because Copilot's plugin boundary and config validation land there. US3 depends on both P1 stories having descriptors and actions. US4 may begin after Foundational but should not close until all shared seams are stable.

### Task Dependencies

- T007-T012 depend on their matching tests T004-T006.
- T014-T016 depend on T013.
- T018/T020/T022/T023 depend on foundational plugin/config contracts.
- T027-T030 depend on T007-T023; T031 depends on all four plugins.
- T038-T044 depend on T030 and foundational delegation contracts.
- T050-T055 depend on descriptors from US1 and action infrastructure from T042.
- T057-T061 depend on foundational Registry and stable server/admin seams.
- T062-T070 are final cleanup and gates.

## Parallel Opportunities

- T001/T002 and contract tests T004-T006 are independent file-level starts.
- T009/T010/T012 can proceed after their contracts are agreed.
- Built-in plugin implementations T027-T029 are parallel once foundation compiles.
- Copilot routing/cache/auth tests T034-T036 are parallel before implementation.
- US3 API/view tests T046-T048 are parallel, but UI implementation waits on API contracts.
- Documentation/audit tasks T063-T066 can run in parallel during polish.

## Implementation Strategy

### MVP First

1. Complete Setup and Foundational phases.
2. Deliver US1 and validate four named sources independently.
3. Stop at the US1 checkpoint only if the team wants a narrow config-first increment; do not ship partial Copilot isolation as final.

### Incremental Delivery

1. Land US1 for stable identities and generic execution.
2. Land US2 to prove the hardest isolation case.
3. Land US3 to eliminate hardcoded admin forms.
4. Close US4 with a measured test-source extension.
5. Finish legacy deletion, docs, security audit, race tests, build, and quickstart.

## Completion Notes

- Every implementation task above has a corresponding focused or shared test task.
- Do not mark US2 complete while `internal/backend` or shared admin still contains Copilot facts.
- Do not mark the feature complete until `task check`, `task test-race`, `task build`, architecture guards, browser-equivalent admin verification, and quickstart scenarios pass.
