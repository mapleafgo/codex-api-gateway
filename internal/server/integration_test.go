package server

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mapleafgo/codex-api-gateway/internal/config"
)

// --- mock backend framework -------------------------------------------------

// mockResp describes one response from the mock backend.
// If status != 0 and != 200, the server returns that status code.
// Otherwise it streams the given SSE data lines (each is a "data: ..." payload
// without the "data: " prefix).
type mockResp struct {
	status int
	lines  []string // raw JSON payloads for "data:" lines
}

// mockBackend creates a programmable mock Anthropic backend.
// responses is indexed by request count; the last entry is reused for
// subsequent requests. Each response is streamed as SSE.
func mockBackend(responses []mockResp) (*httptest.Server, *atomic.Int64) {
	var calls atomic.Int64
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		idx := int(calls.Add(1)) - 1
		resp := mockResp{status: 500}
		if idx < len(responses) {
			resp = responses[idx]
		} else if len(responses) > 0 {
			resp = responses[len(responses)-1]
		}
		if resp.status != 0 && resp.status != 200 {
			w.WriteHeader(resp.status)
			io.WriteString(w, "mock error")
			return
		}
		w.Header().Set("content-type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		f := w.(http.Flusher)
		f.Flush()
		for _, line := range resp.lines {
			io.WriteString(w, "data: "+line+"\n\n")
			f.Flush()
		}
	})), &calls
}

// sseEvent holds a parsed SSE event from the gateway response.
type sseEvent struct {
	eventType string
	data      map[string]any
	rawData   string
}

// readSSE reads the full response body and parses all SSE events.
func readSSE(t *testing.T, body io.Reader) []sseEvent {
	t.Helper()
	var events []sseEvent
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var currentType string
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "event: ") {
			currentType = strings.TrimPrefix(line, "event: ")
		} else if strings.HasPrefix(line, "data: ") {
			payload := strings.TrimPrefix(line, "data: ")
			var m map[string]any
			if err := json.Unmarshal([]byte(payload), &m); err != nil {
				t.Fatalf("parse data line %q: %v", payload, err)
			}
			events = append(events, sseEvent{
				eventType: currentType,
				data:      m,
				rawData:   payload,
			})
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan: %v", err)
	}
	return events
}

// eventTypes extracts the ordered list of event types.
func eventTypes(events []sseEvent) []string {
	types := make([]string, len(events))
	for i, e := range events {
		types[i] = e.eventType
	}
	return types
}

// requireEvent checks that an event type appears at least once.
func requireEvent(t *testing.T, events []sseEvent, eventType string) {
	t.Helper()
	for _, e := range events {
		if e.eventType == eventType {
			return
		}
	}
	t.Fatalf("expected event %q in sequence: %v", eventType, eventTypes(events))
}

// requireNotEvent checks that an event type does NOT appear.
func requireNotEvent(t *testing.T, events []sseEvent, eventType string) {
	t.Helper()
	for _, e := range events {
		if e.eventType == eventType {
			t.Fatalf("event %q should NOT appear in sequence: %v", eventType, eventTypes(events))
		}
	}
}

// findEvent returns the first event of the given type, or fails.
func findEvent(t *testing.T, events []sseEvent, eventType string) sseEvent {
	t.Helper()
	for _, e := range events {
		if e.eventType == eventType {
			return e
		}
	}
	t.Fatalf("event %q not found in: %v", eventType, eventTypes(events))
	return sseEvent{}
}

