# SDK Type Cheatsheet

Naming reference for the SDK type-layer migration. Every identifier below was
confirmed against SDK source (paths/line numbers cited) or the throwaway
`cmd/spike` program whose `go run` output is recorded in §5.

- `github.com/openai/openai-go/v3`            → package `openai`         (aliased `openai` in code)
- `github.com/openai/openai-go/v3/shared`      → package `shared`
- `github.com/openai/openai-go/v3/shared/constant` → package `constant`
- `github.com/openai/openai-go/v3/packages/param`  → package `param`
- `github.com/anthropics/anthropic-sdk-go`     → package `anthropic`

Versions: `openai-go/v3 v3.42.0`, `anthropic-sdk-go v1.57.0`.

Conventions used by both SDKs (Stainless-generated):
- **Param types** (request side, end in `*Param` / `*ParamUnion`) use `omitzero` /
  `inline` JSON tags → `json.Marshal` produces clean, sparse output.
- **Response types** (`Message`, `ResponseStreamEventUnion`, `Response`) use bare
  `json:"field"` tags with NO `omitzero` → they exist for *inbound parsing only*.
  Marshaling them sprays dozens of zero-value fields, so **outbound SSE events
  must be hand-written structs or literal JSON**, never `Marshal(ResponseStreamEventUnion{})`.
- Optional scalars use `param.Opt[T]` with field `.Value` and method `.Valid()`
  (see §3). Discriminated unions are flat structs (`union{OfX *X; OfY *Y}`) —
  set exactly one `Of*` pointer, leave the rest nil.

---

## §1 `anthropic.MessageNewParams` construction

Source: `/tmp/anthropic-sdk-go/message.go:9607`. All fields use `omitzero`.

| Field (message.go line)                                 | Go type                                       | Notes |
|---------------------------------------------------------|-----------------------------------------------|-------|
| `MaxTokens` (9620)                                      | `int64`                                       | Required, no wrapper. |
| `Model` (9696)                                          | `Model` (= `string`, alias at 4282)           | Required. Constants like `ModelClaudeSonnet4_5` (4301). |
| `Messages` (9691)                                       | `[]MessageParam`                              | Required. See `MessageParam` below. |
| `System` (9760)                                         | `[]TextBlockParam`                            | **Plain slice, NOT a union.** Each `TextBlockParam{Text: "..."}` (5635). |
| `Thinking` (9770)                                       | `ThinkingConfigParamUnion`                    | See below. |
| `Tools` (9857)                                          | `[]ToolUnionParam`                            | Each entry: `ToolUnionParam{OfTool: &ToolParam{...}}` (7703). **Element type is `ToolUnionParam`, not `ToolParam`.** |
| `ToolChoice` (9773)                                     | `ToolChoiceUnionParam`                        | `{OfAuto, OfAny, OfTool, OfNone}` (6809). Helper `ToolChoiceParamOfTool(name)` (6800). |
| `Temperature` (9710)                                    | `param.Opt[float64]`                          | |
| `TopP` (9725)                                           | `param.Opt[float64]`                          | |
| `TopK` (9717)                                           | `param.Opt[int64]`                            | |
| `StopSequences` (9754)                                  | `[]string`                                    | |

### `MessageParam` (message.go:4180)
```go
type MessageParam struct {
    Content []ContentBlockParamUnion `json:"content,omitzero"`  // required
    Role    MessageParamRole         `json:"role,omitzero"`     // required
}
```
Role constants (4212): `MessageParamRoleUser`, `MessageParamRoleAssistant`,
`MessageParamRoleSystem`.

Helpers that wrap the common shape:
- `anthropic.NewUserMessage(blocks ...ContentBlockParamUnion) MessageParam`  (4187)
- `anthropic.NewAssistantMessage(blocks ...ContentBlockParamUnion) MessageParam` (4194)

