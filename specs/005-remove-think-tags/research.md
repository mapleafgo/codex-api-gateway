# Research: 剔除正文思维标签处理

## Decision 1: 删除而非保留开关

- **Decision**: 从 `internal/chatstreamconv` 完整删除正文思维标签解析（状态机、缓冲、
  helpers、测试），content 无条件走 `feedText` 原样透传。
- **Rationale**: 用户明确判定该能力为过时负担（"这种旧东西，不用管了"）；上游现代
  兼容端点通过独立 `reasoning_content` 字段表达推理，正文标签形态不再受支持；保留开关
  会保留维护与误判成本，违反最小增量原则。
- **Alternatives considered**:
  - 保留解析但加配置开关：引入新配置项与矩阵状态，与"剔除"目标相反，拒绝。
  - 降级为"只剥标签不进 reasoning"：仍在改写用户可见正文，偏离原样透传语义，拒绝。

## Decision 2: 保留独立推理字段映射

- **Decision**: `delta.reasoning_content` / `delta.reasoning` / `delta.reasoning_text`
  的既有 `feedReasoning` 映射保持不变。
- **Rationale**: 这是 Chat 协议字段级映射（spec FR-003 回归保护），不属于正文标签
  处理；删除会破坏 DeepSeek/GLM 等 thinking-mode 源的既有推理回显。
- **Alternatives considered**: 一并删除（连带简化）：会导致协议能力回退与新 400
  回归，拒绝。

## Decision 3: 文档同步策略

- **Decision**: 更新 `docs/protocol-coverage.md`（新增 2026-08-20 变更记录、确认 c 路径
  出站矩阵 `delta.content` 行保持"原样透传"）；历史规格 `002` / `004` 与
  `docs/superpowers/specs/2026-08-19-c-chat-thinking-tags-langchain-design.md` 作为
  历史记录保留不维护。
- **Rationale**: 章程 II 要求协议行为变更同步矩阵；Assumptions 已声明历史文档不作为
  交付范围。
- **Alternatives considered**: 删除历史文档：会破坏规格编号连续性且超出最小增量，拒绝。
