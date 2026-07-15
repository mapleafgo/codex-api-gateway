# SDK 协议类型层迁移 实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 用 openai-go/v3 + anthropic-sdk-go 替换手写协议 model，根治 Response 协议 P0+P1+P2 完整性差距，保留 HTTP 多源容灾与 SSE 中继。

**Architecture:** 入站用 SDK 类型解析/构造（`ResponseNewParams` 解析、`MessageNewParams` 构造、`MessageStreamEventUnion` 解析流）；出站用自定义 `omitempty` 事件 struct + SDK `shared/constant` 常量（SDK 非 Param 类型 Marshal 有零值污染，不直接用）；HTTP 容灾（scheduler/breaker/failover）与 SSE 中继保留。

**Tech Stack:** Go 1.26、github.com/openai/openai-go/v3@v3.42.0、github.com/anthropics/anthropic-sdk-go@v1.57.0、gopkg.in/yaml.v3

## Global Constraints

- 仅引入上述两个 SDK；依赖经 goproxy.cn 可得（openai-go/v3@v3.42.0、anthropic-sdk-go@v1.57.0 均已确认）
- 出站事件/对象**不得**用 `map[string]any` 或字符串拼接 JSON；**不得**直接 `json.Marshal` SDK 非 Param 类型（union/Response/OutputItemUnion 有零值污染）
- 出站事件 `type` 字段一律引用 SDK `shared/constant` 常量值，杜绝拼错
- HTTP 容灾层（scheduler/breaker/failover）、SSE 中继（writeSSE）逻辑不动，仅适配新类型签名
- 每个任务结束 `go build ./...` 通过、相关包 `go test` 绿、commit
- config.yaml 含 api_key，任何输出/日志掩码

## 接口契约（改造后，所有任务共同遵循）

```go
// convert
func ToAnthropic(req *responses.ResponseNewParams, cfg *config.Config) (*anthropic.MessageNewParams, error)

// anthropic
func (c *Client) Stream(ctx context.Context, baseURL, apiKey string, req *anthropic.MessageNewParams) (io.ReadCloser, error)
func ScanEvents(r io.Reader, fn func(*anthropic.MessageStreamEventUnion) error) error

// scheduler
func (s *Scheduler) Execute(ctx context.Context, req *anthropic.MessageNewParams, onEvent func(*anthropic.MessageStreamEventUnion) error) error

// streamconv
func New() *Converter
func (c *Converter) Feed(ev *anthropic.MessageStreamEventUnion) ([]model.SSEEvent, error)
func (c *Converter) RespID() string
func (c *Converter) OutputItems() []model.OutputItem

// store
type Entry struct { SourceName string; Items []model.OutputItem; expiresAt time.Time }
func (s *SessionStore) Save(responseID, sourceName string, items []model.OutputItem)
func (s *SessionStore) Enrich(req *responses.ResponseNewParams, targetSource string) error

// server handleResponses: decode *responses.ResponseNewParams → Enrich → ToAnthropic → sch.Execute → conv.Feed → writeSSE → Save
```

---

### Task 1: 引入依赖 + Anthropic 侧 spike + 类型使用手册

**Files:**
- Modify: `go.mod`, `go.sum`
- Create: `docs/sdk-type-cheatsheet.md`（类型使用手册，后续任务参照）

**Interfaces:**
- Produces: 确认 `anthropic.MessageNewParams`/`MessageParam`/`ContentBlockParamUnion`/`ThinkingConfigParam` 构造模式、`MessageStreamEventUnion` 解析模式、`responses.ResponseNewParams` 字段访问模式、`shared/constant` 常量值；记录到 cheatsheet

- [ ] **Step 1: go get 两个 SDK**

Run:
```bash
go get github.com/openai/openai-go/v3@v3.42.0
go get github.com/anthropics/anthropic-sdk-go@v1.57.0
go mod tidy
```
Expected: `go.mod` 新增两个 require；`go build ./...` 仍通过（尚未引用，无副作用）。

- [ ] **Step 2: 写 spike 验证 Anthropic 请求构造**

Create `cmd/spike/main.go`（临时，Task 8 前删除）:
```go
package main

import (
	"encoding/json"
	"fmt"

	"github.com/anthropics/anthropic-sdk-go"
)

func main() {
	// 构造一个带 system + user 文本 + thinking 的请求
	req := anthropic.MessageNewParams{
		Model:     anthropic.Model("claude-sonnet-4-20250514"),
		MaxTokens: 4096,
		System: anthropic.TextBlockParam{Text: "be brief"}, // 验证 system 写法；若类型不同按编译错误调整
		Messages: []anthropic.MessageParam{{
			Role: anthropic.MessageParamRoleUser,
			Content: anthropic.ContentBlockParamUnion{OfText: &anthropic.TextBlockParam{Text: "hi"}},
		}},
		Thinking: anthropic.ThinkingConfigParam{OfEnabled: &anthropic.ThinkingConfigEnabledParam{BudgetTokens: 8000}},
	}
	b, err := json.Marshal(req)
	fmt.Println("err:", err)
	fmt.Println("REQ:", string(b))

	// 解析一个 Anthropic SSE 流事件
	sse := `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`
	var ev anthropic.MessageStreamEventUnion
	err2 := json.Unmarshal([]byte(sse), &ev)
	fmt.Println("unmarshal err:", err2, "type:", ev.Type)
}
```
Run: `go run ./cmd/spike`
Expected: 输出合法 JSON 请求（`max_tokens`/`messages`/`model`/`thinking`/`system` 字段正确，无多余零值）；流事件解析 `type=content_block_start`。

