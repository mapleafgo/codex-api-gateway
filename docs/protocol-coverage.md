# Protocol Coverage Matrix

日期: 2026-07-16

本文是 OpenAI Responses API 到 Anthropic Messages API 的覆盖矩阵。它记录协议项是否被语义级翻译、损耗翻译、raw 保真、后端无等价能力或延期专项设计。后续任何协议补齐都必须同步更新本文。

## 状态定义

| 状态 | 含义 | 实现要求 |
|---|---|---|
| `supported` | 有明确 Anthropic 等价能力，且当前网关做语义级转换 | 需要单元测试或集成测试覆盖 |
| `lossy_supported` | 可转换但存在字段或行为损失 | 必须说明损失内容 |
| `raw_preserved` | 暂无语义转换，但原始 JSON 被保存或注入上下文，避免静默丢弃 | 不得对客户端宣称语义支持 |
| `unsupported_by_backend` | Anthropic Messages 无等价能力，不能安全模拟 | 应返回明确错误或在文档中登记不支持 |
| `deferred` | 需要专项设计才能决定语义 | 必须说明后续分析点 |

## 资料来源

- OpenAI API Reference: `https://developers.openai.com/api/reference/resources/responses/methods/create`
- OpenAI SDK: `github.com/openai/openai-go/v3@v3.42.0`
- Anthropic Messages docs: `https://platform.claude.com/docs/en/api/messages`
- Anthropic streaming docs: `https://platform.claude.com/docs/en/build-with-claude/streaming`
- Anthropic SDK: `github.com/anthropics/anthropic-sdk-go@v1.57.0`

## Request Parameters

| OpenAI 参数 | Anthropic 映射 | 当前状态 | 说明 |
|---|---|---|---|
| `model` | `MessageNewParams.Model` | `supported` | 保留 Codex-facing model alias，后端 source 可替换实际模型 |
| `input` string | user text message | `supported` | 转为单条 user text block |
| `input` item list | `messages` / `system` / tool blocks | `lossy_supported` | 仅部分 item 语义支持，详见 Input Item Union |
| `instructions` | top-level `system` | `supported` | 作为 developer 指令段折入 system text |
| `max_output_tokens` | `max_tokens` | `supported` | 未设置时默认 4096 |
| `temperature` | `temperature` | `supported` | 直接映射 |
| `top_p` | `top_p` | `supported` | 直接映射 |
| `parallel_tool_calls` | `disable_parallel_tool_use` 反向映射 | `supported` | `false` 时禁用 Anthropic 并行 tool use |
| `reasoning.effort` | `thinking` budget | `lossy_supported` | OpenAI effort 非 token budget，当前用配置预算映射 |
| `reasoning.summary` | thinking display / summary events | `lossy_supported` | `concise` 映射到 summarized 输出 |
| `text.format.json_schema` | forced Anthropic tool | `lossy_supported` | 通过工具调用模拟 structured output |
| `text.format.json_object` | forced `json_object` tool | `lossy_supported` | 通过工具调用模拟 |
| `tools` | `tools` | `lossy_supported` | 仅部分工具类型支持，详见 Tool Union |
| `tool_choice` | `tool_choice` | `lossy_supported` | 仅部分 choice 支持，详见 Tool Choice Union |
| `previous_response_id` | session replay | `supported` | 依赖本地 store |
| `store` | local session storage switch | `supported` | `false` 跳过存储与回填 |
| `truncation` | response echo only | `raw_preserved` | Anthropic 无直接等价策略 |
| `include` | partial behavior | `deferred` | `reasoning.encrypted_content` 与 logprobs/source include 需逐项分析 |
| `metadata` | response echo / Anthropic metadata | `deferred` | 需确认是否转 Anthropic `metadata` 或仅 echo |
| `prompt_cache_key` | prompt cache behavior | `deferred` | Anthropic prompt caching 语义不同 |
| `prompt_cache_options` | cache control | `deferred` | 需和 Anthropic cache_control 对齐 |
| `background` | none | `unsupported_by_backend` | 当前网关只支持同步 SSE |
| `conversation` | none | `unsupported_by_backend` | 本地 store 不是 OpenAI Conversation API |
| `max_tool_calls` | none | `deferred` | Anthropic 无直接请求参数，可能需网关计数截断 |
| `service_tier` | Anthropic `service_tier` | `deferred` | 需配置源是否允许透传 |
| `safety_identifier` | none | `unsupported_by_backend` | 后端无等价字段 |
| `stream_options.include_obfuscation` | none | `unsupported_by_backend` | Anthropic streaming 无等价 obfuscation |
| `top_logprobs` | none | `unsupported_by_backend` | Anthropic Messages 无 OpenAI output logprobs 等价 |
| `user` | deprecated | `deferred` | OpenAI 已废弃，需决定忽略或映射 metadata |

