# Feature Specification: 模型目录动态同步

**Feature Branch**: `003-dynamic-model-catalog`

**Created**: 2026-08-18

**Status**: Draft

**Input**: User description: "模型写入配置目录 models.json；页面上变更配置时动态更新，使 Codex 通过 model_catalog_json 使用它。"

## User Scenarios & Testing *(mandatory)*

### User Story 1 - 管理页模型变更同步到 Codex 模型目录文件 (Priority: P1)

维护者在管理页上新增、删除或排序模型后，网关除了保存 `config.yaml` 并热重载自身配置，还会把与 `/v1/models` 一致的模型目录写入 Codex 配置目录的 `models.json`。启用「应用到 Codex」后，Codex 的 `config.toml` 通过 `model_catalog_json` 指向该文件；取消启用时恢复原配置。

**Why this priority**: 这是用户明确要求的交付项；文件型模型目录让 Codex 的模型选择不依赖网关页面会话，管理页的每轮模型变更都必须能稳定落到 Codex。

**Independent Test**: 使用临时 Codex 配置目录启动网关，调用管理页模型新增、删除、排序接口，检查 `models.json` 内容与 `/v1/models` 一致；随后执行「应用到 Codex」启停，检查 `config.toml` 键值与备份恢复行为。

**Acceptance Scenarios**:

1. **Given** 管理页完成一次模型新增、删除或排序保存，**When** 保存成功且运行时配置已热重载，**Then** `models.json` 内容与 `/v1/models` 返回的模型 slug、顺序和能力字段一致且已更新。
2. **Given** 用户启用「应用到 Codex」，**When** 写入成功，**Then** `config.toml` 顶层出现 `model_catalog_json`，值为 `models.json` 的绝对路径，并保留网关 provider 配置。
3. **Given** 用户取消「应用到 Codex」，**When** 恢复流程执行，**Then** `model_catalog_json` 恢复为启用前的值（无原值时移除），并清理启停产生的备份。
4. **Given** `models.json` 或 `config.toml` 写入失败、或配置校验失败，**When** 用户保存或启停，**Then** 旧文件与旧配置保持不变，接口返回明确错误。

### Edge Cases

- 模型列表为空、slug 含特殊字符、模型顺序被并发修改时，`models.json` 必须保持完整可解析，且与最新配置一致。
- 管理页保存与外部配置编辑同时发生时，`models.json` 只能出现一次成功同步结果，不能混合新旧模型。
- `CODEX_HOME` 缺失或非法时，不得自动创建目录，也不得让 `config.yaml` 保存本身失败。
- `config.toml` 不存在或「应用到 Codex」未启用时，不得因 `models.json` 同步逻辑而修改 Codex 配置。
- 启用前已存在自定义 `model_catalog_json` 时，启用后可被覆盖，取消后必须恢复原值。

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: 网关 MUST 在 Codex 配置目录维护 `models.json`，其 JSON 结构与 `/v1/models` 的 Codex 模型目录返回一致。
- **FR-002**: 每次模型配置成功加载（管理页保存或配置热重载）后，网关 MUST 重新生成 `models.json`，使文件与最新模型顺序、slug 和能力字段一致。
- **FR-003**: 管理页模型新增、删除、排序接口 MUST 在写盘并热重载成功后触发 `models.json` 更新。
- **FR-004**: `models.json` 写入 MUST 使用原子替换；失败时 MUST 保留旧文件并返回明确错误。
- **FR-005**: 启用「应用到 Codex」时，MUST 在 Codex `config.toml` 中设置顶层 `model_catalog_json` 指向 `models.json` 的绝对路径，同时保留既有网关 provider 配置。
- **FR-006**: 取消「应用到 Codex」时，MUST 恢复或移除 `model_catalog_json`，并清理本功能产生的备份文件。
- **FR-007**: `models.json` 同步失败 MUST NOT 回滚或破坏已经成功保存的 `config.yaml` 与运行时配置。
- **FR-008**: `CODEX_HOME` 缺失、非法或 `config.toml` 不存在时，MUST 沿用现有 `codexconfig` 约束，不自动创建目录或配置文件。

### Key Entities

- **`models.json`**: Codex 配置目录内的模型目录文件，内容与 `/v1/models` 一致，供 Codex `model_catalog_json` 读取。
- **配置重载**: `config.yaml` 成功写入并重新加载后，网关运行时进入新模型配置状态，同时驱动 `models.json` 更新。
- **「应用到 Codex」开关**: 管理/托盘对 `$CODEX_HOME/config.toml` 的启停操作，负责写入或恢复 `model_catalog_json`。
- **启用前备份**: 记录启用前的 `model_provider` 与 `model_catalog_json` 原值，取消启用时按备份恢复。

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: 管理页模型新增、删除、排序各执行一次后，`models.json` 与 `/v1/models` 的模型 slug、顺序和能力字段一致率达到 100%。
- **SC-002**: 启用「应用到 Codex」后，`config.toml` 中的 `model_catalog_json` 指向存在的 `models.json`；取消后原值恢复或无残留，验收用例 100% 通过。
- **SC-003**: 写入失败场景下旧文件与旧配置保持不变，且错误信息可被管理页展示。
- **SC-004**: `task check`、`task test-race`、`golangci-lint run ./...` 全部通过，模型目录相关并发测试不产生 race。

## Assumptions

- `models.json` 位于 Codex 配置目录：启用或未设置 `CODEX_HOME` 时按现有 `codexconfig` 语义解析，非法时不做自动创建。
- `models.json` 的 JSON 结构与 `/v1/models` 返回一致，可直接被 Codex 的 `model_catalog_json` 读取。
- 「应用到 Codex」仍沿用现有约束：`config.toml` 不存在时不自动创建、不写备份；启用前必须先运行一次 codex 生成配置。
- 管理页保存继续走 `config.yaml` 原子写盘与热重载链路，`models.json` 生成挂接在成功重载之后，不新增第二条运行时配置入口。
- Codex 对 `models.json` 的读取与热刷新时机由 Codex 客户端决定；本特性保证文件始终与网关最新配置一致，不承诺修改已有运行中会话。