### `ContentBlockParamUnion` (message.go:2069)
Flat union — set exactly one `Of*` pointer. Variants used by this gateway:
| Variant field | Underlying type              | Discriminator |
|---------------|------------------------------|---------------|
| `OfText`               | `*TextBlockParam`               (5635)             | `"text"` |
| `OfImage`              | `*ImageBlockParam`                                 | `"image"` |
| `OfThinking`           | `*ThinkingBlockParam`                              | `"thinking"` |
| `OfRedactedThinking`   | `*RedactedThinkingBlockParam` (5188)               | `"redacted_thinking"` |
| `OfToolUse`            | `*ToolUseBlockParam`                               | `"tool_use"` |
| `OfToolResult`         | `*ToolResultBlockParam`                            | `"tool_result"` |

(11 more server-tool variants exist at 2078-2086; not needed here.)

`TextBlockParam` (5635): `{Text string; Citations []...; CacheControl ...; Type constant.Text}`.
`Type` carries `default:"text"` — leave it zero and the SDK injects `"text"` on marshal.

### `ThinkingConfigParamUnion` (message.go:6609)
```go
type ThinkingConfigParamUnion struct {
    OfEnabled  *ThinkingConfigEnabledParam   // 6658
    OfDisabled *ThinkingConfigDisabledParam  // 6544
    OfAdaptive *ThinkingConfigAdaptiveParam
}
```
- `ThinkingConfigEnabledParam` (6558): `{BudgetTokens int64 (required, 6568); Display ThinkingConfigEnabledDisplay (6575); Type constant.Enabled}`.
  `Display` constants (6596): `ThinkingConfigEnabledDisplaySummarized`, `ThinkingConfigEnabledDisplayOmitted`.
  Helper: `anthropic.ThinkingConfigParamOfEnabled(budgetTokens int64)` (6600).
- `ThinkingConfigDisabledParam` (6544): `{Type constant.Disabled}` — zero-value struct, SDK injects `"disabled"`.

### Minimal construction (verified by spike, see §5 output)
```go
params := anthropic.MessageNewParams{
    Model:     anthropic.ModelClaudeSonnet4_5,
    MaxTokens: 1024,
    System: []anthropic.TextBlockParam{
        {Text: "You are helpful."},
    },
    Messages: []anthropic.MessageParam{
        {
            Role: anthropic.MessageParamRoleUser,
            Content: []anthropic.ContentBlockParamUnion{
                {OfText: &anthropic.TextBlockParam{Text: "Hello"}},
            },
        },
    },
    Thinking: anthropic.ThinkingConfigParamUnion{
        OfEnabled: &anthropic.ThinkingConfigEnabledParam{BudgetTokens: 2048},
    },
    Tools: []anthropic.ToolUnionParam{
        {OfTool: &anthropic.ToolParam{
            Name: "get_weather",
            InputSchema: anthropic.ToolInputSchemaParam{
                Properties: map[string]any{"location": map[string]any{"type": "string"}},
                Required:   []string{"location"},
            },
        }},
    },
}
b, _ := json.Marshal(params) // clean, sparse JSON — verified
```

---

## §2 `anthropic.MessageStreamEventUnion` parsing

Source: `/tmp/anthropic-sdk-go/message.go:5019`. **Flat aggregate** (not a tagged
union) — unmarshal any SSE `data:` payload into it and read fields directly.

```go
type MessageStreamEventUnion struct {
    Message      Message                                `json:"message"`       // 5021
    Type         string                                 `json:"type"`          // 5024
    Delta        MessageStreamEventUnionDelta           `json:"delta"`         // 5026
    Usage        MessageDeltaUsage                      `json:"usage"`         // 5028
    ContentBlock ContentBlockStartEventContentBlockUnion `json:"content_block"` // 5030
    Index        int64                                  `json:"index"`         // 5031
}
```

`Type` values (comment at 5022-5023): `"message_start"`, `"message_delta"`,
`"message_stop"`, `"content_block_start"`, `"content_block_delta"`,
`"content_block_stop"`.

