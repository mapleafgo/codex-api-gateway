# Feature Specification: 模型目录与思维标签联动优化

**Feature Branch**: `002-model-catalog-think-tags`

**Created**: 2026-08-18

**Status**: Draft

**Input**: User description: "处理正文中 ` thinking` 与 ` response` 字面 token 的问题（首字符是空格，不是尖括号标签），参考 trpc-agent-go 项目。另一个需求：模型写入配置目录 models.json，页面上变更配置时动态更新，应用到 Codex 时使 Codex 的 model_catalog_json 指向该文件。"

## User Scenarios & Testing *(mandatory)*

### User Story 1 - 正文思维标签不污染用户可见回答 (Priority: P1)

用户通过 Chat 兼容上游提问时，部分上游会把思维链字面 token ` thinking`（开启）与 ` response`（收尾）和推理文本混在正文流里返回。用户看到的最终回答必须只包含真正的内容，这两个 token 不得出现在回复正文中，推理内容应归入非可见的 reasoning 通道。

**Why this priority**: 这是用户可见的协议解析缺陷；标签泄漏会直接破坏终端展示和下一轮历史回灌，是当前最需要先修复的问题。

**Independent Test**: 构造一组带有思维标签的流式回复（含完整标签、跨 chunk 拆分标签、孤立闭标签、GLM 式 reasoning 与正文为空四种形态），逐条验证最终用户可见文本中不出现标签，且需要展示的 reasoning 内容被保留。

**Acceptance Scenarios**:

1. **Given** 上游正文流包含思维文本、` thinking` / ` response` token 和最终答复，**When** 网关完成流式转换，**Then** 最终用户可见回复包含最终答复且不包含这两个 token。
2. **Given** ` response` token 被拆成多个流式片段，**When** 网关拼接处理，**Then** 仍能正确识别 token 边界，token 不进入用户可见正文。
3. **Given** 正文只出现孤立 ` response` token、没有前置 ` thinking`，**When** 网关处理该段内容，**Then** token 被剥离且不会伪造一个新的 reasoning 展示。
4. **Given** 上游返回内容为空、但 reasoning 非空，且本轮没有工具调用（参考 trpc-agent-go 的 GLM 变体处理），**When** 网关生成最终回复，**Then** 用户仍能看到 reasoning 内容作为最终答复，而不是得到空回复。

---

### User Story 2 - 模型目录动态同步到 Codex (Priority: P2)

维护者在管理页面上新增、删除或排序模型后，网关除了更新自身运行时配置，还必须在 Codex 配置目录的 `models.json` 中落地同一份模型目录。启用「应用到 Codex」后，Codex 的 `config.toml` 应通过 `model_catalog_json` 指向该文件；取消启用时应恢复原配置。

**Why this priority**: 该能力让 Codex 使用文件型模型目录，减少对页面会话或运行时状态的依赖，是管理体验的增强项。

**Independent Test**: 使用临时 Codex 配置目录启动网关，依次执行模型新增、删除、排序和「应用到 Codex」启停，检查 `models.json` 内容、`config.toml` 键值与失败回滚行为。

**Acceptance Scenarios**:

1. **Given** 管理页完成一次模型新增、删除或排序保存，**When** 保存成功且运行时配置已热重载，**Then** `models.json` 内容与网关模型目录接口返回的模型列表一致且已更新。
2. **Given** 用户通过外部编辑或配置热重载修改模型配置，**When** 新配置成功加载，**Then** `models.json` 重新生成，与最新配置一致。
3. **Given** 用户启用「应用到 Codex」，**When** `config.toml` 写入成功，**Then** 顶层出现 `model_catalog_json`，其值为 `models.json` 的绝对路径，并保留网关 provider 配置。
4. **Given** 用户取消「应用到 Codex」，**When** 恢复流程执行，**Then** `model_catalog_json` 恢复为启用前的值（无原值时移除），并清理本次启停产生的备份。
5. **Given** `models.json` 或 `config.toml` 写入失败或配置校验失败，**When** 用户保存或启停，**Then** 旧文件与旧配置保持不变，接口返回明确错误。

### Edge Cases