## Input Content Union

| OpenAI content | Anthropic 映射 | 当前状态 | 说明 |
|---|---|---|---|
| `input_text` | `text` block | `supported` | 文本语义保留 |
| `input_image.image_url` | `image` block | `supported` | URL 或 data URI 映射 |
| `input_image.file_id` | none | `unsupported_by_backend` | 网关没有 OpenAI Files 凭据来拉取文件 |
| `input_file.file_data` | `document` block | `supported` | 以 base64/plain text 方式构造 document |
| `input_file.file_url` | `document` block | `supported` | URL document |
| `input_file.file_id` | none | `unsupported_by_backend` | 同 OpenAI Files 限制 |
| `prompt_cache_breakpoint` | Anthropic cache_control | `deferred` | 需专项对齐缓存边界和 TTL |

## Input Item Union

| OpenAI item | Anthropic 映射 | 当前状态 | 说明 |
|---|---|---|---|
| `message` / `EasyInputMessage` | message/system text | `lossy_supported` | system/developer 仅保留文本；图片等非文本内容无法放入 Anthropic system |
| `input_message` | message | `raw_preserved` | 当前未做专门分支 |
| `output_message` | assistant message | `raw_preserved` | 当前未做专门分支 |
| `file_search_call` | none | `unsupported_by_backend` | Anthropic server tool 不等价 OpenAI file search |
| `computer_call` | none | `unsupported_by_backend` | Computer use 需要专项执行环境 |
| `computer_call_output` | none | `unsupported_by_backend` | 同上 |
| `web_search_call` | Anthropic web search result/server tool | `deferred` | 两侧 hosted tool 语义不同 |
| `function_call` | assistant `tool_use` | `supported` | `arguments` 转 tool input |
| `function_call_output` | user `tool_result` | `supported` | output 转 text tool result |
| `tool_search_call` | assistant `tool_use` name=`tool_search` | `supported` | 已有语义分支 |
| `tool_search_output` | dynamic tools + tool_result | `supported` | 工具注入并记录 developer marker |
| `additional_tools` | dynamic tools | `supported` | 工具注入并记录 developer marker |
| `reasoning` | `thinking` / `redacted_thinking` | `supported` | summary 与 encrypted/signature 处理已有 |
| `compaction` | system marker | `raw_preserved` | Anthropic 无 OpenAI compaction item |
| `image_generation_call` | none | `unsupported_by_backend` | Anthropic Messages 不生成 OpenAI image output item |
| `code_interpreter_call` | Anthropic code execution tool | `deferred` | 两侧 tool/result/event 结构需专项映射 |
| `local_shell_call` | assistant `tool_use` name=`shell` | `lossy_supported` | 命令数组拼为文本；环境、超时、用户和工作目录未映射 |
| `local_shell_call_output` | user `tool_result` | `lossy_supported` | 输出作为文本保留，session 以 raw JSON 回放 |
| `shell_call` | assistant `tool_use` name=`shell` | `lossy_supported` | 命令数组拼为文本；执行环境、调用者与限制未映射 |
| `shell_call_output` | user `tool_result` | `lossy_supported` | stdout/stderr 拼为文本；结果状态和调用者未映射 |
| `apply_patch_call` | assistant `tool_use` name=`apply_patch` | `lossy_supported` | create/update diff 作为文本；delete 操作及调用者元数据未映射 |
| `apply_patch_call_output` | user `tool_result` | `lossy_supported` | 可选日志作为文本；状态和调用者未映射 |
| `mcp_list_tools` | none | `unsupported_by_backend` | Anthropic remote MCP/connector 语义不同 |
| `mcp_approval_request` | none | `unsupported_by_backend` | 无等价审批协议 |
| `mcp_approval_response` | none | `unsupported_by_backend` | 无等价审批协议 |
| `mcp_call` | none | `unsupported_by_backend` | 无安全等价映射 |
| `custom_tool_call` | assistant custom `tool_use` | `supported` | freeform custom tool 支持 |
| `custom_tool_call_output` | user `tool_result` | `supported` | output text 支持 |
| `compaction_trigger` | system marker | `raw_preserved` | Anthropic 无等价事件 |
| `item_reference` | local session lookup | `deferred` | 需决定是否解析本地 store item |
| `program` | none | `unsupported_by_backend` | OpenAI program item 无 Anthropic 等价 |
| `program_output` | none | `unsupported_by_backend` | 同上 |

