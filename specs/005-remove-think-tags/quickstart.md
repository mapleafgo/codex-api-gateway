# Quickstart: 剔除正文思维标签处理

## 前置

- 仓库根目录 `/home/mapleafgo/Projects/OpenProject/codex-api-gateway`
- Go 工具链与 Task 可用

## 验证场景

### 1. 单元测试（转换层）

```bash
go test ./internal/chatstreamconv -count=1
```

预期：全部通过；`think_test.go` 已删除，不再出现标签状态机测试；既有
`TestReasoningContentBeforeText` 等原生推理字段测试保持通过。

### 2. 残留检查

```bash
rg -n "feedContentThink|thinkOpenTag|thinkCloseTag|isThinking|thinkBuf|thinkLastTag" internal/chatstreamconv
```

预期：无匹配（无标签解析符号残留）。

### 3. 全量门禁

```bash
task check
task test-race
golangci-lint run ./...
```

预期：全部通过。

## 端到端行为（可选手测）

向网关发 `backend_type: c` 请求并让上游返回含 `<thinking>...</thinking>` 的 content：

```bash
curl -N http://localhost:9870/v1/responses ... # 经 source=opencode 等 c 源
```

预期：SSE 输出中的 `output_text` 原样包含标签文本；无因标签产生的 reasoning item；
原生 `reasoning_content` 仍映射为 `response.reasoning_text.*`。