### Switch by `ev.Type` (verified by spike, see §5)
```go
var ev anthropic.MessageStreamEventUnion
if err := json.Unmarshal(dataBytes, &ev); err != nil { return err }

switch ev.Type {
case "message_start":
    // ev.Message (Message, 3470): .ID .Model .Role .Content .StopReason .StopSequence .Usage
    id := ev.Message.ID
    model := ev.Message.Model

case "content_block_start":
    // ev.Index int64
    // ev.ContentBlock (ContentBlockStartEventContentBlockUnion, 4570):
    //   .Type ("text"|"thinking"|"redacted_thinking"|"tool_use"|...)
    //   .Text / .Thinking / .Signature / .Data / .ID / .Name
    blockType := ev.ContentBlock.Type

case "content_block_delta":
    // ev.Index int64
    // ev.Delta (MessageStreamEventUnionDelta, 5130) — flat subunion; switch on
    //   ev.Delta.Type ("text_delta"|"thinking_delta"|"input_json_delta"|
    //                   "signature_delta"|"citations_delta")  (comment at 4424-4425)
    switch ev.Delta.Type {
    case "text_delta":       // TextDelta (5979)
        text := ev.Delta.Text
    case "thinking_delta":   // ThinkingDelta (6673)
        thinking := ev.Delta.Thinking
    case "input_json_delta": // InputJSONDelta (3403) — partial tool-call args
        partial := ev.Delta.PartialJSON
    case "signature_delta":  // SignatureDelta (5580) — thinking signature
        sig := ev.Delta.Signature
    }

case "content_block_stop":
    // ev.Index int64 only

case "message_delta":
    // ev.Delta.StopReason (StopReason, 5598) — end_turn|max_tokens|stop_sequence|
    //   tool_use|pause_turn|refusal  (constants 5601-5606)
    // ev.Delta.StopSequence string
    // ev.Usage (MessageDeltaUsage, 4142): .OutputTokens .InputTokens
    //   .CacheCreationInputTokens .CacheReadInputTokens
    stop := ev.Delta.StopReason
    out := ev.Usage.OutputTokens

case "message_stop":
    // no payload
}
```

### Error events are NOT a `MessageStreamEventUnion` variant
The SDK's SSE decoder intercepts `event: error` and converts it to a Go error on
`Stream.Err()` — it is never surfaced as a `MessageStreamEventUnion`.

Source: `packages/ssestream/ssestream.go:209-216`:
```go
case "error":
    data := s.decoder.Event().Data
    if ed, ok := s.decoder.(richErrorDecoder); ok {
        s.err = ed.newAPIError(data)  // *apierror.Error
    } else {
        s.err = fmt.Errorf("received error while streaming: %s", string(data))
    }
    return false
```
**Migration impact:** the current gateway surfaces mid-stream error events as
`response.failed` (commit `296ef78`) by parsing raw SSE. After switching to the
SDK's `MessageStream`, error events arrive via `stream.Err()` instead — the
converter needs an `if err := stream.Err(); err != nil { emit response.failed }`
branch. The 6 `Type` values above are the only ones that appear inside
`MessageStreamEventUnion`.

---

## §3 `responses.ResponseNewParams` field access (OpenAI side)

Source: `/tmp/openai-go/responses/response.go:25696`. Controller-verified
(UnmarshalJSON parses Codex requests); field names re-confirmed from source.
All optional scalars are `param.Opt[T]` — read with `.Valid()` then `.Value`
(**field, not method**; `param.Opt` defined at `packages/param/option.go:30`).