> 若 `System`/`Content`/`Thinking` 字段名或变体名（OfText/OfEnabled 等）与 SDK 不符，**按编译错误修正为 SDK 实际命名**，并把确认后的写法记入 cheatsheet。这一步就是要消除命名不确定。

- [ ] **Step 3: 写 cheatsheet**

Create `docs/sdk-type-cheatsheet.md`，记录经 spike 确认的：
- `anthropic.MessageNewParams` 构造（system/messages/content 变体/thinking 变体/max_tokens/model）
- `anthropic.MessageStreamEventUnion` 按 `ev.Type` switch 的 6 个分支（message_start/delta/stop/content_block_start/delta/stop）及各分支字段访问
- `responses.ResponseNewParams` 字段访问（`req.Model`、`req.Input.OfString/OfInputItemList`、`req.Reasoning.Effort`、`req.Tools`、`req.Text.Format.Type`、`req.MaxOutputTokens.Value()`、`req.Include`）
- 出站事件 `type` 常量值表（从 `shared/constant` 包读取）：`response.created/in_progress/completed/incomplete/failed`、`response.output_item.added/done`、`response.content_part.added/done`、`response.output_text.delta/done`、`response.reasoning_text.delta/done`、`response.reasoning_summary_part.added/done`、`response.reasoning_summary_text.delta/done`、`response.function_call_arguments.delta/done`、`error`

Run:
```bash
grep -E 'Response(OutputTextDelta|OutputTextDone|ReasoningTextDelta|ReasoningTextDone|ReasoningSummaryPartAdded|ReasoningSummaryPartDone|ReasoningSummaryTextDelta|ReasoningSummaryTextDone|OutputItemAdded|OutputItemDone|ContentPartAdded|ContentPartDone|FunctionCallArgumentsDelta|FunctionCallArgumentsDone|Created|InProgress|Completed|Incomplete|Failed) ' ~/go/pkg/mod/github.com/openai/openai-go/v3@v3.42.0/shared/constant/constants.go
```
（或 `$(go env GOMODCACHE)/github.com/openai/openai-go/v3@v3.42.0/shared/constant/constants.go`）把命中的常量名记入 cheatsheet。

- [ ] **Step 4: 删除临时 spike，commit 依赖**

```bash
rm -rf cmd/spike
go build ./...
git add go.mod go.sum docs/sdk-type-cheatsheet.md
git commit -m "chore: 引入 openai-go/v3 + anthropic-sdk-go 依赖与类型手册"
```

---

### Task 2: model 包重建（出站事件 struct + OutputItem + responseObject）

**Files:**
- Create: `internal/model/event.go`（出站事件 struct + SSEEvent）
- Create: `internal/model/outputitem.go`（OutputItem + OutputTextPart）
- Create: `internal/model/responseobject.go`（responseObject，P2 回显）
- Create: `internal/model/event_test.go`
- Delete: `internal/model/response.go`, `internal/model/anthropic.go`（本任务暂保留，Task 8 统一删；此处只新增）

**Interfaces:**
- Produces: `model.SSEEvent`、`model.OutputItem`、出站事件 struct、`model.NewResponseObject(...)`、`model.Constant*` 便捷取值

- [ ] **Step 1: 写 SSEEvent + 出站事件 struct 失败测试**

