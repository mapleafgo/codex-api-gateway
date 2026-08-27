# 实施报告：插件式上游源架构

日期：2026-08-27。本文件登记 specs/010 收尾（analyze → implement → converge）
的验证证据、quickstart 场景执行结果与残留事项，供 T056/T069/T070 追踪。

## 门禁结果

- `task check`（fmt-check + vet + `go test ./...`）：通过。
- `task test-race`：通过。
- `task build`：通过，产出 `./codex-api-gateway`。
- `golangci-lint run ./...`：0 issues（本轮补 `SupportsWebSearchValue` 文档注释
  后清零）。

## 本轮落地的测试证据

- T035：`internal/plugins/copilot/cache_test.go` 新增
  `TestClientConcurrentMissRefreshFetchesOnce`，16 个 goroutine 同时并发 miss，
  经 singleflight 折叠为一次上游请求（hits == 1）。
- T037：`internal/plugin/architecture_test.go` 新增
  `TestSharedCoreHasNoSourceFacts`，对 admin/config/configwatch/health/scheduler/server
  的非 test Go 源码做文本级守护；仅白名单 `internal/config/config.go`（迁移错误
  必须指名旧字段）。与既有的 import 级守护互补。
- T058/T059/T061：`internal/assembly/builtins.go` 暴露 `Builtins()`，
  `internal/server/source_plugins_e2e_test.go` 新增：
  - `TestServerRegistersExternalSourcePluginEndToEnd`：注册式 test source 经
    `/v1/responses` 透传 SSE 且 metrics 历史携带插件自报身份；
  - `TestExternalSourceReloadRejectsUnknownBackend`：写未知 backend 的新配置，
    热重载失败保留旧 holder 状态。
- 观测身份：`internal/metrics` 空 backend 归一为 `unknown`（不再伪造 anthropic），
  `internal/server` 兜底占位同步；相关测试断言已更新。

## 管理页共享化（T053/T054/T055）

`internal/admin/assets/index.html` 已移除全部源专属硬编码：

- 设备授权状态机改由插件 Descriptor 的声明式动作元数据驱动（
  `routesForAction('device-flow')`），前端不再出现
  `/admin/api/copilot/auth/*` 路径字面量；
- i18n `copilotAuth*`/`githubToken`/`backendAnthropic|Chat|Responses|Copilot`
  键全部删除，改为通用 `auth*`/`unknownBackend`；
- 请求历史 `backendTypeLabel/Title` 不再做 A/C/G/R 短码映射，未知/历史值原样显示；
- `saveConfig` 删除 `s.github_token` 特判；表单默认 backend 与默认源名由描述符推导；
- admin 测试同步改为断言通用键与通用方法名
  （`TestAuthSourceCardHidesTokenInput` 等）。

## quickstart 执行记录（T069）

- 场景 1（迁移错误）：`backend_type: g` 旧形状配置启动即被拒绝：
  `config: source backend_type is removed; set backend to a registered source plugin id ...`。
- 场景 2（三类内置源）：good config 启动，`/v1/models` 返回 200，
  `/admin/api/source-plugins` 返回四个稳定 ID
  `anthropic, github-copilot, openai-chat, openai-responses`，
  Copilot 描述符带 `device-flow` 动作。
- 场景 3（Copilot 委托）：由 `internal/plugins/copilot` 的
  backend/endpoint/models 单测覆盖（r>a>c 分发、目录不可用回退）；真实凭据
  验证跳过。
- 场景 4（管理页）：HTML 契约测试 + admin API 测试覆盖动态表单、扩展动作、
  脱敏与保存刷新；真实浏览器手工验收仍须人工执行。
- 场景 5（加法器测试源）：`TestServerRegistersExternalSourcePluginEndToEnd`
  在 registry/assembly 注入实现，共享核心文件零改动。

## FR/SC 追踪（T070）

| 需求 | 验证证据 |
|---|---|
| FR-014 / SC-004 | server e2e 注册式源零共享改动 |
| SC-002 | `internal/plugin/architecture_test.go` import + 文本双守 |
| SC-003 | source_plugins 契约测试 + admin HTML 断言 |
| SC-005 | config v2 校验测试 + 热重载失败保留旧配置（e2e） |
| SC-001 | `task check` 全量回归 |

## 收敛补记：顶层 anthropic 配置输入移除落地

本次 converge 把「移除顶层 `AnthropicCfg` 配置输入」从契约落实到代码（T013/T014
要求），同时保留 `Config.Anthropic` 作为运行时载体：

- `internal/config/config.go`：`Config.Anthropic` 字段改为 `koanf:"-" yaml:"-"`
  纯运行时载体，只由 `plugins/anthropic` 在请求时把 per-source options 归一化写入、
  `convert.ToAnthropic` 读取；加载时 `validate` 补齐内置默认值。
- `rejectLegacyConfigShape` 新增顶层 `anthropic:` 迁移错误，指明迁入
  `backend: anthropic` 源的 `options`。
- 删除 `applyEnvOverrides` 中 `anthropic.*` 环境变量覆盖、`MarshalYAML` 输出与
  `defaultConfigYAML` 段。
- `internal/admin`：删除 `anthropicView` 视图/输入结构与全局面板；管理页 source
  卡片已用 Descriptor 通用渲染 `options.default_max_tokens` / `options.cache_enabled`。
- `config.example.yaml` 顶层 anthropic 段改为注释，指向源 options。
- 测试：`TestAnthropicConfigRejectsTopLevel` 断言迁移错误；删除环境覆盖与无效值
  用例；admin 面板测试反转断言为"不存在全部局 anthropic 面板"。
- `docs/protocol-coverage.md` 两处引用改为 per-source `options.*` 表述。
| SC-006 | Device Flow/脱敏单测 + 全量无凭据泄漏 grep |
| FR-010..FR-013, FR-015 | 对应任务 T036/T047/T012/T060..T066 已勾选并附测试 |

## 残留与显式说明

- `supports_web_search` 前端缺省值保留与服务端 `SupportsWebSearchValue()`
  一致的行为（仅 `openai-chat` 缺省 false）；这是既有产品契约，如需下沉为
  插件级能力声明可另开议题。
- 浏览器级手工验收（quickstart 场景 4 的点击路径）尚未自动化，本次以
  `internal/admin` 的 HTML 契约测试作为等效 harness。