## Tool Union

| OpenAI tool | Anthropic 映射 | 当前状态 | 说明 |
|---|---|---|---|
| `function` | client tool | `supported` | JSON schema 转 `input_schema` |
| `file_search` | none | `unsupported_by_backend` | 无 OpenAI vector store 后端；请求时返回明确转换错误 |
| `computer` | none | `unsupported_by_backend` | 需 computer use 执行环境；请求时返回明确转换错误 |
| `computer_use_preview` | none | `unsupported_by_backend` | 同上；请求时返回明确转换错误 |
| `web_search` | Anthropic web search server tool | `deferred` | hosted tool 语义不同；专项映射前请求时返回明确转换错误 |
| `web_search_preview` | Anthropic web search server tool | `deferred` | hosted tool 语义不同；专项映射前请求时返回明确转换错误 |
| `mcp` | none | `unsupported_by_backend` | MCP server/tool lifecycle 不等价；请求时返回明确转换错误 |
| `code_interpreter` | Anthropic code execution tool | `deferred` | 需专项映射 outputs/events；专项映射前请求时返回明确转换错误 |
| `programmatic_tool_calling` | none | `unsupported_by_backend` | 无等价能力；请求时返回明确转换错误 |
| `image_generation` | none | `unsupported_by_backend` | Anthropic Messages 不生成 OpenAI image result；请求时返回明确转换错误 |
| `local_shell` | client custom tool `shell` | `lossy_supported` | 环境/skills 字段未完整映射 |
| `shell` | client custom tool `shell` | `lossy_supported` | environment 未完整映射 |
| `custom` | Anthropic custom tool | `lossy_supported` | `format` grammar/text 语义未完整保留 |
| `namespace` | flattened tool names | `lossy_supported` | namespace 被拼入 tool name |
| `tool_search` | client tool `tool_search` | `supported` | 当前按普通 tool 暴露 |
| `apply_patch` | client custom tool `apply_patch` | `supported` | freeform input 支持 |

## Tool Choice Union

| OpenAI choice | Anthropic 映射 | 当前状态 | 说明 |
|---|---|---|---|
| `none` | `tool_choice.none` | `supported` | 直接映射 |
| `auto` | `tool_choice.auto` | `supported` | 直接映射 |
| `required` | `tool_choice.any` | `supported` | Anthropic 使用 `any` |
| `function` | `tool_choice.tool(name)` | `supported` | 直接映射 |
| `custom` | `tool_choice.tool(name)` | `supported` | 直接映射 |
| `apply_patch` | `tool_choice.tool("apply_patch")` | `supported` | 直接映射 |
| `shell` | `tool_choice.tool("shell")` | `supported` | 直接映射 |
| `allowed_tools` | filtered tool set + choice mode | `lossy_supported` | 按名称过滤支持的 Anthropic 工具；hosted/MCP allowed 条目仍不支持 |
| hosted tool choice | none | `unsupported_by_backend` | file/web/computer/code/image 等内置工具不能安全模拟；请求时返回明确转换错误 |
| `mcp` | none | `unsupported_by_backend` | 无等价 MCP choice；请求时返回明确转换错误 |
| `programmatic_tool_calling` | none | `unsupported_by_backend` | 无等价 programmatic tool choice；请求时返回明确转换错误 |

## Output Item Union