Create `internal/model/event_test.go`:
```go
package model

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestOutputTextDeltaMarshalClean(t *testing.T) {
	ev := outputTextDeltaEvent{
		Type: "response.output_text.delta", SequenceNumber: 3,
		OutputIndex: 1, ContentIndex: 0, ItemID: "msg_1", Delta: "hi",
	}
	b, _ := json.Marshal(ev)
	s := string(b)
	for _, bad := range []string{"logprobs", "refusal", "code", "param"} {
		if strings.Contains(s, bad) {
			t.Fatalf("unexpected field %q in %s", bad, s)
		}
	}
	if !strings.Contains(s, `"delta":"hi"`) || !strings.Contains(s, `"sequence_number":3`) {
		t.Fatalf("missing expected fields: %s", s)
	}
}

func TestResponseObjectMarshalHasRequired(t *testing.T) {
	obj := NewResponseObject("resp_1", "completed", "gpt-5", 100, 0.7, 4096)
	b, _ := json.Marshal(wrapResponseObject(obj, "response.completed", 9))
	s := string(b)
	for _, field := range []string{`"object":"response"`, `"output":[`, `"created_at":`, `"id":"resp_1"`, `"sequence_number":9`} {
		if !strings.Contains(s, field) {
			t.Fatalf("missing %q in %s", field, s)
		}
	}
}
```
Run: `go test ./internal/model/` → 预期编译失败（类型未定义）。

- [ ] **Step 2: 实现 SSEEvent + 事件 struct**

Create `internal/model/event.go`:
```go
package model

import "encoding/json"

// SSEEvent is one server-sent event to emit to the Codex client.
// Data holds the already-marshaled event JSON.
type SSEEvent struct {
	Type string
	Data json.RawMessage
}

// event is the minimal envelope; each concrete event embeds it implicitly
// via its own fields. We define one struct per event with omitempty so
// json.Marshal produces clean, protocol-correct JSON (no SDK union pollution).
type outputTextDeltaEvent struct {
	Type           string `json:"type"`
	SequenceNumber int64  `json:"sequence_number,omitempty"`
	OutputIndex    int    `json:"output_index"`
	ContentIndex   int    `json:"content_index,omitempty"`
	ItemID         string `json:"item_id"`
	Delta          string `json:"delta"`
}

type outputTextDoneEvent struct {
	Type           string      `json:"type"`
	SequenceNumber int64       `json:"sequence_number,omitempty"`
	OutputIndex    int         `json:"output_index"`
	ContentIndex   int         `json:"content_index,omitempty"`
	ItemID         string      `json:"item_id"`
	Text           string      `json:"text"`
}

type reasoningTextDeltaEvent struct {
	Type           string `json:"type"`
	SequenceNumber int64  `json:"sequence_number,omitempty"`
	OutputIndex    int    `json:"output_index"`
	ContentIndex   int    `json:"content_index,omitempty"`
	ItemID         string `json:"item_id"`
	Delta          string `json:"delta"`
}

type reasoningTextDoneEvent struct {
	Type           string `json:"type"`
	SequenceNumber int64  `json:"sequence_number,omitempty"`
	OutputIndex    int    `json:"output_index"`
	ContentIndex   int    `json:"content_index,omitempty"`
	ItemID         string `json:"item_id"`
	Text           string `json:"text"`
}

type reasoningSummaryPartAddedEvent struct {
	Type           string      `json:"type"`
	SequenceNumber int64       `json:"sequence_number,omitempty"`
	OutputIndex    int         `json:"output_index"`
	SummaryIndex   int         `json:"summary_index"`
	ItemID         string      `json:"item_id"`
	Part           summaryPart `json:"part"`
}

type reasoningSummaryPartDoneEvent struct {
	Type           string      `json:"type"`
	SequenceNumber int64       `json:"sequence_number,omitempty"`
	OutputIndex    int         `json:"output_index"`
	SummaryIndex   int         `json:"summary_index"`
	ItemID         string      `json:"item_id"`
	Part           summaryPart `json:"part"`
}

type reasoningSummaryTextDeltaEvent struct {
	Type           string `json:"type"`
	SequenceNumber int64  `json:"sequence_number,omitempty"`
	OutputIndex    int    `json:"output_index"`
	SummaryIndex   int   `json:"summary_index"`
	ItemID         string `json:"item_id"`
	Delta          string `json:"delta"`
}

type reasoningSummaryTextDoneEvent struct {
	Type           string `json:"type"`
	SequenceNumber int64  `json:"sequence_number,omitempty"`
	OutputIndex    int    `json:"output_index"`
	SummaryIndex   int   `json:"summary_index"`
	ItemID         string `json:"item_id"`
	Delta          string `json:"delta"`
}

type contentPartAddedEvent struct {
	Type           string         `json:"type"`
	SequenceNumber int64          `json:"sequence_number,omitempty"`
	OutputIndex    int            `json:"output_index"`
	ContentIndex   int            `json:"content_index"`
	ItemID         string         `json:"item_id"`
	Part           contentPartOut `json:"part"`
}

type contentPartDoneEvent struct {
	Type           string         `json:"type"`
	SequenceNumber int64          `json:"sequence_number,omitempty"`
	OutputIndex    int            `json:"output_index"`
	ContentIndex   int            `json:"content_index"`
	ItemID         string         `json:"item_id"`
	Part           contentPartOut `json:"part"`
}

type functionCallArgumentsDeltaEvent struct {
	Type           string `json:"type"`
	SequenceNumber int64  `json:"sequence_number,omitempty"`
	OutputIndex    int    `json:"output_index"`
	ItemID         string `json:"item_id"`
	Delta          string `json:"delta"`
}

type functionCallArgumentsDoneEvent struct {
	Type           string `json:"type"`
	SequenceNumber int64  `json:"sequence_number,omitempty"`
	OutputIndex    int    `json:"output_index"`
	ItemID         string `json:"item_id"`
	Arguments      string `json:"arguments"`
}

type outputItemAddedEvent struct {
	Type           string    `json:"type"`
	SequenceNumber int64     `json:"sequence_number,omitempty"`
	OutputIndex    int       `json:"output_index"`
	Item           OutputItem `json:"item"`
}

type outputItemDoneEvent struct {
	Type           string    `json:"type"`
	SequenceNumber int64     `json:"sequence_number,omitempty"`
	OutputIndex    int       `json:"output_index"`
	Item           OutputItem `json:"item"`
}

// terminalResponseEvent wraps responseObject for created/in_progress/
// completed/incomplete/failed events.
type terminalResponseEvent struct {
	Type           string         `json:"type"`
	SequenceNumber int64          `json:"sequence_number,omitempty"`
	Response       responseObject `json:"response"`
}

type responseErrorEvent struct {
	Type           string `json:"type"`
	SequenceNumber int64  `json:"sequence_number,omitempty"`
	Code           string `json:"code,omitempty"`
	Message        string `json:"message"`
	Param          string `json:"param,omitempty"`
}

// summaryPart is one reasoning summary content part (text).
type summaryPart struct {
	Type string `json:"type"` // "summary_text"
	Text string `json:"text"`
}

// contentPartOut is one content part emitted in content_part.added/done.
type contentPartOut struct {
	Type string `json:"type"` // output_text | refusal
	Text string `json:"text,omitempty"`
}

// MarshalEvent marshals any event struct into an SSEEvent.
func MarshalEvent(eventType string, v any) SSEEvent {
	b, _ := json.Marshal(v)
	return SSEEvent{Type: eventType, Data: b}
}
```

- [ ] **Step 3: 实现 OutputItem**

Create `internal/model/outputitem.go`:
```go
package model

// OutputItem is a self-contained output item (message/function_call/reasoning)
// used both for emitted output_item.added/done events and for session storage.
// It uses omitempty so Marshal stays clean (unlike SDK ResponseOutputItemUnion).
type OutputItem struct {
	Type      string         `json:"type"` // message | function_call | reasoning
	ID        string         `json:"id"`
	Status    string         `json:"status,omitempty"`
	Role      string         `json:"role,omitempty"`           // message
	Content   []OutputText   `json:"content,omitempty"`        // message
	CallID    string         `json:"call_id,omitempty"`        // function_call
	Name      string         `json:"name,omitempty"`           // function_call
	Arguments string         `json:"arguments,omitempty"`      // function_call
	Summary   []OutputText   `json:"summary,omitempty"`        // reasoning
	EncryptedContent string  `json:"encrypted_content,omitempty"` // reasoning (redacted)
}

// OutputText is one text content/summary part.
type OutputText struct {
	Type string `json:"type"` // output_text | summary_text
	Text string `json:"text"`
}
```

- [ ] **Step 4: 实现 responseObject（P2 回显）**

Create `internal/model/responseobject.go`:
```go
package model

// responseObject is the `response` object embedded in created/in_progress/
// completed/incomplete/failed events. Fields use omitempty so we emit exactly
// the P2 fields we can populate, without SDK Response's 35+ zero fields.
type responseObject struct {
	ID                 string         `json:"id"`
	Object             string         `json:"object"` // always "response"
	Status             string         `json:"status"`
	Model              string         `json:"model"`
	CreatedAt          int64          `json:"created_at"`
	CompletedAt        int64          `json:"completed_at,omitempty"`
	Output             []OutputItem   `json:"output"`
	Usage              *responseUsage `json:"usage,omitempty"`
	IncompleteDetails  *incompleteDetails `json:"incomplete_details,omitempty"`
	Instructions       string         `json:"instructions,omitempty"`
	Temperature        *float64       `json:"temperature,omitempty"`
	TopP               *float64       `json:"top_p,omitempty"`
	MaxOutputTokens    *int64         `json:"max_output_tokens,omitempty"`
	ToolChoice         any            `json:"tool_choice,omitempty"`
	ParallelToolCalls  *bool          `json:"parallel_tool_calls,omitempty"`
	Reasoning          any            `json:"reasoning,omitempty"`
	PreviousResponseID string         `json:"previous_response_id,omitempty"`
	Truncation         string         `json:"truncation,omitempty"`
}

type responseUsage struct {
	InputTokens  int   `json:"input_tokens"`
	OutputTokens int   `json:"output_tokens"`
	TotalTokens  int   `json:"total_tokens"`
}

type incompleteDetails struct {
	Reason string `json:"reason"` // max_output_tokens | content_filter
}

// ResponseObjectParams carries request echo fields into NewResponseObject.
type ResponseObjectParams struct {
	Instructions       string
	Temperature        *float64
	TopP               *float64
	MaxOutputTokens    *int64
	ToolChoice         any
	ParallelToolCalls  *bool
	Reasoning          any
	PreviousResponseID string
	Truncation         string
}

// NewResponseObject builds a responseObject with echoed request fields.
func NewResponseObject(id, status, model string, createdAt int64, p ResponseObjectParams) responseObject {
	return responseObject{
		ID: id, Object: "response", Status: status, Model: model, CreatedAt: createdAt,
		Instructions: p.Instructions, Temperature: p.Temperature, TopP: p.TopP,
		MaxOutputTokens: p.MaxOutputTokens, ToolChoice: p.ToolChoice,
		ParallelToolCalls: p.ParallelToolCalls, Reasoning: p.Reasoning,
		PreviousResponseID: p.PreviousResponseID, Truncation: p.Truncation,
	}
}

// wrapResponseObject wraps a responseObject into a terminal event for tests/helpers.
func wrapResponseObject(obj responseObject, eventType string, seq int64) terminalResponseEvent {
	return terminalResponseEvent{Type: eventType, SequenceNumber: seq, Response: obj}
}
```

- [ ] **Step 5: 测试通过 + commit**

Run: `go test ./internal/model/` → PASS。
```bash
git add internal/model/event.go internal/model/outputitem.go internal/model/responseobject.go internal/model/event_test.go
git commit -m "feat(model): 自定义出站事件 struct + OutputItem + responseObject"
```

---

### Task 3: convert 重写（ResponseNewParams → MessageNewParams）

**Files:**
- Rewrite: `internal/convert/request.go`
- Rewrite: `internal/convert/image.go`
- Rewrite: `internal/convert/request_test.go`

**Interfaces:**
- Consumes: `responses.ResponseNewParams`、`config.Config`、`docs/sdk-type-cheatsheet.md`（Anthropic 构造模式）
- Produces: `convert.ToAnthropic(*responses.ResponseNewParams, *config.Config) (*anthropic.MessageNewParams, error)`

- [ ] **Step 1: 写转换失败测试**

参考 cheatsheet 的构造模式。`internal/convert/request_test.go` 选取 3 个核心用例改写：
```go
package convert

import (
	"encoding/json"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/openai/openai-go/v3/shared"
	"github.com/openai/openai-go/v3/shared/param"
	oairesponses "github.com/openai/openai-go/v3/responses"
	"openai2response/internal/config"
)

func mustReq(t *testing.T, body string) *oairesponses.ResponseNewParams {
	t.Helper()
	var r oairesponses.ResponseNewParams
	if err := json.Unmarshal([]byte(body), &r); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return &r
}

func TestTextRequestConverts(t *testing.T) {
	req := mustReq(t, `{"model":"gpt-5","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],"stream":true}`)
	out, err := ToAnthropic(req, &config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if out.MaxTokens == 0 {
		t.Fatal("max_tokens default not set")
	}
	if len(out.Messages) != 1 || out.Messages[0].Role != anthropic.MessageParamRoleUser {
		t.Fatalf("user message not converted: %+v", out.Messages)
	}
}

func TestReasoningEffortMapsToThinking(t *testing.T) {
	req := mustReq(t, `{"model":"gpt-5","input":"hi","reasoning":{"effort":"high"},"stream":true}`)
	out, _ := ToAnthropic(req, &config.Config{Thinking: config.ThinkingCfg{EffortBudget: map[string]int{"high": 32000}}})
	if out.Thinking == nil { // field may be zero-valued; assert via marshal instead
		b, _ := json.Marshal(out)
		t.Fatalf("thinking not set: %s", b)
	}
}

func TestDeveloperRoleFoldsToSystem(t *testing.T) {
	req := mustReq(t, `{"model":"gpt-5","instructions":"be brief","input":[{"type":"message","role":"developer","content":[{"type":"input_text","text":"rules"}]},{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],"stream":true}`)
	out, _ := ToAnthropic(req, &config.Config{})
	b, _ := json.Marshal(out)
	if !strings.Contains(string(b), `"system"`) {
		t.Fatalf("developer not folded to system: %s", b)
	}
}
```
（补充 `import "strings"`；`config.ThinkingCfg` 字段名按实际 config 包调整。）
Run: `go test ./internal/convert/` → 编译失败（旧 ToAnthropic 签名不符）。

- [ ] **Step 2: 重写 image.go（SDK 类型）**

参考 cheatsheet 的 ContentBlock 变体。核心：把 `input_image` URL 转成 `anthropic.ContentBlockParamUnion`（base64 或 url source）。
```go
package convert

import (
	"encoding/base64"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
)

func imageBlock(url string) anthropic.ContentBlockParamUnion {
	if isDataURI(url) {
		media, data := splitDataURI(url)
		dec, _ := base64.StdEncoding.DecodeString(data)
		_ = media
		return anthropic.ContentBlockParamUnion{
			OfImage: &anthropic.ImageBlockParam{
				Source: anthropic.ImageBlockParamSource{OfBase64: &anthropic.Base64ImageSourceParam{
					MediaType: anthropic.Base64ImageSourceParamMediaType(media),
					Data:      base64.StdEncoding.EncodeToString(dec),
				}},
			},
		} // 字段名按 cheatsheet/spike 修正
	}
	return anthropic.ContentBlockParamUnion{
		OfImage: &anthropic.ImageBlockParam{
			Source: anthropic.ImageBlockParamSource{OfURL: &anthropic.URLImageSourceParam{URL: url}},
		},
	}
}
func isDataURI(s string) bool { return strings.HasPrefix(s, "data:") }
func splitDataURI(s string) (media, data string) { /* 复用旧实现 */ return }
```
> 字段名（OfImage/OfBase64/OfURL/MediaType）以 spike 确认为准——这是 Task 1 cheatsheet 存在的原因。实现者在此按 cheatsheet 套用确认命名。

- [ ] **Step 3: 重写 request.go 主转换**

按 cheatsheet 的 MessageNewParams 构造模式。逻辑沿用旧实现（role 折叠、reasoning→thinking、tool 转换、structured output 注入），仅类型换 SDK：
```go
package convert

import (
	"encoding/json"
	"fmt"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/openai/openai-go/v3/shared/param"
	oairesponses "github.com/openai/openai-go/v3/responses"
	"openai2response/internal/config"
)

func ToAnthropic(req *oairesponses.ResponseNewParams, cfg *config.Config) (*anthropic.MessageNewParams, error) {
	out := &anthropic.MessageNewParams{
		Model:     anthropic.Model(resolveModelName(req.Model)),
		MaxTokens: 4096,
	}
	if v := req.MaxOutputTokens; v.Valid() && v.Value() > 0 {
		out.MaxTokens = v.Value()
	}
	out.Temperature = req.Temperature
	out.TopP = req.TopP
	var sysParts []string
	for _, it := range req.Input.OfInputItemList { // ResponseInputParam = []ResponseInputItemUnionParam
		// 按 it.OfMessage / it.OfReasoning / it.OfFunctionCall / it.OfFunctionCallOutput 分发
		// role==developer/system 折入 sysParts；其余 append 到 out.Messages
		if err := appendItem(out, &sysParts, &it); err != nil {
			return nil, fmt.Errorf("convert input item: %w", err)
		}
	}
	if len(sysParts) > 0 {
		out.System = anthropic.TextBlockParam{Text: joinNonEmpty("\n", sysParts)} // 按 cheatsheet
	}
	// reasoning → thinking（沿用旧 effort 预算 + budget<max_tokens + summary=concise→summarized 逻辑）
	applyReasoning(out, req, cfg)
	// tools + structured output（沿用旧逻辑，类型换 SDK）
	if err := convertTools(out, req); err != nil {
		return nil, err
	}
	injectStructuredOutput(out, req)
	out.ToolChoice = convertToolChoice(req.ToolChoice)
	return out, nil
}
```
> `appendItem`/`applyReasoning`/`convertTools`/`injectStructuredOutput`/`convertToolChoice` 内部逻辑沿用旧 request.go，类型从手写 model 换成 SDK（`anthropic.MessageParam`/`ContentBlockParamUnion`/`ThinkingConfigParam`/`anthropic.ToolParam`）。每个 helper 按 cheatsheet 构造模式写。**逐个 helper 移植 + 编译**，遇到 SDK 命名不符即查 cheatsheet 修正。

- [ ] **Step 4: 测试通过 + commit**

Run: `go test ./internal/convert/` → PASS。
```bash
git add internal/convert/
git commit -m "feat(convert): ResponseNewParams → MessageNewParams（SDK 类型）"
```

---

### Task 4: anthropic client + scheduler 适配 SDK 类型

**Files:**
- Modify: `internal/anthropic/client.go`
- Modify: `internal/scheduler/scheduler.go`
- Modify: `internal/anthropic/client_test.go`, `internal/scheduler/scheduler_test.go`

**Interfaces:**
- Consumes: `anthropic.MessageNewParams`、`anthropic.MessageStreamEventUnion`
- Produces: 见接口契约

- [ ] **Step 1: client.go 改签名**

`Stream` 参数 `*model.AnthropicRequest` → `*anthropic.MessageNewParams`；`ScanEvents` 回调 `*model.AnthropicEvent` → `*anthropic.MessageStreamEventUnion`。Thinking header：判断 `req.Thinking` 是否非零（SDK param 类型，按 cheatsheet 判 Valid/零值）。
```go
func (c *Client) Stream(ctx context.Context, baseURL, apiKey string, req *anthropic.MessageNewParams) (io.ReadCloser, error) {
	body, err := json.Marshal(req)
	// ... 同旧实现 ...
	// thinking header: SDK ThinkingConfigParam 非零时设置 beta
}
func ScanEvents(r io.Reader, fn func(*anthropic.MessageStreamEventUnion) error) error {
	// 同旧 SSE 扫描；json.Unmarshal 到 *anthropic.MessageStreamEventUnion
}
```
删 `import "openai2response/internal/model"`。

- [ ] **Step 2: scheduler.go 改签名**

`Execute`/`trySource` 的 `req *model.AnthropicRequest` → `*anthropic.MessageNewParams`；`onEvent func(*model.AnthropicEvent)` → `func(*anthropic.MessageStreamEventUnion)`。浅拷贝改 Model：
```go
resolvedReq := *req
resolvedReq.Model = anthropic.Model(ResolveModel(&src, string(req.Model)))
body, err := s.client.Stream(fbCtx, src.BaseURL, src.APIKey, &resolvedReq)
```
`log.Printf` 里 `req.Model` → `string(req.Model)`。删 `import model`（若不再用）。

- [ ] **Step 3: 更新测试 + 通过 + commit**

把 client/scheduler 测试的构造从手写 `model.AnthropicRequest`/`AnthropicEvent` 换成 SDK 类型（按 cheatsheet）。Run: `go test ./internal/anthropic/ ./internal/scheduler/` → PASS。
```bash
git add internal/anthropic/ internal/scheduler/
git commit -m "refactor(anthropic,scheduler): 适配 SDK 请求/流事件类型"
```

---

### Task 5: streamconv 重写（MessageStreamEventUnion → 出站事件，含 P0/P1）

**Files:**
- Rewrite: `internal/streamconv/converter.go`
- Rewrite: `internal/streamconv/converter_test.go`

**Interfaces:**
- Consumes: `anthropic.MessageStreamEventUnion`、`model` 出站事件、`shared/constant` 常量
- Produces: `Converter.Feed` 返回 `[]model.SSEEvent`、`OutputItems() []model.OutputItem`、`RespID() string`；并暴露 `RequestEcho` 设置入口供 server 注入 P2 回显参数

- [ ] **Step 1: 写核心事件测试（覆盖 P0/P1）**

`converter_test.go` 喂入合成的 `MessageStreamEventUnion`，断言产出事件 `type` 为 SDK 常量值：
```go
// message_start → response.created + response.in_progress
// content_block_start(text) → output_item.added + content_part.added
// content_block_delta(text_delta) → output_text.delta
// content_block_stop → output_text.done + content_part.done + output_item.done
// thinking 块 → reasoning_text.delta/done（非 summarized）或 reasoning_summary_text.*（summarized）
// tool_use → output_item.added + function_call_arguments.delta/done + output_item.done
// message_delta(stop_reason) → 记录
// message_stop → response.completed（status=completed，含 object/output/usage）
// stop_reason=max_tokens → response.incomplete（incomplete_details.reason=max_output_tokens）
```
每个断言：`ev.Type == "response.created"`（用字符串字面量或 constant 常量）且含 `sequence_number` 单调。
Run: `go test ./internal/streamconv/` → 编译失败。

- [ ] **Step 2: 重写 converter.go**

沿用旧 converter 的状态机（openText/openThinking/toolCalls/itemOrder/sigBuilder/usage/completed），事件构造改用 model 出站 struct + constant 常量 + SequenceNumber 递增。关键点：
- `handleMessageStart`：发 `response.created` + `response.in_progress`（P1.4），各含 responseObject（P2 回显，用注入的 RequestEcho）
- `handleBlockStart` text：`output_item.added` + `content_part.added`（P1.7）
- `handleBlockDelta` text_delta：`output_text.delta`
- `handleBlockStop` text：`output_text.done`（P1.6）+ `content_part.done` + `output_item.done`
- thinking：`reasoning_text.delta/done`（P0.1，非 summarized）或 summarized 模式下 `reasoning_summary_part.added` + `reasoning_summary_text.delta/done` + `reasoning_summary_part.done`（P1.8）
- tool_use：`output_item.added` + `function_call_arguments.delta/done` + `output_item.done`
- `handleComplete`：`response.completed`，responseObject 含 `object:"response"`（P0.2）+ `output`（P0.3）+ usage + 回显字段
- `handleError`：`response.error`（P1.9，含 code/message/param）
- stop_reason 非 end_turn/tool_use → `response.incomplete`（P1.5，incomplete_details.reason）
- sequence_number：Converter 持 `seq int64`，每事件 ++

```go
package streamconv

import (
	"github.com/anthropics/anthropic-sdk-go"
	"github.com/openai/openai-go/v3/shared/constant"
	"openai2response/internal/model"
)

type Converter struct {
	respID, model string
	seq           int64
	itemOrder     int
	openText      bool; textItem, textContentIdx int
	openThinking  bool; thinkItem, thinkContentIdx int; summarized bool
	toolCalls     map[int]int; toolIDs map[int]string
	toolArgBuilders map[int]*strings.Builder
	sigBuilder    strings.Builder
	stopReason    string
	usage         *model.responseUsage // 或保存 anthropic usage 字段
	completed     bool
	outputItems   []model.OutputItem
	textBuilder, thinkBuilder strings.Builder
	echo          model.ResponseObjectParams
}

func (c *Converter) nextSeq() int64 { c.seq++; return c.seq }

func (c *Converter) Feed(ev *anthropic.MessageStreamEventUnion) ([]model.SSEEvent, error) {
	switch ev.Type {
	case "message_start": // ev.Message.ID/Model
	case "content_block_start": // ev.Index, ev.ContentBlock（按 .Type 判 text/thinking/redacted_thinking/tool_use）
	case "content_block_delta": // ev.Delta（text_delta/thinking_delta/input_json_delta/signature_delta）
	case "content_block_stop":
	case "message_delta": // ev.Delta.StopReason, ev.Usage
	case "message_stop":
	case "error": // ev.?? Anthropic error 字段
	}
	// 每个分支构造对应 model.*Event，model.MarshalEvent(constantValue, ev) 返回 SSEEvent
}
```
> `ev.ContentBlock`/`ev.Delta` 字段按 cheatsheet（MessageStreamEventUnion 各分支访问）。constant 常量值取自 cheatsheet 第三表。

- [ ] **Step 3: 测试通过 + commit**

Run: `go test ./internal/streamconv/` → PASS。
```bash
git add internal/streamconv/
git commit -m "feat(streamconv): MessageStreamEventUnion → 出站事件（P0/P1 事件齐全）"
```

---

### Task 6: store 适配（OutputItem + Enrich 转换）

**Files:**
- Modify: `internal/store/session.go`
- Modify: `internal/store/session_test.go`

**Interfaces:**
- Consumes: `model.OutputItem`、`responses.ResponseNewParams`
- Produces: `Save(id, source string, items []model.OutputItem)`、`Enrich(req *responses.ResponseNewParams, targetSource string) error`

- [ ] **Step 1: 改 Entry/Save/Get/Enrich 类型**

`Entry.Items` → `[]model.OutputItem`。`Enrich` 接收 `*responses.ResponseNewParams`，把存储的 `OutputItem` 转成 `responses.ResponseInputItemUnionParam` 变体（OfMessage/OfFunctionCall/OfReasoning）前置到 `req.Input.OfInputItemList`：
```go
func (s *SessionStore) Enrich(req *oairesponses.ResponseNewParams, targetSource string) error {
	if !req.PreviousResponseID.Valid() || req.PreviousResponseID.Value() == "" {
		return nil
	}
	e, ok := s.Get(req.PreviousResponseID.Value())
	if !ok { return nil }
	sameSource := e.SourceName == targetSource
	prefix := make([]oairesponses.ResponseInputItemUnionParam, 0, len(e.Items))
	for _, it := range e.Items {
		if it.Type == "reasoning" && !sameSource { continue }
		prefix = append(prefix, toInputItemParam(it)) // 按 cheatsheet 构造变体
	}
	req.Input.OfInputItemList = append(prefix, req.Input.OfInputItemList...)
	return nil
}
```
> `req.PreviousResponseID` 是 `param.Opt[string]`，用 `.Valid()/.Value()`（cheatsheet 确认）。`toInputItemParam` 把 message/function_call/reasoning 的 OutputItem 映射到对应 Of* 变体。

- [ ] **Step 2: 测试改写 + 通过 + commit**

Run: `go test ./internal/store/` → PASS。
```bash
git add internal/store/
git commit -m "refactor(store): OutputItem 存储 + Enrich 转 SDK 入站 item"
```

---

### Task 7: server 适配（ResponseNewParams 解码 + 透传 + P2 回显 + 新事件接线）

**Files:**
- Modify: `internal/server/server.go`
- Modify: `internal/server/server_test.go`

**Interfaces:**
- Consumes: 全部上游任务的产物

- [ ] **Step 1: 改 handleResponses**

```go
var req responses.ResponseNewParams
if err := json.NewDecoder(r.Body).Decode(&req); err != nil { /* 400 */ }
ordered := s.cfg.OrderedSources()
var sourceName string
if len(ordered) > 0 {
	sourceName = ordered[0].Name
	_ = s.sess.Enrich(&req, sourceName)
}
anthReq, err := convert.ToAnthropic(&req, s.cfg)
if err != nil { /* 400 */ }
// ... SSE header ...
conv := streamconv.New()
conv.SetEcho(echoFromRequest(&req)) // 注入 P2 回显参数（instructions/temp/top_p/max_output_tokens/tool_choice/parallel_tool_calls/reasoning/previous_response_id/truncation）
execErr := s.sch.Execute(r.Context(), anthReq, func(ev *anthropic.MessageStreamEventUnion) error {
	out, _ := conv.Feed(ev)
	for _, e := range out { writeSSE(w, e) }
	flusher.Flush(); return nil
})
// success: feed trailing message_stop；error: writeSSE(model.MarshalEvent("response.failed", ...))
id := conv.RespID(); if id == "" { id = newResponseID() }
items := conv.OutputItems(); if len(items) == 0 { items = collectOutput(&req) }
s.sess.Save(id, sourceName, items)
```
`writeSSE` 改为读 `e.Data`（已是 json.RawMessage）：`fmt.Fprintf(w, "event: %s\ndata: %s\n\n", e.Type, e.Data)`。`collectOutput` 改为从 `req.Input.OfInputItemList` 收集 function_call/reasoning 变体为 `[]model.OutputItem`。

- [ ] **Step 2: 测试改写 + 通过 + commit**

Run: `go test ./internal/server/` → PASS。
```bash
git add internal/server/
git commit -m "refactor(server): ResponseNewParams 解码 + P2 回显透传 + 新事件接线"
```

---

### Task 8: 删除旧手写 model + 全量收口

**Files:**
- Delete: `internal/model/response.go`, `internal/model/anthropic.go`, 及其旧 `_test.go` 中已被 event_test.go 取代的部分
- Modify: 清理一切残留 `model.ResponseRequest`/`AnthropicRequest`/`AnthropicEvent`/`InputItem`(旧) 引用

**Interfaces:** —

- [ ] **Step 1: 删除旧文件**

```bash
git rm internal/model/response.go internal/model/anthropic.go
```
修复任何残留引用（grep `model.ResponseRequest\|model.AnthropicRequest\|model.AnthropicEvent\|model.InputItem\b`），全部替换为 SDK/新类型。

- [ ] **Step 2: 全量构建 + 测试**

```bash
go build ./...
go test ./...
```
Expected: 全绿。若有未覆盖的协议字段/事件，按 cheatsheet 补到对应包。

- [ ] **Step 3: smoke + commit**

```bash
go run ./cmd/server -config config.example.yaml &
# curl POST /v1/responses（dummy key 预期 response.failed，pipeline 跑通）
git add -A
git commit -m "refactor(model): 删除手写协议 model，SDK 迁移收口"
```

---

## Self-Review

**Spec 覆盖**：
- P0.1 事件名 → Task 5（constant 常量）✅
- P0.2/P0.3 object/output → Task 2 responseObject + Task 5 handleComplete ✅
- P1.4 in_progress → Task 5 ✅；P1.5 incomplete → Task 5 ✅；P1.6 output_text.done → Task 5 ✅；P1.7 content_part → Task 5 ✅；P1.8 reasoning summary 系列 → Task 5 ✅；P1.9 error vs failed → Task 5/7 ✅；P1.10 sequence_number → Task 2 所有 struct + Task 5 nextSeq ✅；P1.11 include → Task 3（ResponseNewParams 天然字段）✅
- P2.12 回显 → Task 2 responseObject + Task 7 echo ✅；P2.13 透传 → Task 3 ✅
- 分层（入站 SDK/出站自定义 struct/容灾保留）→ Task 3/4/5/6/7 ✅
- 删手写 model → Task 8 ✅

**Placeholder 扫描**：Task 3/5/6 中标注"按 cheatsheet/spike 套用 SDK 命名"的处——这是 Task 1 的真实产出物（已 commit 的 `docs/sdk-type-cheatsheet.md`），非逃避式 TODO。实现者必须先做 Task 1。

**类型一致性**：`OutputItem`（Task 2 定义）在 Task 5(conv 输出)、Task 6(store)、Task 7(server/collectOutput) 一致使用；`responseObject`/`ResponseObjectParams`（Task 2）在 Task 5(handleComplete)、Task 7(echo) 一致；`Feed(*anthropic.MessageStreamEventUnion)`（Task 5）被 Task 7 server 调用一致。

**范围**：单一迁移，8 任务各自可测可 commit。