- 模型列表为空、模型 slug 含特殊字符、模型顺序被并发修改时，`models.json` 必须保持完整可解析。
- 管理页保存与外部配置编辑同时发生时，`models.json` 只能出现一次成功同步，不能出现半旧半新的混合内容。
- `CODEX_HOME` 缺失或指向非目录时，不得自动创建目录，也不得让模型页面保存失败影响既有 `config.yaml` 热重载。
- `config.toml` 不存在或「应用到 Codex」未启用时，不应因 `models.json` 维护逻辑而创建或修改 Codex 配置。
- 思维标签出现在用户真实正文中（非思维位置）时，不能误删合法正文；标签识别只作用于已确认的思维链形态。

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: 网关 MUST 把已识别的思维链正文（含 ` thinking` / ` response` 字面 token 形态）从用户可见回答中剥离，并按 reasoning 通道回传，token 本身 MUST 不出现在用户可见正文。
- **FR-002**: 网关 MUST 正确处理跨流式分片的思维标签，包括完整标签、拆分标签和流末截断标签。
- **FR-003**: 对孤立 ` response` token 或单边 token，网关 MUST 剥离 token 且不得伪造新的 reasoning 输出；已由 `reasoning_content` 下发的思维内容 MUST 保持原有归属。
- **FR-004**: 当上游内容为空、reasoning 非空且无工具调用时，网关 MUST 按 trpc-agent-go 的 GLM 行为把 reasoning 保留为最终可见答复。
- **FR-005**: 网关 MUST 在每次模型配置成功加载（管理页保存或配置热重载）后，把与 `/v1/models` 一致的内容写入 Codex 配置目录的 `models.json`。
- **FR-006**: `models.json` 写入 MUST 使用原子替换；失败时 MUST 保留旧文件并返回错误。
- **FR-007**: 启用「应用到 Codex」时，MUST 在 Codex `config.toml` 中设置顶层 `model_catalog_json` 指向 `models.json`，同时保留既有网关 provider 配置。
- **FR-008**: 取消「应用到 Codex」时，MUST 恢复或移除 `model_catalog_json`，并清理本功能产生的备份文件。
- **FR-009**: `models.json` 同步失败 MUST NOT 回滚或破坏已经成功保存的网关 `config.yaml` 与运行时配置。

### Key Entities

- **思维链正文**: 上游 content 流中携带思维标记及内容的文本段，需转成 reasoning 而不是最终正文。
- **reasoning 通道**: 用户不可见但会存续的推理内容，供历史回灌和上游 thinking-mode 契约使用。
- **`models.json`**: 与网关 `/v1/models` 返回结构一致的模型目录文件，位于 Codex 配置目录内。
- **Codex `config.toml`**: Codex CLI 的用户配置，通过 `model_catalog_json` 指向 `models.json`。
- **「应用到 Codex」开关**: 管理页/托盘对 Codex 配置的启停操作，负责写入或恢复 `model_catalog_json`。

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: 完整标签、拆分标签、孤立标签、GLM 兜底四类思维链路场景的标签泄漏数量为 0，且最终可见正文与预期完全一致。
- **SC-002**: 管理页每一次模型增删改排序后，`models.json` 与 `/v1/models` 的模型 slug、顺序和能力字段一致率达到 100%。
- **SC-003**: 启用「应用到 Codex」后，`config.toml` 中的 `model_catalog_json` 指向存在的 `models.json`；取消后原值恢复或无残留，验收用例 100% 通过。
- **SC-004**: 模型保存失败或 `models.json` 写入失败时，旧文件与旧配置保持原样，错误信息可被管理页展示。
- **SC-005**: `task check`、`task test-race`、`golangci-lint run ./...` 全部通过，模型目录相关并发测试不产生 race。

## Assumptions

- `models.json` 输出到 Codex 配置目录：启用或未设置 `CODEX_HOME` 时按现有 `codexconfig` 语义解析，缺失或非法时不做自动创建。
- `models.json` 的 JSON 结构与 `/v1/models` 的 Codex 模型目录返回一致，可直接被 Codex `model_catalog_json` 读取。
- 「应用到 Codex」仍沿用现有约束：`config.toml` 不存在时不自动创建、不写备份；启用前必须先运行一次 codex 生成配置。
- 管理页保存继续走 `config.yaml` 原子写盘与热重载链路，`models.json` 生成挂接在成功重载之后，不新增第二条运行时配置入口。
- Codex 对 `models.json` 的读取与热刷新时机由 Codex 客户端决定；本特性保证文件始终与网关最新配置一致，不承诺修改已有运行中会话。
- 思维链处理对齐 `chatstreamconv` 当前已使用的 ` thinking`（开启）与 ` response`（收尾）字面 token，首字符是空格、不是尖括号标签；参考 trpc-agent-go 仅用于 GLM reasoning 空正文兜底策略，不引入其模型能力判定。