| OpenAI output item | Anthropic 来源 | 当前状态 | 说明 |
|---|---|---|---|
| `message` | text block | `supported` | 输出 text message |
| `reasoning` | thinking/redacted thinking block | `supported` | 支持 summary/signature/encrypted |
| `function_call` | `tool_use` | `supported` | 普通 tool use |
| `function_call_output` | request replay only | `supported` | 作为 input item 回放 |
| `custom_tool_call` | custom `tool_use` | `supported` | freeform input 解包 |
| `custom_tool_call_output` | request replay only | `supported` | 作为 input item 回放 |
| `tool_search_call` | `tool_use` name=`tool_search` | `deferred` | 当前会输出 function/custom，需专门输出 item |
| `tool_search_output` | request dynamic tools | `deferred` | 当前不作为 output item 发出 |
| `additional_tools` | request dynamic tools | `deferred` | 当前不作为 output item 发出 |
| `compaction` | response compact API | `raw_preserved` | 非模型 stream output |
| `file_search_call` | none | `unsupported_by_backend` | 无等价 |
| `web_search_call` | Anthropic server web search | `deferred` | 需专项映射 |
| `computer_call` | none | `unsupported_by_backend` | 无等价 |
| `computer_call_output` | none | `unsupported_by_backend` | 无等价 |
| `program` | none | `unsupported_by_backend` | 无等价 |
| `program_output` | none | `unsupported_by_backend` | 无等价 |
| `image_generation_call` | none | `unsupported_by_backend` | 无等价 |
| `code_interpreter_call` | Anthropic code execution | `deferred` | 需专项映射 |
| `local_shell_call` | `tool_use` name=`shell` | `lossy_supported` | 命令数组拼为文本；环境、超时、用户和工作目录未映射 |
| `local_shell_call_output` | request replay only | `lossy_supported` | 输出作为文本保留，session 以 raw JSON 回放 |
| `shell_call` | `tool_use` name=`shell` | `lossy_supported` | 命令数组拼为文本；执行环境、调用者与限制未映射 |
| `shell_call_output` | request replay only | `lossy_supported` | stdout/stderr 拼为文本；结果状态和调用者未映射 |
| `apply_patch_call` | `tool_use` name=`apply_patch` | `lossy_supported` | create/update diff 作为文本；delete 操作及调用者元数据未映射 |
| `apply_patch_call_output` | request replay only | `lossy_supported` | 可选日志作为文本；状态和调用者未映射 |
| `mcp_call` | none | `unsupported_by_backend` | 无等价 |
| `mcp_list_tools` | none | `unsupported_by_backend` | 无等价 |
| `mcp_approval_request` | none | `unsupported_by_backend` | 无等价 |
| `mcp_approval_response` | none | `unsupported_by_backend` | 无等价 |

## Responses SSE Events

