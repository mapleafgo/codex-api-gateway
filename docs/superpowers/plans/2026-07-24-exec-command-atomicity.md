# exec_command 原子调用约束实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 在中英文基线指令中统一约束所有 `exec_command`，要求互不依赖的工作单元拆成独立并行工具调用，同时允许原子任务内部使用必要的 shell 组合语法。

**Architecture:** 仅调整 `base_instructions.md` 与 `base_instructions_cn.md` 的通用工具调用规则，不在 Skill 章节增加特例，也不修改网关协议转换。规则以“原子工作单元”为边界，区分独立任务合并与单项任务内部的数据处理管道。

**Tech Stack:** Markdown、Codex ModelInfo `base_instructions`

## Global Constraints

- 一个互不依赖的工作单元对应一个独立的 `exec_command` 工具调用。
- 同一轮存在多个互不依赖的工作单元时，必须拆分后并行发出。
- 禁止使用 `;`、`&&`、`||`、循环、子 shell 或 `xargs` 合并多个独立工作单元。
- 单个原子工作单元内部允许使用必要的管道、重定向和条件处理。
- 中英文规则必须保持相同语义。
- 新规则必须保持与 General 相邻条目一致的简洁长度和指令句式。
- 英文必须使用 `must` / `must never`，中文必须使用“必须”/“禁止”。
- 不修改网关协议转换及工具调用结果关联。

---

### Task 1: 收紧中英文 exec_command 规则

**Files:**
- Modify: `base_instructions.md:6`
- Modify: `base_instructions_cn.md:7`

**Interfaces:**
- Consumes: `/v1/models` 返回的 `CodexModelInfo.base_instructions`
- Produces: 面向模型的中英文 `exec_command` 原子工作单元规则

- [ ] **Step 1: 记录修改前的规则缺口**

确认当前规则只要求独立调用并行、禁止带横幅的长脚本，但没有明确禁止使用 `;`、`&&`、`||`、循环、子 shell 或 `xargs` 合并多个独立工作单元：

```bash
sed -n '6,9p' base_instructions.md
sed -n '7,10p' base_instructions_cn.md
```

Expected: 两份文件均未出现“原子工作单元”边界及完整的禁止合并语法列表。

- [ ] **Step 2: 修改英文规则**

将现有两条通用工具调用规则收紧为两条简短指令：

```markdown
- Each independent unit of work must use its own tool call. Independent `exec_command` operations must be issued as separate parallel calls and must never be combined into one `cmd`; shell composition is allowed only within one atomic operation.
- You must never print decorative or otherwise useless separators such as `echo "===="` or `printf '---'`; that output noise worsens the user-visible conversation.
```

- [ ] **Step 3: 修改中文规则**

将对应规则同步为：

```markdown
- 每个互不依赖的工作单元必须使用独立的工具调用。互不依赖的 `exec_command` 操作必须拆成独立的并行调用，禁止合并到同一个 `cmd`；只有单个原子操作内部才允许使用 shell 组合。
- 禁止输出 `echo "===="`、`printf '---'` 等装饰性或无用分隔符；这类输出噪音会损害用户侧对话体验。
```

- [ ] **Step 4: 验证中英文规则覆盖范围**

Run:

```bash
rg -n 'must use its own tool call|must never be combined|必须使用独立的工具调用|禁止合并' base_instructions.md base_instructions_cn.md
git diff --check
```

Expected: 英文和中文各匹配独立调用、禁止合并及原子操作边界，`git diff --check` 无输出。

- [ ] **Step 5: 运行仓库门禁**

Run:

```bash
task fmt
golangci-lint run ./...
task test
```

Expected: 格式检查、lint 和全部 Go 测试通过。

- [ ] **Step 6: 提交实现**

```bash
git add base_instructions.md base_instructions_cn.md
git commit -m "docs(instructions): 收紧 exec_command 原子调用约束"
```