// postResponses sends a POST to the gateway and returns parsed SSE events.
func postResponses(t *testing.T, ts *httptest.Server, body string) []sseEvent {
	t.Helper()
	resp, err := http.Post(ts.URL+"/v1/responses", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	return readSSE(t, resp.Body)
}

// verifySeqMonotonic checks that sequence_number values are strictly increasing.
func verifySeqMonotonic(t *testing.T, events []sseEvent) {
	t.Helper()
	var prev int64
	for _, e := range events {
		if e.data == nil {
			continue
		}
		sn, ok := e.data["sequence_number"].(float64)
		if !ok {
			continue
		}
		if int64(sn) <= prev {
			t.Fatalf("sequence_number not monotonic at event %q: %d <= %d", e.eventType, int64(sn), prev)
		}
		prev = int64(sn)
	}
}

// --- standard SSE payloads ---------------------------------------------------

const textStreamBody = `{"model":"gpt-5","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],"stream":true}`

func textStreamLines() []string {
	return []string{
		`{"type":"message_start","message":{"id":"m_text1","model":"claude-sonnet"}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hel"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"lo"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":" world"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"type":"message_delta","stop_reason":"end_turn"},"usage":{"input_tokens":10,"output_tokens":5}}`,
		`{"type":"message_stop"}`,
	}
}

// --- Step 2: full-pathway tests ---------------------------------------------

// TestIntegrationPlainTextStream verifies the complete text streaming pathway:
// mock sends message_start -> text block -> deltas -> stop -> message_delta ->
// message_stop. Asserts the full Response event sequence, monotonic sequence
// numbers, and completed response fields (object, output, completed_at, usage).
func TestIntegrationPlainTextStream(t *testing.T) {
	upstream, _ := mockBackend([]mockResp{{lines: textStreamLines()}})
	defer upstream.Close()

	cfg := &config.Config{
		Breaker: config.BreakerCfg{FirstByteTimeout: config.Duration(5 * time.Second)},
		Sources: []config.Source{{Name: "up", BaseURL: upstream.URL}},
	}
	srv := New(cfg)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	events := postResponses(t, ts, textStreamBody)

	// Verify the expected event sequence.
	expected := []string{
		"response.created",
		"response.in_progress",
		"response.output_item.added",
		"response.content_part.added",
		"response.output_text.delta",
		"response.output_text.delta",
		"response.output_text.delta",
		"response.output_text.done",
		"response.content_part.done",
		"response.output_item.done",
		"response.completed",
	}
	types := eventTypes(events)
	if len(types) != len(expected) {
		t.Fatalf("event count mismatch: got %d events %v, want %d %v", len(types), types, len(expected), expected)
	}
	for i, want := range expected {
		if types[i] != want {
			t.Fatalf("event[%d]: got %q, want %q\nfull: %v", i, types[i], want, types)
		}
	}

	// sequence_number must be strictly monotonic.
	verifySeqMonotonic(t, events)

	// response.completed must carry object, output, completed_at, usage.
	completed := findEvent(t, events, "response.completed")
	resp := completed.data["response"].(map[string]any)
	if resp["object"] != "response" {
		t.Fatalf("expected object=response, got %v", resp["object"])
	}
	if resp["status"] != "completed" {
		t.Fatalf("expected status=completed, got %v", resp["status"])
	}
	output, ok := resp["output"].([]any)
	if !ok {
		t.Fatalf("expected output array, got %T", resp["output"])
	}
	if len(output) != 1 {
		t.Fatalf("expected 1 output item, got %d", len(output))
	}
	if _, ok := resp["completed_at"]; !ok {
		t.Fatalf("expected completed_at field in response.completed")
	}
	usage, ok := resp["usage"].(map[string]any)
	if !ok {
		t.Fatalf("expected usage in response.completed")
	}
	if usage["input_tokens"].(float64) != 10 || usage["output_tokens"].(float64) != 5 {
		t.Fatalf("unexpected usage: %v", usage)
	}
	if usage["total_tokens"].(float64) != 15 {
		t.Fatalf("expected total_tokens=15, got %v", usage["total_tokens"])
	}

	// Verify text content in output_text.done has the full concatenated text.
	doneEv := findEvent(t, events, "response.output_text.done")
	if doneEv.data["text"] != "Hello world" {
		t.Fatalf("expected output_text.done text='Hello world', got %v", doneEv.data["text"])
	}
}

// TestIntegrationWebSearchRoundTrip verifies the full web search lifecycle:
// the gateway maps a Codex web_search tool onto Anthropic's native web search
// server tool on the way out, and translates the upstream server_tool_use +
// web_search_tool_result blocks back into a Responses web_search_call item on
// the way back.
func TestIntegrationWebSearchRoundTrip(t *testing.T) {
	var requestBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read upstream request: %v", err)
		}
		requestBody = string(body)
		w.Header().Set("content-type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		f := w.(http.Flusher)
		f.Flush()
		for _, line := range []string{
			`{"type":"message_start","message":{"id":"m_ws","model":"claude"}}`,
			`{"type":"content_block_start","index":0,"content_block":{"type":"server_tool_use","id":"toolu_ws1","name":"web_search","input":{"query":"golang tutorial"}}}`,
			`{"type":"content_block_stop","index":0}`,
			`{"type":"content_block_start","index":1,"content_block":{"type":"web_search_tool_result","tool_use_id":"toolu_ws1"}}`,
			`{"type":"content_block_stop","index":1}`,
			`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":5,"output_tokens":3}}`,
			`{"type":"message_stop"}`,
		} {
			io.WriteString(w, "data: "+line+"\n\n")
			f.Flush()
		}
	}))
	defer upstream.Close()

	cfg := &config.Config{
		Breaker: config.BreakerCfg{FirstByteTimeout: config.Duration(5 * time.Second)},
		Sources: []config.Source{{Name: "up", BaseURL: upstream.URL}},
	}
	srv := New(cfg)
	defer srv.Close()
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	events := postResponses(t, ts, `{"model":"gpt-5","input":"search the web","tools":[{"type":"web_search","filters":{"allowed_domains":["example.com"]}}],"stream":true}`)

	// Request side: filters.allowed_domains must reach the upstream as Anthropic's
	// native web search server tool.
	if !strings.Contains(requestBody, "allowed_domains") || !strings.Contains(requestBody, "example.com") {
		t.Fatalf("upstream request missing mapped web search tool: %s", requestBody)
	}

	// Response side: server_tool_use + web_search_tool_result surface as a
	// web_search_call item with the full status lifecycle.
	requireEvent(t, events, "response.web_search_call.in_progress")
	requireEvent(t, events, "response.web_search_call.searching")
	requireEvent(t, events, "response.web_search_call.completed")
	added := findEvent(t, events, "response.output_item.added")
	item := added.data["item"].(map[string]any)
	if item["type"] != "web_search_call" {
		t.Fatalf("expected web_search_call output item, got %v", item["type"])
	}
}