| Field (response.go line)           | Go type                                    | Access |
|------------------------------------|--------------------------------------------|--------|
| `Input` (25854)                    | `ResponseNewParamsInputUnion`              | `ev.Input.OfString` (`param.Opt[string]`) or `ev.Input.OfInputItemList` (`ResponseInputParam = []ResponseInputItemUnionParam`, 10795). Union at 25962. |
| `Model` (25860)                    | `shared.ResponsesModel` (= `string`)       | Plain string. |
| `MaxOutputTokens` (25709)          | `param.Opt[int64]`                         | `if p.MaxOutputTokens.Valid() { n := p.MaxOutputTokens.Value }` |
| `PreviousResponseID` (25721)       | `param.Opt[string]`                        | `.Valid()` / `.Value` |
| `Instructions` (25705)             | `param.Opt[string]`                        | `.Valid()` / `.Value` |
| `Temperature` (25728)              | `param.Opt[float64]`                       | `.Valid()` / `.Value` |
| `TopP` (25738)                     | `param.Opt[float64]`                       | `.Valid()` / `.Value` |
| `Store` (25723)                    | `param.Opt[bool]`                          | `.Valid()` / `.Value` |
| `Reasoning` (25875)                | `shared.ReasoningParam` (shared.go:919)    | `.Effort ReasoningEffort` (934); `.Summary ReasoningSummary` (953); `.GenerateSummary` (944, deprecated). Effort values: `none/minimal/low/medium/high/xhigh/max`. |
| `Text` (25881)                     | `ResponseTextConfigParam` (21908)          | `.Format ResponseFormatTextConfigUnionParam` (21928) with `.OfText`/`.OfJSONSchema`/`.OfJSONObject` (union at 8203). `.GetType() *string` returns discriminator (`"text"`/`"json_schema"`/`"json_object"`). `.Verbosity` (21914). |
| `Tools` (25905)                    | `[]ToolUnionParam`                         | Tool union; `OfFunction` variant carries custom function tools. |
| `ToolChoice` (25885)               | `ResponseNewParamsToolChoiceUnion`         | |
| `Include` (25781)                  | `[]ResponseIncludable`                     | 8 constants at 10761-10768 (e.g. `ResponseIncludableReasoningEncryptedContent = "reasoning.encrypted_content"`). |

### `param.Opt[T]` API (packages/param/option.go:30)
```go
type Opt[T comparable] struct {
    Value T        // field, NOT a method — read directly after Valid()
    // ...
}
func (o Opt[T]) Valid() bool       // true unless omitted or null
func (o Opt[T]) Or(v T) T          // value if Valid, else v
```
So `MaxOutputTokens.Value` is a **field access**, despite the task brief writing
`.Value()`. Same shape for `PreviousResponseID.Value`, `Temperature.Value`, etc.

---

## §4 Outbound event `type` constants (OpenAI Responses SSE)

Source: `/tmp/openai-go/shared/constant/constants.go`. Each constant is a named
string type with a `Default() <Self>` method (lines 522, 761-909) returning the
wire string. The wire string is the authoritative value for hand-written
outbound SSE; the constant type itself is only useful when populating an SDK
*response* struct (which we do NOT marshal for outbound — see top of file).

Pattern: `constant.ResponseCreated{}.Default()` == `"response.created"`. A
zero-value `constant.ResponseCreated` also marshals to `"response.created"` via
the SDK's `default:"..."` machinery, but we emit literal strings in SSE writers.

Constants actually emitted by this gateway (verified values from the `Default()`
methods):