| OpenAI SSE event | Anthropic 来源 | 当前状态 | 说明 |
|---|---|---|---|
| `response.created` | `message_start` | `supported` | 已输出 |
| `response.in_progress` | `message_start` | `supported` | 已输出 |
| `response.completed` | `message_stop` | `supported` | 已输出 |
| `response.incomplete` | `message_stop` + stop reason | `lossy_supported` | 枚举需修正 |
| `response.failed` | upstream error | `supported` | 已输出 |
| `error` | Anthropic error event | `deferred` | 当前多映射为 failed，需决定是否同时发 error |
| `response.queued` | none | `unsupported_by_backend` | 后端无队列状态 |
| `response.output_item.added` | content block start | `supported` | text/reasoning/tool use 支持 |
| `response.output_item.done` | content block stop | `supported` | text/reasoning/tool use 支持 |
| `response.content_part.added` | text block start | `supported` | output_text 支持 |
| `response.content_part.done` | text block stop | `supported` | output_text 支持 |
| `response.output_text.delta` | `text_delta` | `supported` | 已输出 |
| `response.output_text.done` | text block stop | `supported` | 已输出 |
| `response.output_text.annotation.added` | `citations_delta` | `deferred` | 需映射 Anthropic citation |
| `response.refusal.delta` | Anthropic refusal | `supported` | 已输出 |
| `response.refusal.done` | Anthropic refusal | `supported` | 已输出 |
| `response.reasoning_text.delta` | `thinking_delta` | `supported` | 已输出 |
| `response.reasoning_text.done` | thinking block stop | `supported` | 已输出 |
| `response.reasoning_summary_part.added` | summarized thinking | `supported` | summarized 模式 |
| `response.reasoning_summary_text.delta` | summarized thinking | `supported` | summarized 模式 |
| `response.reasoning_summary_text.done` | summarized thinking | `supported` | summarized 模式 |
| `response.reasoning_summary_part.done` | summarized thinking | `supported` | summarized 模式 |
| `response.function_call_arguments.delta` | `input_json_delta` | `supported` | 普通 function tool |
| `response.function_call_arguments.done` | tool_use stop | `supported` | 普通 function tool |
| `response.custom_tool_call_input.delta` | custom tool stop | `supported` | freeform custom tool |
| `response.custom_tool_call_input.done` | custom tool stop | `supported` | freeform custom tool |
| `response.file_search_call.searching` | none | `unsupported_by_backend` | 无等价 |
| `response.file_search_call.in_progress` | none | `unsupported_by_backend` | 无等价 |
| `response.file_search_call.completed` | none | `unsupported_by_backend` | 无等价 |
| `response.web_search_call.searching` | Anthropic web search | `deferred` | 需专项映射 |
| `response.web_search_call.in_progress` | Anthropic web search | `deferred` | 需专项映射 |
| `response.web_search_call.completed` | Anthropic web search | `deferred` | 需专项映射 |
| `response.code_interpreter_call_code.delta` | Anthropic code execution | `deferred` | 需专项映射 |
| `response.code_interpreter_call_code.done` | Anthropic code execution | `deferred` | 需专项映射 |
| `response.code_interpreter_call.in_progress` | Anthropic code execution | `deferred` | 需专项映射 |
| `response.code_interpreter_call.interpreting` | Anthropic code execution | `deferred` | 需专项映射 |
| `response.code_interpreter_call.completed` | Anthropic code execution | `deferred` | 需专项映射 |
| `response.image_generation_call.in_progress` | none | `unsupported_by_backend` | 无等价 |
| `response.image_generation_call.generating` | none | `unsupported_by_backend` | 无等价 |
| `response.image_generation_call.partial_image` | none | `unsupported_by_backend` | 无等价 |
| `response.image_generation_call.completed` | none | `unsupported_by_backend` | 无等价 |
| `response.mcp_call_arguments.delta` | none | `unsupported_by_backend` | 无等价 |
| `response.mcp_call_arguments.done` | none | `unsupported_by_backend` | 无等价 |
| `response.mcp_call.in_progress` | none | `unsupported_by_backend` | 无等价 |
| `response.mcp_call.completed` | none | `unsupported_by_backend` | 无等价 |
| `response.mcp_call.failed` | none | `unsupported_by_backend` | 无等价 |
| `response.mcp_list_tools.in_progress` | none | `unsupported_by_backend` | 无等价 |
| `response.mcp_list_tools.completed` | none | `unsupported_by_backend` | 无等价 |
| `response.mcp_list_tools.failed` | none | `unsupported_by_backend` | 无等价 |
| `response.audio.delta` | none | `unsupported_by_backend` | 当前后端不产生 audio |
| `response.audio.done` | none | `unsupported_by_backend` | 当前后端不产生 audio |
| `response.audio.transcript.delta` | none | `unsupported_by_backend` | 当前后端不产生 audio transcript |
| `response.audio.transcript.done` | none | `unsupported_by_backend` | 当前后端不产生 audio transcript |

## Anthropic Content Blocks And Stream Events