// TestIntegrationCacheControlReachesUpstream verifies the gateway actually
// emits cache_control to the upstream request body (system/tools/top-level),
// closing the prompt-caching loop end-to-end.
func TestIntegrationCacheControlReachesUpstream(t *testing.T) {
	var requestBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		requestBody = string(body)
		w.Header().Set("content-type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		f := w.(http.Flusher)
		f.Flush()
		for _, line := range textStreamLines() {
			io.WriteString(w, "data: "+line+"\n\n")
			f.Flush()
		}
	}))
	defer upstream.Close()

	cfg := &config.Config{
		Breaker: config.BreakerCfg{FirstByteTimeout: config.Duration(5 * time.Second)},
		Sources: []config.Source{{Name: "up", BaseURL: upstream.URL}},
	}
	srv := New(cfg)
	defer srv.Close()
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	postResponses(t, ts, `{"model":"gpt-5","instructions":"you are helpful","input":"hi","tools":[{"type":"function","name":"f","parameters":{"type":"object"}}],"stream":true}`)

	if !strings.Contains(requestBody, "cache_control") {
		t.Fatalf("upstream request missing cache_control: %s", requestBody)
	}
}

func TestIntegrationCustomToolStream(t *testing.T) {
	customToolLines := []string{
		`{"type":"message_start","message":{"id":"m_custom1","model":"claude"}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"call_raw","name":"raw_edit"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"input\":"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"\"patch text\"}"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"type":"message_delta","stop_reason":"tool_use"},"usage":{"input_tokens":5,"output_tokens":3}}`,
		`{"type":"message_stop"}`,
	}

	upstream, _ := mockBackend([]mockResp{{lines: customToolLines}})
	defer upstream.Close()

	cfg := &config.Config{
		Breaker: config.BreakerCfg{FirstByteTimeout: config.Duration(5 * time.Second)},
		Sources: []config.Source{{Name: "up", BaseURL: upstream.URL}},
	}
	srv := New(cfg)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	body := `{"model":"gpt-5","input":"hi","tools":[{"type":"custom","name":"raw_edit"}],"stream":true}`
	events := postResponses(t, ts, body)

	requireEvent(t, events, "response.custom_tool_call_input.delta")
	doneEv := findEvent(t, events, "response.custom_tool_call_input.done")
	if doneEv.data["input"] != "patch text" {
		t.Fatalf("expected custom input 'patch text', got %v", doneEv.data["input"])
	}
	requireNotEvent(t, events, "response.function_call_arguments.done")

	added := findEvent(t, events, "response.output_item.added")
	item := added.data["item"].(map[string]any)
	if item["type"] != "custom_tool_call" || item["name"] != "raw_edit" {
		t.Fatalf("expected custom_tool_call raw_edit item, got %+v", item)
	}
}

// TestIntegrationReasoningPlaintext verifies plaintext thinking with
// thinking_delta + signature_delta produces reasoning_summary_* events and
// stores the signature for round-trip.

// TestIntegrationApplyPatchNormalizesExtraStars 端到端：上游 tool_use(apply_patch)
// 产出带多余 *** 的 V4A 时，客户端收到的 custom_tool_call.input 首行必须是
// "*** Begin Patch"（Codex 校验字面量）。
func TestIntegrationApplyPatchNormalizesExtraStars(t *testing.T) {
	lines := []string{
		`{"type":"message_start","message":{"id":"m_ap1","model":"claude"}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"call_ap","name":"apply_patch"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"input\":\"*** Begin Patch ***\\n*** Update File: a.go\\n@@\\n-old\\n+new\\n*** End Patch ***\"}"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"type":"message_delta","stop_reason":"tool_use"},"usage":{"input_tokens":5,"output_tokens":3}}`,
		`{"type":"message_stop"}`,
	}
	upstream, _ := mockBackend([]mockResp{{lines: lines}})
	defer upstream.Close()
	cfg := &config.Config{
		Breaker: config.BreakerCfg{FirstByteTimeout: config.Duration(5 * time.Second)},
		Sources: []config.Source{{Name: "up", BaseURL: upstream.URL}},
	}
	srv := New(cfg)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	body := `{"model":"gpt-5","input":"edit","tools":[{"type":"apply_patch"}],"stream":true}`
	events := postResponses(t, ts, body)
	doneEv := findEvent(t, events, "response.custom_tool_call_input.done")
	input, _ := doneEv.data["input"].(string)
	if !strings.HasPrefix(input, "*** Begin Patch\n") {
		t.Fatalf("begin marker wrong: %q", input)
	}
	if strings.Contains(input, "Patch ***") {
		t.Fatalf("extra stars remain: %q", input)
	}
}