| Constant type (constants.go line)            | Wire string                          |
|----------------------------------------------|--------------------------------------|
| `Error` (103, Default 522)                   | `"error"`                            |
| `ResponseCreated` (268, 795)                 | `"response.created"`                 |
| `ResponseInProgress` (282, 831)              | `"response.in_progress"`             |
| `ResponseCompleted` (264, 787)               | `"response.completed"`               |
| `ResponseIncomplete` (283, 832)              | `"response.incomplete"`              |
| `ResponseFailed` (272, 803)                  | `"response.failed"`                  |
| `ResponseOutputItemAdded` (300, 871)         | `"response.output_item.added"`       |
| `ResponseOutputItemDone` (301, 874)          | `"response.output_item.done"`        |
| `ResponseContentPartAdded` (265, 788)        | `"response.content_part.added"`      |
| `ResponseContentPartDone` (266, 791)         | `"response.content_part.done"`       |
| `ResponseOutputTextDelta` (303, 878)         | `"response.output_text.delta"`       |
| `ResponseOutputTextDone` (304, 881)          | `"response.output_text.done"`        |
| `ResponseReasoningTextDelta` (310, 895)      | `"response.reasoning_text.delta"`    |
| `ResponseReasoningTextDone` (311, 898)       | `"response.reasoning_text.done"`     |
| `ResponseReasoningSummaryPartAdded` (306, 883)  | `"response.reasoning_summary_part.added"`  |
| `ResponseReasoningSummaryPartDone` (307, 886)   | `"response.reasoning_summary_part.done"`   |
| `ResponseReasoningSummaryTextDelta` (308, 889)  | `"response.reasoning_summary_text.delta"`  |
| `ResponseReasoningSummaryTextDone` (309, 892)   | `"response.reasoning_summary_text.done"`   |
| `ResponseFunctionCallArgumentsDelta` (276, 813) | `"response.function_call_arguments.delta"` |
| `ResponseFunctionCallArgumentsDone` (277, 816)  | `"response.function_call_arguments.done"`  |

Full set in the file covers ~56 event types (audio, code_interpreter, file_search,
web_search, mcp_call, image_generation, refusal, etc.) — pull more from
`constants.go` lines 761-909 as needed.

---

## §5 Spike verification (`cmd/spike`, throwaway)

`go run ./cmd/spike` produced (verbatim) the confirmation that every name in §1/§2
compiles and round-trips:

```
=== MessageNewParams JSON ===
{"max_tokens":1024,"messages":[{"content":[{"text":"Hello","type":"text"}],"role":"user"}],"model":"claude-sonnet-4-5","system":[{"text":"You are helpful.","type":"text"}],"thinking":{"budget_tokens":2048,"type":"enabled"},"tools":[{"input_schema":{"properties":{"location":{"type":"string"}},"required":["location"],"type":"object"},"name":"get_weather"}]}

=== MessageNewParams (thinking disabled) ===
{"max_tokens":64,"messages":[{"content":[{"text":"ping","type":"text"}],"role":"user"}],"model":"claude-sonnet-4-5","thinking":{"type":"disabled"}}

=== MessageStreamEventUnion parse ===
[message_start]        Message.ID=msg_1 Message.Model=claude-sonnet-4-5
[content_block_start]  Index=0 ContentBlock.Type=text
[content_block_start]  Index=1 ContentBlock.Type=thinking
[content_block_start]  Index=2 ContentBlock.Type=tool_use
[content_block_delta]  Index=0 Delta.Type=text_delta Delta.Text="Hi"
[content_block_delta]  Index=1 Delta.Type=thinking_delta Delta.Thinking="hmm"
[content_block_delta]  Index=2 Delta.Type=input_json_delta Delta.PartialJSON="{\"a\":"
[content_block_delta]  Index=1 Delta.Type=signature_delta Delta.Signature="sig"
[content_block_stop]   Index=0
[message_delta]        Delta.StopReason=end_turn Usage.OutputTokens=42
[message_stop]
```

Observations load-bearing for the migration:
1. `Marshal(MessageNewParams)` is sparse and clean — `omitzero` works. Param
   types are safe to construct and marshal directly for the upstream Anthropic
   call.
2. The `default:"..."` tag injects `"text"`/`"object"`/`"enabled"`/`"disabled"`
   on the wire even when the `Type`/`constant` field is left zero — no need to
   set discriminator fields manually on the param side.
3. `MessageStreamEventUnion` parses all six event types and all four
   in-scope delta subtypes with the exact field names listed in §2.
4. **No `error` variant exists on `MessageStreamEventUnion`** — `event: error`
   is consumed by the SDK's SSE decoder and surfaced via `Stream.Err()`
   (`packages/ssestream/ssestream.go:209-216`). The current raw-SSE error
   handling (commit 296ef78) must move to a `stream.Err()` branch.
