# Tool Invocation Contract Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 为所有工具调用增加统一的名称、schema、前置条件与前置结果约束。

**Architecture:** 规则落在中英文基线指令的通用约束中，不进入 Skill、MCP 或任何具体工具章节。Chat 转换和工具执行链保持不变。

**Tech Stack:** Markdown、Task

## Global Constraints

- `base_instructions.md` 与 `base_instructions_cn.md` 必须成对修改。
- 英文使用 `must` / `must never`，中文使用“必须”/“禁止”。
- 禁止增加具体工具、provider 或协议特例。
- 禁止修改 Chat 转换、工具 schema、路由或执行逻辑。
- 不创建提交。

---

### Task 1: 增加通用工具调用契约

**Files:**
- Modify: `base_instructions.md:5`
- Modify: `base_instructions_cn.md:6`
- Modify: `docs/superpowers/specs/2026-07-27-tool-invocation-contract-design.md`

**Interfaces:**
- Consumes: 当前会话提供的工具名称、schema、前置条件与工具返回值。
- Produces: 对全部工具统一生效的模型执行规则。

- [x] **Step 1: 修改英文基线**

在 `# General` 的通用工具约束中加入：

```markdown
- Every tool call must follow the exact tool name, argument schema, and prerequisites declared in the current conversation. You must never invent undeclared tools, argument fields, or prerequisite results. When a tool requires values returned by another tool, you must use the exact values returned in the current conversation and must never call it without them.
```

- [x] **Step 2: 修改中文基线**

在 `# 总则` 的对应位置加入：

```markdown
- 每次工具调用都必须遵守当前对话中声明的准确工具名、参数 schema 与前置条件。禁止编造未声明的工具、参数字段或前置结果。当工具要求使用其他工具返回的值时，必须使用当前对话中实际返回的精确值；没有这些返回值时禁止调用。
```

- [x] **Step 3: 同步设计文档**

确认设计文档记录相同规则，并明确不增加具体工具特例。

- [x] **Step 4: 检查文本与差异**

Run: `git diff --check`

Expected: 无输出，退出码为 0。

- [x] **Step 5: 运行项目门禁**

Run: `task check`

Expected: 格式检查、`go vet` 与全部测试通过。