// TestIntegrationFunctionArgsCoerceIntegerFloats 端到端：上游 function 参数
// 含 85100.0 时，客户端 arguments 必须是整数字面量。
func TestIntegrationFunctionArgsCoerceIntegerFloats(t *testing.T) {
	lines := []string{
		`{"type":"message_start","message":{"id":"m_fn1","model":"claude"}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"call_fn","name":"write_stdin"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"session_id\":85100.0,\"yield_time_ms\":300000.0}"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"type":"message_delta","stop_reason":"tool_use"},"usage":{"input_tokens":5,"output_tokens":3}}`,
		`{"type":"message_stop"}`,
	}
	upstream, _ := mockBackend([]mockResp{{lines: lines}})
	defer upstream.Close()
	cfg := &config.Config{
		Breaker: config.BreakerCfg{FirstByteTimeout: config.Duration(5 * time.Second)},
		Sources: []config.Source{{Name: "up", BaseURL: upstream.URL}},
	}
	srv := New(cfg)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	body := `{"model":"gpt-5","input":"hi","tools":[{"type":"function","name":"write_stdin","parameters":{"type":"object"}}],"stream":true}`
	events := postResponses(t, ts, body)
	doneEv := findEvent(t, events, "response.function_call_arguments.done")
	args, _ := doneEv.data["arguments"].(string)
	if strings.Contains(args, ".0") {
		t.Fatalf("float ints remain: %s", args)
	}
	if !strings.Contains(args, "85100") || !strings.Contains(args, "300000") {
		t.Fatalf("ints missing: %s", args)
	}
}

func TestIntegrationReasoningPlaintext(t *testing.T) {
	lines := []string{
		`{"type":"message_start","message":{"id":"m_reason1","model":"claude"}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"thinking"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"Let me consider"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"Sig12345"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"content_block_start","index":1,"content_block":{"type":"text"}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"Answer"}}`,
		`{"type":"content_block_stop","index":1}`,
		`{"type":"message_delta","delta":{"type":"message_delta","stop_reason":"end_turn"},"usage":{"input_tokens":3,"output_tokens":2}}`,
		`{"type":"message_stop"}`,
	}
	upstream, _ := mockBackend([]mockResp{{lines: lines}})
	defer upstream.Close()

	cfg := &config.Config{
		Breaker: config.BreakerCfg{FirstByteTimeout: config.Duration(5 * time.Second)},
		Sources: []config.Source{{Name: "up", BaseURL: upstream.URL}},
	}
	srv := New(cfg)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	body := `{"model":"gpt-5","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],"reasoning":{"effort":"medium","summary":"concise"},"stream":true}`
	events := postResponses(t, ts, body)

	// summary=concise triggers summarized mode: reasoning_summary_* events are emitted.
	requireEvent(t, events, "response.reasoning_summary_text.delta")
	requireEvent(t, events, "response.reasoning_summary_text.done")
	requireNotEvent(t, events, "response.reasoning_text.delta")

	// Verify the reasoning text.
	reasonDone := findEvent(t, events, "response.reasoning_summary_text.done")
	if reasonDone.data["text"] != "Let me consider" {
		t.Fatalf("expected reasoning text 'Let me consider', got %v", reasonDone.data["text"])
	}

	// Verify signature is stored in the output item for round-trip.
	completed := findEvent(t, events, "response.completed")
	resp := completed.data["response"].(map[string]any)
	output := resp["output"].([]any)
	var foundSig bool
	for _, item := range output {
		m := item.(map[string]any)
		if m["type"] == "reasoning" {
			if m["signature"] == "Sig12345" {
				foundSig = true
			}
		}
	}
	if !foundSig {
		t.Fatalf("reasoning signature 'Sig12345' not stored in output: %+v", output)
	}
}

// TestIntegrationReasoningSummarized verifies that with reasoning.summary=concise,
// the converter emits reasoning_summary_* events instead of reasoning_text.*.
func TestIntegrationReasoningSummarized(t *testing.T) {
	lines := []string{
		`{"type":"message_start","message":{"id":"m_sum1","model":"claude"}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"thinking"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"Summarized thought"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"content_block_start","index":1,"content_block":{"type":"text"}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"Result"}}`,
		`{"type":"content_block_stop","index":1}`,
		`{"type":"message_delta","delta":{"type":"message_delta","stop_reason":"end_turn"},"usage":{"input_tokens":3,"output_tokens":2}}`,
		`{"type":"message_stop"}`,
	}
	upstream, _ := mockBackend([]mockResp{{lines: lines}})
	defer upstream.Close()

	cfg := &config.Config{
		Breaker: config.BreakerCfg{FirstByteTimeout: config.Duration(5 * time.Second)},
		Sources: []config.Source{{Name: "up", BaseURL: upstream.URL}},
	}
	srv := New(cfg)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	body := `{"model":"gpt-5","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],"reasoning":{"effort":"medium","summary":"concise"},"stream":true}`
	events := postResponses(t, ts, body)

	// Must emit reasoning_summary_* events.
	requireEvent(t, events, "response.reasoning_summary_part.added")
	requireEvent(t, events, "response.reasoning_summary_text.delta")
	requireEvent(t, events, "response.reasoning_summary_text.done")
	requireEvent(t, events, "response.reasoning_summary_part.done")

	// Must NOT emit reasoning_text.* events.
	requireNotEvent(t, events, "response.reasoning_text.delta")
	requireNotEvent(t, events, "response.reasoning_text.done")

	// Verify the summary text content.
	sumDone := findEvent(t, events, "response.reasoning_summary_text.done")
	if sumDone.data["text"] != "Summarized thought" {
		t.Fatalf("expected summary text 'Summarized thought', got %v", sumDone.data["text"])
	}
}