| Anthropic block/event | OpenAI 映射 | 当前状态 | 说明 |
|---|---|---|---|
| request `text` | message content text | `supported` | 双向常用路径 |
| request `image` | input_image | `supported` | OpenAI URL/data URI 到 Anthropic image |
| request `document` | input_file | `supported` | 文件输入 |
| request `thinking` | reasoning | `supported` | 用于回放 thinking signature |
| request `redacted_thinking` | reasoning encrypted | `supported` | 用于回放 redacted thinking |
| request `tool_use` | function/custom/tool call | `supported` | 常用工具路径 |
| request `tool_result` | tool call output | `supported` | 常用工具结果 |
| stream `text` | output message | `supported` | 已输出 output_text |
| stream `thinking` | reasoning | `supported` | 已输出 reasoning events |
| stream `redacted_thinking` | reasoning encrypted | `supported` | 已存 encrypted_content |
| stream `tool_use` | function/custom tool call | `supported` | 已输出 tool call events |
| stream `server_tool_use` | built-in tool call | `deferred` | 当前静默忽略风险，需补诊断策略 |
| stream `web_search_tool_result` | web search call result | `deferred` | 需专项映射 |
| stream `web_fetch_tool_result` | web fetch result | `unsupported_by_backend` | OpenAI Responses 无直接等价 |
| stream `code_execution_tool_result` | code interpreter output | `deferred` | 需专项映射 |
| stream `bash_code_execution_tool_result` | shell/code output | `deferred` | 需专项映射 |
| stream `text_editor_code_execution_tool_result` | apply_patch/text editor output | `deferred` | 需专项映射 |
| stream `tool_search_tool_result` | tool_search_output | `deferred` | 需专项映射 |
| stream `container_upload` | none | `unsupported_by_backend` | 无 OpenAI Responses 等价输出 |
| stream `mid_conversation_system` | none | `deferred` | 需决定是否转 developer/system marker |
| event `ping` | no-op | `supported` | 忽略是正确行为 |
| event `message_start` | `response.created/in_progress` | `supported` | 已处理 |
| event `content_block_start` | item/content start | `lossy_supported` | 部分 block 类型未处理 |
| event `content_block_delta` | delta events | `lossy_supported` | citation/server tool delta 未处理 |
| event `content_block_stop` | done events | `lossy_supported` | 未知 block stop 未诊断 |
| event `message_delta` | stop reason / usage | `lossy_supported` | stop reason 枚举需修正 |
| event `message_stop` | terminal response | `supported` | 已处理 |
| event `error` | response.failed/error | `supported` | raw SSE client 转 synthetic error |

## Enum Mapping

| 枚举类别 | OpenAI 值 | Anthropic 值 | 当前状态 | 说明 |
|---|---|---|---|---|
| role | `user` | `user` | `supported` | 直接映射 |
| role | `assistant` | `assistant` | `supported` | 直接映射 |
| role | `system` | top-level `system` | `lossy_supported` | Anthropic 无 system message role |
| role | `developer` | top-level `system` with marker | `lossy_supported` | 通过 marker 保留层级 |
| assistant phase | `commentary` | none | `lossy_supported` | 通过 `<assistant_phase>` marker |
| assistant phase | `final_answer` | none | `lossy_supported` | 输出 item 保留 phase |
| response status | `in_progress` | message active | `supported` | created 后输出 |
| response status | `completed` | `end_turn/tool_use/stop_sequence` | `supported` | 需按 stop reason |
| response status | `incomplete` | `max_tokens` | `supported` | reason=`max_output_tokens` |
| response status | `failed` | upstream error | `supported` | response.failed |
| response status | `queued` | none | `unsupported_by_backend` | 无队列状态 |
| response status | `cancelled` | client cancel | `deferred` | 需决定是否生成 cancelled response |
| incomplete reason | `max_output_tokens` | `max_tokens` | `supported` | 直接映射 |
| incomplete reason | `content_filter` | policy/refusal | `supported` | refusal 映射 |
| stop reason | none | `end_turn` | `supported` | completed |
| stop reason | none | `tool_use` | `supported` | completed，客户端继续工具回合 |
| stop reason | none | `stop_sequence` | `supported` | completed |
| stop reason | none | `max_tokens` | `supported` | incomplete/max_output_tokens |
| stop reason | none | `pause_turn` | `lossy_supported` | incomplete，但不写入非法 reason |
| stop reason | none | `refusal` | `supported` | 映射为 content_filter 并输出 refusal 事件 |
| content part | `output_text` | `text` | `supported` | 直接映射 |
| content part | `refusal` | refusal stop/details | `supported` | 已输出 |
| content part | `reasoning_text` | `thinking` | `supported` | streaming part |
| reasoning summary | `summary_text` | summarized thinking | `supported` | summarized 模式 |
| tool choice | `auto` | `auto` | `supported` | 直接映射 |
| tool choice | `required` | `any` | `supported` | 语义近似 |
| tool choice | `none` | `none` | `supported` | 直接映射 |