// TestIntegrationRedactedThinking verifies that redacted_thinking blocks
// store EncryptedContent for round-trip.
func TestIntegrationRedactedThinking(t *testing.T) {
	lines := []string{
		`{"type":"message_start","message":{"id":"m_redact1","model":"claude"}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"redacted_thinking","data":"ENCRYPTED_BLOB_123"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"content_block_start","index":1,"content_block":{"type":"text"}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"Done"}}`,
		`{"type":"content_block_stop","index":1}`,
		`{"type":"message_delta","delta":{"type":"message_delta","stop_reason":"end_turn"},"usage":{"input_tokens":3,"output_tokens":1}}`,
		`{"type":"message_stop"}`,
	}
	upstream, _ := mockBackend([]mockResp{{lines: lines}})
	defer upstream.Close()

	cfg := &config.Config{
		Breaker: config.BreakerCfg{FirstByteTimeout: config.Duration(5 * time.Second)},
		Sources: []config.Source{{Name: "up", BaseURL: upstream.URL}},
	}
	srv := New(cfg)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	events := postResponses(t, ts, textStreamBody)

	// Should complete normally.
	requireEvent(t, events, "response.completed")

	// Verify EncryptedContent is stored in output and session.
	completed := findEvent(t, events, "response.completed")
	resp := completed.data["response"].(map[string]any)
	output := resp["output"].([]any)
	var foundEncrypted bool
	for _, item := range output {
		m := item.(map[string]any)
		if m["type"] == "reasoning" {
			if m["encrypted_content"] == "ENCRYPTED_BLOB_123" {
				foundEncrypted = true
			}
		}
	}
	if !foundEncrypted {
		t.Fatalf("encrypted_content not stored: %+v", output)
	}
}

// TestIntegrationErrorToFailed verifies that a mid-stream error event produces
// exactly ONE response.failed event (not double-fired).
func TestIntegrationErrorToFailed(t *testing.T) {
	lines := []string{
		`{"type":"message_start","message":{"id":"m_err1","model":"claude"}}`,
		`{"type":"error","error":{"type":"overloaded_error","message":"Server is overloaded"}}`,
	}
	upstream, _ := mockBackend([]mockResp{{lines: lines}})
	defer upstream.Close()

	cfg := &config.Config{
		Breaker: config.BreakerCfg{FirstByteTimeout: config.Duration(5 * time.Second)},
		Sources: []config.Source{{Name: "up", BaseURL: upstream.URL}},
	}
	srv := New(cfg)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	events := postResponses(t, ts, textStreamBody)

	// Count response.failed events — must be exactly 1.
	count := 0
	for _, e := range events {
		if e.eventType == "response.failed" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("expected exactly 1 response.failed, got %d. events: %v", count, eventTypes(events))
	}

	// Must NOT have response.completed.
	requireNotEvent(t, events, "response.completed")

	// The failed event must carry error.message.
	failed := findEvent(t, events, "response.failed")
	resp := failed.data["response"].(map[string]any)
	errObj, ok := resp["error"].(map[string]any)
	if !ok || errObj["message"] != "Server is overloaded" {
		t.Fatalf("expected error.message='Server is overloaded', got: %v", resp["error"])
	}
}

// TestIntegrationNon200EmitsFailed verifies that a non-200 status (connection
// error before any events) produces a response.failed event.
func TestIntegrationNon200EmitsFailed(t *testing.T) {
	upstream, _ := mockBackend([]mockResp{{status: 500}})
	defer upstream.Close()

	cfg := &config.Config{
		Breaker: config.BreakerCfg{FirstByteTimeout: config.Duration(5 * time.Second)},
		Sources: []config.Source{{Name: "up", BaseURL: upstream.URL}},
	}
	srv := New(cfg)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	events := postResponses(t, ts, textStreamBody)

	requireEvent(t, events, "response.failed")
	requireNotEvent(t, events, "response.completed")
}

// --- Step 3: resilience tests -----------------------------------------------

// TestIntegrationFailover verifies that when source A returns 500 and source B
// succeeds, the gateway fails over to B and produces normal events.
func TestIntegrationFailover(t *testing.T) {
	badA, aCalls := mockBackend([]mockResp{{status: 500}})
	defer badA.Close()
	goodB, bCalls := mockBackend([]mockResp{{lines: textStreamLines()}})
	defer goodB.Close()

	cfg := &config.Config{
		Breaker: config.BreakerCfg{
			FirstByteTimeout: config.Duration(5 * time.Second),
			DegradeThreshold: 100, // high so A doesn't degrade in this test
			Cooldown:         config.Duration(time.Minute),
			HalfOpenProbes:   1,
		},
		Sources: []config.Source{
			{Name: "A", BaseURL: badA.URL},
			{Name: "B", BaseURL: goodB.URL},
		},
	}
	srv := New(cfg)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	events := postResponses(t, ts, textStreamBody)

	// Should succeed via B.
	requireEvent(t, events, "response.completed")
	requireNotEvent(t, events, "response.failed")

	// A was called once (failed), B was called once (succeeded).
	if got := aCalls.Load(); got != 1 {
		t.Fatalf("expected A called once, got %d", got)
	}
	if got := bCalls.Load(); got != 1 {
		t.Fatalf("expected B called once, got %d", got)
	}
}

func TestIntegrationFailoverUsesSuccessfulSourceID(t *testing.T) {
	badA, _ := mockBackend([]mockResp{{status: 500}})
	defer badA.Close()
	goodB, _ := mockBackend([]mockResp{{lines: []string{
		`{"type":"message_start","message":{"id":"m_actual_source","model":"claude-sonnet"}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"thinking"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"think"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"SigB"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"type":"message_delta","stop_reason":"end_turn"}}`,
		`{"type":"message_stop"}`,
	}}})
	defer goodB.Close()

	cfg := &config.Config{
		Breaker: config.BreakerCfg{
			FirstByteTimeout: config.Duration(5 * time.Second),
			DegradeThreshold: 100,
			Cooldown:         config.Duration(time.Minute),
			HalfOpenProbes:   1,
			MaxRetries:       0,
		},
		Sources: []config.Source{
			{Name: "A", BaseURL: badA.URL},
			{Name: "B", BaseURL: goodB.URL},
		},
	}
	srv := New(cfg)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	events := postResponses(t, ts, `{"model":"gpt-5","input":"hi","reasoning":{"effort":"medium"},"stream":true}`)
	requireEvent(t, events, "response.completed")
	// failover 后，最终 response.id 应来自成功源 B 返回的 message id。
	completed := findEvent(t, events, "response.completed")
	resp := completed.data["response"].(map[string]any)
	if resp["id"] != "m_actual_source" {
		t.Fatalf("response.id = %v, want m_actual_source from source B", resp["id"])
	}
}

func TestIntegrationServerSideFailureAdvancesSequenceNumber(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		f := w.(http.Flusher)
		f.Flush()
		io.WriteString(w, `data: {"type":"message_start","message":{"id":"m_reset","model":"claude"}}`+"\n\n")
		f.Flush()
		hj := w.(http.Hijacker)
		conn, _, err := hj.Hijack()
		if err != nil {
			return
		}
		_ = conn.Close()
	}))
	defer upstream.Close()

	cfg := &config.Config{
		Breaker: config.BreakerCfg{
			FirstByteTimeout: config.Duration(5 * time.Second),
			DegradeThreshold: 100,
			Cooldown:         config.Duration(time.Minute),
			HalfOpenProbes:   1,
			MaxRetries:       0,
		},
		Sources: []config.Source{{Name: "up", BaseURL: upstream.URL}},
	}
	srv := New(cfg)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	events := postResponses(t, ts, textStreamBody)
	requireEvent(t, events, "response.failed")
	var prev int64
	for _, e := range events {
		sn, ok := e.data["sequence_number"].(float64)
		if !ok {
			continue
		}
		if int64(sn) <= prev {
			t.Fatalf("sequence_number not monotonic at %s: %d <= %d; events=%v", e.eventType, int64(sn), prev, eventTypes(events))
		}
		prev = int64(sn)
	}
}

// TestIntegrationDegradeReorder verifies that after source A degrades (3
// consecutive failures), the runtime order moves A to the end. The next
// request should hit B first.
func TestIntegrationDegradeReorder(t *testing.T) {
	badA, aCalls := mockBackend([]mockResp{{status: 500}})
	defer badA.Close()
	goodB, bCalls := mockBackend([]mockResp{{lines: textStreamLines()}})
	defer goodB.Close()

	cfg := &config.Config{
		Breaker: config.BreakerCfg{
			FirstByteTimeout: config.Duration(5 * time.Second),
			DegradeThreshold: 3,
			RecoverThreshold: 1,
			Cooldown:         config.Duration(time.Minute),
			HalfOpenProbes:   1,
			MaxRetries:       0,
		},
		Sources: []config.Source{
			{Name: "A", BaseURL: badA.URL},
			{Name: "B", BaseURL: goodB.URL},
		},
	}
	srv := New(cfg)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// Send 3 requests: each tries A first (500), failovers to B (success).
	// After 3 failures on A, A degrades -> moved to end of runtimeOrder.
	for i := 0; i < 3; i++ {
		events := postResponses(t, ts, textStreamBody)
		requireEvent(t, events, "response.completed")
	}

	// A should have been called 3 times (one per request), B 3 times.
	if got := aCalls.Load(); got != 3 {
		t.Fatalf("expected A called 3 times, got %d", got)
	}
	if got := bCalls.Load(); got != 3 {
		t.Fatalf("expected B called 3 times, got %d", got)
	}

	// Reset counters for the verification request.
	aCalls.Store(0)
	bCalls.Store(0)

	// 4th request: A is now degraded and moved to end, so B should be tried first.
	events := postResponses(t, ts, textStreamBody)
	requireEvent(t, events, "response.completed")

	// B should be called (and A should NOT be called at all since B succeeds).
	if got := bCalls.Load(); got != 1 {
		t.Fatalf("after degrade, expected B called once, got %d", got)
	}
	if got := aCalls.Load(); got != 0 {
		t.Fatalf("after degrade, A should not be tried when B succeeds first, got %d", got)
	}
}

// TestIntegrationRetry verifies that with max_retries=1, the gateway retries
// the entire round after all sources fail. Uses a single source that always
// returns 500; with max_retries=1, total calls = 2 (initial + 1 retry).
// The default backoff (2s) means this test takes ~2 seconds.
func TestIntegrationRetry(t *testing.T) {
	bad, calls := mockBackend([]mockResp{{status: 500}})
	defer bad.Close()

	cfg := &config.Config{
		Breaker: config.BreakerCfg{
			FirstByteTimeout: config.Duration(5 * time.Second),
			DegradeThreshold: 100, // high so it doesn't circuit-open
			Cooldown:         config.Duration(time.Minute),
			HalfOpenProbes:   1,
			MaxRetries:       1, // initial + 1 retry = 2 rounds
		},
		Sources: []config.Source{{Name: "bad", BaseURL: bad.URL}},
	}
	srv := New(cfg)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	events := postResponses(t, ts, textStreamBody)

	// Should fail with response.failed.
	requireEvent(t, events, "response.failed")
	requireNotEvent(t, events, "response.completed")

	// 1 source * 2 rounds (initial + 1 retry) = 2 calls.
	if got := calls.Load(); got != 2 {
		t.Fatalf("expected 2 total calls (initial + 1 retry), got %d", got)
	}
}

// TestIntegrationRetryMaxZero verifies that max_retries=0 means no retry:
// the gateway makes exactly 1 call and fails.
func TestIntegrationRetryMaxZero(t *testing.T) {
	bad, calls := mockBackend([]mockResp{{status: 500}})
	defer bad.Close()

	cfg := &config.Config{
		Breaker: config.BreakerCfg{
			FirstByteTimeout: config.Duration(5 * time.Second),
			DegradeThreshold: 100,
			Cooldown:         config.Duration(time.Minute),
			HalfOpenProbes:   1,
			MaxRetries:       0, // no retry
		},
		Sources: []config.Source{{Name: "bad", BaseURL: bad.URL}},
	}
	srv := New(cfg)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	events := postResponses(t, ts, textStreamBody)
	requireEvent(t, events, "response.failed")

	if got := calls.Load(); got != 1 {
		t.Fatalf("expected exactly 1 call (no retry), got %d", got)
	}
}

// TestIntegrationCircuitOpenRecovery verifies the full circuit breaker cycle
// end-to-end through the HTTP gateway:
//  1. Sources A and B both fail initially. A reaches degraded then circuitOpen
//     across two requests (DegradeThreshold=1).
//  2. Once A is circuitOpen, only B is attempted.
//  3. After cooldown, A transitions to halfOpen on the next request.
//  4. If the halfOpen probe succeeds (A flips to healthy), A recovers to normal
//     and is restored to its original priority position.
//  5. After recovery, A is tried first again on subsequent requests.
//
// All assertions use observable behavior (upstream call counts + SSE events),
// not internal breaker/scheduler state.
func TestIntegrationCircuitOpenRecovery(t *testing.T) {
	// Source A: programmable — fails during phase 0, succeeds during phase 1.
	var aPhase atomic.Int32 // 0=fail, 1=succeed
	var aCalls atomic.Int64
	upstreamA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		aCalls.Add(1)
		if aPhase.Load() == 0 {
			w.WriteHeader(http.StatusInternalServerError)
			io.WriteString(w, "A is down")
			return
		}
		writeGoodSSE(w)
	}))
	defer upstreamA.Close()

	// Source B: also fails on first call, then succeeds.
	var bFailCount atomic.Int64
	var bCalls atomic.Int64
	upstreamB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bCalls.Add(1)
		if bFailCount.Add(-1) >= 0 {
			w.WriteHeader(http.StatusInternalServerError)
			io.WriteString(w, "B is down")
			return
		}
		writeGoodSSE(w)
	}))
	defer upstreamB.Close()

	cfg := &config.Config{
		Breaker: config.BreakerCfg{
			FirstByteTimeout: config.Duration(5 * time.Second),
			DegradeThreshold: 1, // 1 failure -> degraded, 2nd -> circuitOpen
			RecoverThreshold: 1,
			Cooldown:         config.Duration(500 * time.Millisecond), // generous: avoids CI-load timing flakes
			HalfOpenProbes:   1,
			MaxRetries:       0,
		},
		Sources: []config.Source{
			{Name: "A", BaseURL: upstreamA.URL},
			{Name: "B", BaseURL: upstreamB.URL},
		},
	}
	srv := New(cfg)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// Phase 0: Drive A to circuitOpen.
	// B is set to fail exactly once (first call) so that request 1 hits both
	// A and B (both fail). On request 2, A fails again (reaching circuitOpen)
	// and B succeeds (becoming the failover target).
	bFailCount.Store(1) // B fails on its 1st call only

	// Request 1: runtimeSeq=[A, B]. A fails -> degraded -> moveToEnd.
	// B fails -> degraded -> moveToEnd. Both fail -> response.failed.
	events1 := postResponses(t, ts, textStreamBody)
	requireEvent(t, events1, "response.failed")

	// After request 1: A degraded (degradeCount=1), B degraded (degradeCount=1).
	// Order: both moved to end and back — net [A(0), B(1)] (moveToEnd(A)→[B,A],
	// then moveToEnd(B)→[A,B]).

	// Request 2: runtimeSeq=[A, B]. A fails (call #2) -> circuitOpen.
	// B now succeeds (bFailCount exhausted) -> B's breaker recovers to normal.
	events2 := postResponses(t, ts, textStreamBody)
	requireEvent(t, events2, "response.completed")

	// After request 2: A is circuitOpen, B is normal.
	// A was called 2 times total (both failed).
	if got := aCalls.Load(); got != 2 {
		t.Fatalf("phase 0: expected A called 2 times, got %d", got)
	}

	// Reset counters.
	aCalls.Store(0)
	bCalls.Store(0)

	// Request 3: A is circuitOpen (cooldown=500ms, not yet elapsed) -> skipped.
	// B serves the request.
	events3 := postResponses(t, ts, textStreamBody)
	requireEvent(t, events3, "response.completed")
	if got := aCalls.Load(); got != 0 {
		t.Fatalf("circuitOpen A should NOT be called, got %d", got)
	}
	if got := bCalls.Load(); got != 1 {
		t.Fatalf("B should serve request 3, got %d calls", got)
	}

	// Phase 1: Flip A to healthy and wait for cooldown.
	aPhase.Store(1)
	time.Sleep(600 * time.Millisecond) // > Cooldown (500ms)

	// Reset counters.
	aCalls.Store(0)
	bCalls.Store(0)

	// Request 4: A's cooldown elapsed -> Allow() transitions to halfOpen.
	// A is probed and succeeds -> A recovers to normal, restored to position 0.
	events4 := postResponses(t, ts, textStreamBody)
	requireEvent(t, events4, "response.completed")

	// A should have been called at least once (the halfOpen probe).
	if got := aCalls.Load(); got < 1 {
		t.Fatalf("after cooldown, A should be probed via halfOpen, got %d calls", got)
	}

	// Reset counters.
	aCalls.Store(0)
	bCalls.Store(0)

	// Request 5: A is now normal and first in runtimeOrder (restored).
	// A should be tried first and succeed; B should NOT be called.
	events5 := postResponses(t, ts, textStreamBody)
	requireEvent(t, events5, "response.completed")
	if got := aCalls.Load(); got != 1 {
		t.Fatalf("after recovery, A should be called first, got %d", got)
	}
	if got := bCalls.Load(); got != 0 {
		t.Fatalf("after recovery, B should NOT be called when A succeeds, got %d", got)
	}
}

// TestIntegrationCircuitOpenSourceSkipped verifies that after A reaches
// circuitOpen through real HTTP traffic (with B also initially failing so A
// gets called on both requests), subsequent requests skip A entirely because
// Allow() returns false while circuitOpen and cooldown hasn't elapsed.
func TestIntegrationCircuitOpenSourceSkipped(t *testing.T) {
	var aCalls atomic.Int64
	upstreamA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		aCalls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
		io.WriteString(w, "A is down")
	}))
	defer upstreamA.Close()

	// B: fails on first call, succeeds afterwards (so request 2 gets a
	// completed response despite A circuit-opening).
	var bFailCount atomic.Int64
	var bCalls atomic.Int64
	upstreamB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bCalls.Add(1)
		if bFailCount.Add(-1) >= 0 {
			w.WriteHeader(http.StatusInternalServerError)
			io.WriteString(w, "B is down")
			return
		}
		writeGoodSSE(w)
	}))
	defer upstreamB.Close()

	cfg := &config.Config{
		Breaker: config.BreakerCfg{
			FirstByteTimeout: config.Duration(5 * time.Second),
			DegradeThreshold: 1, // 1 failure -> degraded, 2nd -> circuitOpen
			RecoverThreshold: 1,
			Cooldown:         config.Duration(time.Minute), // long cooldown: A stays open
			HalfOpenProbes:   1,
			MaxRetries:       0,
		},
		Sources: []config.Source{
			{Name: "A", BaseURL: upstreamA.URL},
			{Name: "B", BaseURL: upstreamB.URL},
		},
	}
	srv := New(cfg)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	bFailCount.Store(1) // B fails on 1st call only

	// Request 1: [A, B]. A fails -> degraded. B fails -> degraded. Both fail.
	events1 := postResponses(t, ts, textStreamBody)
	requireEvent(t, events1, "response.failed")

	// Request 2: [A, B]. A fails -> circuitOpen. B succeeds.
	events2 := postResponses(t, ts, textStreamBody)
	requireEvent(t, events2, "response.completed")

	// A was called 2 times (both failed) to reach circuitOpen.
	if got := aCalls.Load(); got != 2 {
		t.Fatalf("expected A called 2 times to reach circuitOpen, got %d", got)
	}

	// Reset counters.
	aCalls.Store(0)
	bCalls.Store(0)

	// Request 3: A is circuitOpen (cooldown=1min) -> Allow()=false -> skipped.
	// B serves the request.
	events3 := postResponses(t, ts, textStreamBody)
	requireEvent(t, events3, "response.completed")

	if got := aCalls.Load(); got != 0 {
		t.Fatalf("circuitOpen A should NOT be called, got %d", got)
	}
	if got := bCalls.Load(); got != 1 {
		t.Fatalf("B should serve, got %d calls", got)
	}
}

// writeGoodSSE writes a valid complete Anthropic SSE stream to the writer.
func writeGoodSSE(w http.ResponseWriter) {
	w.Header().Set("content-type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	f := w.(http.Flusher)
	f.Flush()
	for _, line := range textStreamLines() {
		io.WriteString(w, "data: "+line+"\n\n")
		f.Flush()
	}
}
