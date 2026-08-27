package server

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mapleafgo/codex-api-gateway/internal/config"
)

// TestAppServerSubAgentHistoryKeepsAgentMessage 端到端验证 Codex 0.147 多 agent
// 历史经 a 路径回灌后的归属。测试只在显式设置 APP_SERVER_E2E=1 时运行。
func TestAppServerSubAgentHistoryKeepsAgentMessage(t *testing.T) {
	if os.Getenv("APP_SERVER_E2E") != "1" {
		t.Skip("set APP_SERVER_E2E=1 to run the real codex app-server e2e test")
	}
	if _, err := exec.LookPath("codex"); err != nil {
		t.Fatalf("codex executable not found: %v", err)
	}

	var requestsMu sync.Mutex
	upstreamRequests := make([]string, 0, 8)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		requestsMu.Lock()
		upstreamRequests = append(upstreamRequests, string(body))
		requestsMu.Unlock()
		w.Header().Set("content-type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		flusher.Flush()
		writeAnthropicSSE(w, flusher, anthropicReplyFor(string(body)))
	}))
	defer upstream.Close()

	cfg := &config.Config{
		Logging: config.LoggingCfg{Level: "error"},
		Breaker: config.BreakerCfg{FirstByteTimeout: config.Duration(10 * time.Second)},
		Sources: []config.Source{{
			Name:         "appserver-e2e",
			BaseURL:      upstream.URL,
			Backend:      "anthropic",
			DefaultModel: "mock-claude",
		}},
		ModelOverrides: map[string]config.ModelOverride{
			"mock-model": {ContextWindow: int64Ptr(200000)},
		},
		ModelSlugOrder: []string{"mock-model"},
	}
	srv := newSrv(cfg)
	defer srv.Close()
	gateway := httptest.NewServer(srv.Handler())
	defer gateway.Close()

	appServer := startAppServer(t, gateway.URL)
	_, parentAnswers := appServer.runSubAgentProbe(t, "PARENT_QUESTION_A: spawn exactly one child, wait for it, then summarize.")

	requestsMu.Lock()
	bodies := append([]string(nil), upstreamRequests...)
	requestsMu.Unlock()
	var parentFinal string
	for _, body := range bodies {
		if strings.Contains(body, "PARENT_QUESTION_A") &&
			strings.Contains(body, "Wait completed.") {
			parentFinal = body
		}
	}
	if parentFinal == "" {
		t.Fatalf("parent final upstream request not captured; requests=%d", len(bodies))
	}

	var payload struct {
		Messages []struct {
			Role    string `json:"role"`
			Content []struct {
				Type      string          `json:"type"`
				Text      string          `json:"text"`
				Content   json.RawMessage `json:"content"`
				ToolUseID string          `json:"tool_use_id"`
				Name      string          `json:"name"`
				Input     any             `json:"input"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(parentFinal), &payload); err != nil {
		t.Fatalf("decode parent final Anthropic request: %v\n%s", err, parentFinal)
	}

	waitResultIndex := -1
	childAnswerIndex := -1
	for i, message := range payload.Messages {
		for _, block := range message.Content {
			if block.Type == "tool_result" && strings.Contains(string(block.Content), "Wait completed.") {
				waitResultIndex = i
			}
			if block.Type == "text" && strings.Contains(block.Text, "Message Type: FINAL_ANSWER") && strings.Contains(block.Text, "CHILD_FINAL_A") {
				childAnswerIndex = i
			}
		}
	}
	if waitResultIndex < 0 {
		t.Fatalf("wait_agent tool_result missing from parent history:\n%s", parentFinal)
	}
	if childAnswerIndex < 0 {
		t.Fatalf("child FINAL_ANSWER must remain an assistant message, but was merged or dropped:\n%s", parentFinal)
	}
	if childAnswerIndex < waitResultIndex {
		t.Fatalf("child FINAL_ANSWER must follow wait result; wait=%d child=%d", waitResultIndex, childAnswerIndex)
	}
	if payload.Messages[childAnswerIndex].Role != "assistant" {
		t.Fatalf("child FINAL_ANSWER role = %q, want assistant", payload.Messages[childAnswerIndex].Role)
	}
	if !containsString(parentAnswers, "PARENT_FINAL_A") {
		t.Fatalf("parent final answer missing from app-server items: %v", parentAnswers)
	}
}

// TestAppServerResponsesBackendDeliversAgentMessage 复现用户生产的 r 路径：
// Codex 子 agent 初始 NEW_TASK 是 plaintext agent_message，上游不认识该扩展时，
// 网关必须在 outbound Responses body 中按原位置折成 assistant message。
func TestAppServerResponsesBackendDeliversAgentMessage(t *testing.T) {
	if os.Getenv("APP_SERVER_E2E") != "1" {
		t.Skip("set APP_SERVER_E2E=1 to run the real codex app-server e2e test")
	}
	if _, err := exec.LookPath("codex"); err != nil {
		t.Fatalf("codex executable not found: %v", err)
	}

	var requestsMu sync.Mutex
	upstreamRequests := make([]string, 0, 8)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		requestsMu.Lock()
		upstreamRequests = append(upstreamRequests, string(body))
		requestsMu.Unlock()
		w.Header().Set("content-type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		flusher.Flush()
		writeResponsesSSE(w, flusher, responsesReplyFor(string(body)))
	}))
	defer upstream.Close()

	cfg := &config.Config{
		Logging: config.LoggingCfg{Level: "error"},
		Breaker: config.BreakerCfg{FirstByteTimeout: config.Duration(10 * time.Second)},
		Sources: []config.Source{{
			Name:    "responses-e2e",
			BaseURL: upstream.URL,
			Backend: "openai-responses",
		}},
		ModelOverrides: map[string]config.ModelOverride{
			"mock-model": {ContextWindow: int64Ptr(200000)},
		},
		ModelSlugOrder: []string{"mock-model"},
	}
	srv := newSrv(cfg)
	defer srv.Close()
	gateway := httptest.NewServer(srv.Handler())
	defer gateway.Close()

	appServer := startAppServer(t, gateway.URL)
	_, parentAnswers := appServer.runSubAgentProbe(t, "PARENT_QUESTION_R: spawn exactly one child, wait for it, then summarize.")
	if !containsString(parentAnswers, "PARENT_FINAL_R") {
		t.Fatalf("parent final answer missing from app-server items: %v", parentAnswers)
	}

	requestsMu.Lock()
	bodies := append([]string(nil), upstreamRequests...)
	requestsMu.Unlock()
	for _, body := range bodies {
		if strings.Contains(body, `"type":"agent_message"`) {
			t.Fatalf("plaintext agent_message leaked to Responses upstream: %s", body)
		}
	}

	var childTask string
	for _, body := range bodies {
		if strings.Contains(body, "PARENT_TASK_R") && strings.Contains(body, `"role":"assistant"`) {
			childTask = body
			break
		}
	}
	if childTask == "" {
		t.Fatalf("child task was not delivered as assistant message; requests=%d", len(bodies))
	}

	var parentFinal string
	for _, body := range bodies {
		if strings.Contains(body, "Wait completed.") && strings.Contains(body, "CHILD_FINAL_R") {
			parentFinal = body
			break
		}
	}
	if parentFinal == "" {
		t.Fatalf("parent final request missing; requests=%d", len(bodies))
	}
	var payload struct {
		Input []struct {
			Type    string `json:"type"`
			Role    string `json:"role"`
			CallID  string `json:"call_id"`
			Output  any    `json:"output"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"input"`
	}
	if err := json.Unmarshal([]byte(parentFinal), &payload); err != nil {
		t.Fatalf("decode parent final Responses request: %v\n%s", err, parentFinal)
	}
	waitIndex := -1
	childIndex := -1
	for i, item := range payload.Input {
		if item.Type == "function_call_output" && strings.Contains(fmt.Sprint(item.Output), "Wait completed.") {
			waitIndex = i
		}
		if item.Type == "message" && item.Role == "assistant" {
			for _, part := range item.Content {
				if strings.Contains(part.Text, "CHILD_FINAL_R") {
					childIndex = i
				}
			}
		}
	}
	if waitIndex < 0 || childIndex < 0 || childIndex < waitIndex {
		t.Fatalf("parent final order wrong: wait=%d child=%d payload=%s", waitIndex, childIndex, parentFinal)
	}
}

// TestAppServerAnthropicBackendPreservesFollowUpQuestion 验证 a 路径与 c 路径
// 在 agent_message 修复后共享相同行为：父会话 follow-up 仍以用户消息结尾。
func TestAppServerAnthropicBackendPreservesFollowUpQuestion(t *testing.T) {
	if os.Getenv("APP_SERVER_E2E") != "1" {
		t.Skip("set APP_SERVER_E2E=1 to run the real codex app-server e2e test")
	}
	if _, err := exec.LookPath("codex"); err != nil {
		t.Fatalf("codex executable not found: %v", err)
	}

	var requestsMu sync.Mutex
	upstreamRequests := make([]string, 0, 8)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		requestsMu.Lock()
		upstreamRequests = append(upstreamRequests, string(body))
		requestsMu.Unlock()
		w.Header().Set("content-type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		flusher.Flush()
		writeAnthropicSSE(w, flusher, anthropicReplyFor(string(body)))
	}))
	defer upstream.Close()

	cfg := &config.Config{
		Logging: config.LoggingCfg{Level: "error"},
		Breaker: config.BreakerCfg{FirstByteTimeout: config.Duration(10 * time.Second)},
		Sources: []config.Source{{
			Name:         "appserver-followup-e2e",
			BaseURL:      upstream.URL,
			Backend:      "anthropic",
			DefaultModel: "mock-claude",
		}},
		ModelOverrides: map[string]config.ModelOverride{
			"mock-model": {ContextWindow: int64Ptr(200000)},
		},
		ModelSlugOrder: []string{"mock-model"},
	}
	srv := newSrv(cfg)
	defer srv.Close()
	gateway := httptest.NewServer(srv.Handler())
	defer gateway.Close()

	appServer := startAppServer(t, gateway.URL)
	_, parentAnswers := appServer.runSubAgentProbe(t, "PARENT_QUESTION_A: spawn exactly one child, wait for it, then summarize.")
	if !containsString(parentAnswers, "PARENT_FINAL_A") {
		t.Fatalf("parent final answer missing from app-server items: %v", parentAnswers)
	}
	_, followAnswers := appServer.runTurn(t, 4, "FOLLOWUP_A: answer with FOLLOWUP_ANSWER_A only.")
	if !containsString(followAnswers, "FOLLOWUP_ANSWER_A") {
		t.Fatalf("follow-up answer missing from app-server items: %v", followAnswers)
	}

	requestsMu.Lock()
	bodies := make([]string, 0, len(upstreamRequests))
	bodies = append(bodies, upstreamRequests...)
	requestsMu.Unlock()
	var followUp string
	for _, body := range bodies {
		if strings.Contains(body, "FOLLOWUP_A") && strings.Contains(body, "Wait completed.") {
			followUp = body
			break
		}
	}
	if followUp == "" {
		t.Fatalf("follow-up Anthropic request not captured; requests=%d", len(bodies))
	}

	var payload struct {
		Messages []struct {
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(followUp), &payload); err != nil {
		t.Fatalf("decode follow-up Anthropic request: %v\n%s", err, followUp)
	}
	if len(payload.Messages) == 0 {
		t.Fatalf("follow-up Anthropic request has no messages:\n%s", followUp)
	}
	last := payload.Messages[len(payload.Messages)-1]
	lastText := ""
	for _, block := range last.Content {
		if block.Type == "text" {
			lastText += block.Text
		}
	}
	if last.Role != "user" || !strings.Contains(lastText, "FOLLOWUP_A") {
		t.Fatalf("follow-up Anthropic request must end with the new user message; got role=%s text=%s\n%s",
			last.Role, lastText, followUp)
	}
}

// TestAppServerChatBackendPreservesFollowUpQuestion 复现用户生产的 c 路径：
// 子 agent 完成后，父会话继续提问时，Chat 出站历史必须仍以该用户问题结尾，
// 且子 agent FINAL_ANSWER 不能被折叠成工具输出或用户消息。
func TestAppServerChatBackendPreservesFollowUpQuestion(t *testing.T) {
	if os.Getenv("APP_SERVER_E2E") != "1" {
		t.Skip("set APP_SERVER_E2E=1 to run the real codex app-server e2e test")
	}
	if _, err := exec.LookPath("codex"); err != nil {
		t.Fatalf("codex executable not found: %v", err)
	}

	var requestsMu sync.Mutex
	upstreamRequests := make([][]byte, 0, 8)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		requestsMu.Lock()
		upstreamRequests = append(upstreamRequests, body)
		requestsMu.Unlock()
		w.Header().Set("content-type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		flusher.Flush()
		writeChatSSE(w, flusher, chatReplyFor(body))
	}))
	defer upstream.Close()

	cfg := &config.Config{
		Logging: config.LoggingCfg{Level: "error"},
		Breaker: config.BreakerCfg{FirstByteTimeout: config.Duration(10 * time.Second)},
		Sources: []config.Source{{
			Name:    "chat-e2e",
			BaseURL: upstream.URL,
			Backend: "openai-chat",
		}},
		ModelOverrides: map[string]config.ModelOverride{
			"mock-model": {ContextWindow: int64Ptr(200000)},
		},
		ModelSlugOrder: []string{"mock-model"},
	}
	srv := newSrv(cfg)
	defer srv.Close()
	gateway := httptest.NewServer(srv.Handler())
	defer gateway.Close()

	appServer := startAppServer(t, gateway.URL)
	_, parentAnswers := appServer.runSubAgentProbe(t, "PARENT_QUESTION_C: spawn exactly one child, wait for it, then summarize.")
	if !containsString(parentAnswers, "PARENT_FINAL_C") {
		t.Fatalf("parent final answer missing from app-server items: %v", parentAnswers)
	}
	notifications, answers := appServer.runTurn(t, 4, "FOLLOWUP_C: answer with FOLLOWUP_ANSWER_C only.")
	if !containsString(answers, "FOLLOWUP_ANSWER_C") {
		t.Fatalf("follow-up answer missing from app-server items: %v\nnotifications:\n%s",
			answers, strings.Join(notifications, "\n"))
	}

	requestsMu.Lock()
	bodies := make([][]byte, 0, len(upstreamRequests))
	bodies = append(bodies, upstreamRequests...)
	requestsMu.Unlock()
	var followUp []byte
	for _, body := range bodies {
		if strings.Contains(string(body), "FOLLOWUP_C") {
			followUp = body
			break
		}
	}
	if followUp == nil {
		t.Fatalf("follow-up upstream request not captured; requests=%d", len(bodies))
	}

	var payload struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(followUp, &payload); err != nil {
		t.Fatalf("decode follow-up Chat request: %v\n%s", err, followUp)
	}
	if len(payload.Messages) == 0 {
		t.Fatalf("follow-up Chat request has no messages:\n%s", followUp)
	}
	last := payload.Messages[len(payload.Messages)-1]
	if last.Role != "user" || !strings.Contains(string(last.Content), "FOLLOWUP_C") {
		t.Fatalf("follow-up Chat request must end with the new user message; got role=%s content=%s\n%s",
			last.Role, last.Content, followUp)
	}
}

type appServerProcess struct {
	cmd      *exec.Cmd
	stdin    io.WriteCloser
	lines    chan string
	stderr   lockedBuffer
	threadID string
}

func startAppServer(t *testing.T, gatewayURL string) *appServerProcess {
	t.Helper()
	codexHome := t.TempDir()
	modelsResp, err := http.Get(gatewayURL + "/v1/models")
	if err != nil {
		t.Fatalf("fetch gateway model catalog: %v", err)
	}
	modelsBody, err := io.ReadAll(modelsResp.Body)
	_ = modelsResp.Body.Close()
	if err != nil {
		t.Fatalf("read gateway model catalog: %v", err)
	}
	modelsPath := filepath.Join(codexHome, "models.json")
	if err := os.WriteFile(modelsPath, modelsBody, 0o600); err != nil {
		t.Fatalf("write gateway model catalog: %v", err)
	}
	configTOML := fmt.Sprintf(`
model = "mock-model"
approval_policy = "never"
sandbox_mode = "read-only"
model_provider = "gateway"
model_catalog_json = %q

[features.multi_agent_v2]
enabled = true
min_wait_timeout_ms = 100
max_wait_timeout_ms = 1000
default_wait_timeout_ms = 100

[model_providers.gateway]
name = "Gateway E2E"
base_url = %q
wire_api = "responses"
requires_openai_auth = false
request_max_retries = 0
stream_max_retries = 0
`, modelsPath, gatewayURL+"/v1")
	if err := os.WriteFile(filepath.Join(codexHome, "config.toml"), []byte(configTOML), 0o600); err != nil {
		t.Fatalf("write codex config: %v", err)
	}

	cmd := exec.Command("codex", "app-server", "--stdio")
	cmd.Env = append(os.Environ(), "CODEX_HOME="+codexHome, "RUST_LOG=error")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	p := &appServerProcess{cmd: cmd, stdin: stdin, lines: make(chan string, 256)}
	cmd.Stderr = &p.stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start app-server: %v", err)
	}
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		if p.stderr.Len() > 0 {
			t.Logf("app-server stderr:\n%s", p.stderr.String())
		}
	})
	go func() {
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
		for scanner.Scan() {
			p.lines <- scanner.Text()
		}
	}()

	sendJSONLine(t, stdin, map[string]any{
		"id":     1,
		"method": "initialize",
		"params": map[string]any{
			"clientInfo": map[string]any{"name": "gateway-e2e", "version": "0"},
		},
	})
	waitResponse(t, p.lines, 1)
	sendJSONLine(t, stdin, map[string]any{"method": "initialized", "params": map[string]any{}})
	sendJSONLine(t, stdin, map[string]any{
		"id":     2,
		"method": "thread/start",
		"params": map[string]any{"model": "mock-model", "cwd": t.TempDir()},
	})
	threadResponse := waitResponse(t, p.lines, 2)
	p.threadID, _ = threadResponse["thread"].(map[string]any)["id"].(string)
	if p.threadID == "" {
		t.Fatalf("thread id missing from response: %v", threadResponse)
	}
	return p
}

func (p *appServerProcess) runSubAgentProbe(t *testing.T, question string) ([]string, []string) {
	t.Helper()
	return p.runTurn(t, 3, question)
}

func (p *appServerProcess) runTurn(t *testing.T, id int, question string) ([]string, []string) {
	t.Helper()
	sendJSONLine(t, p.stdin, map[string]any{
		"id":     id,
		"method": "turn/start",
		"params": map[string]any{
			"threadId": p.threadID,
			"input": []any{map[string]any{
				"type": "text",
				"text": question,
			}},
		},
	})

	var parentCompleted bool
	notifications := make([]string, 0, 128)
	parentAnswers := make([]string, 0, 4)
	for !parentCompleted {
		select {
		case line, ok := <-p.lines:
			if !ok {
				t.Fatalf("app-server stdout closed before parent turn completed")
			}
			var msg struct {
				Method string         `json:"method"`
				Params map[string]any `json:"params"`
			}
			if err := json.Unmarshal([]byte(line), &msg); err != nil {
				t.Fatalf("decode app-server notification %q: %v", line, err)
			}
			notifications = append(notifications, line)
			if msg.Method == "item/completed" {
				if item, _ := msg.Params["item"].(map[string]any); item["type"] == "agentMessage" {
					if text, _ := item["text"].(string); text != "" {
						parentAnswers = append(parentAnswers, text)
					}
				}
			}
			if msg.Method == "turn/completed" && msg.Params["threadId"] == p.threadID {
				parentCompleted = true
			}
		case <-time.After(30 * time.Second):
			t.Fatalf("timed out waiting for parent turn completion; notifications:\n%s\nstderr:\n%s",
				strings.Join(notifications, "\n"), p.stderr.String())
		}
	}
	return notifications, parentAnswers
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Len()
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func waitResponse(t *testing.T, lines <-chan string, id int) map[string]any {
	t.Helper()
	for {
		select {
		case line, ok := <-lines:
			if !ok {
				t.Fatalf("app-server stdout closed while waiting for response %d", id)
			}
			var msg struct {
				ID     int            `json:"id"`
				Result map[string]any `json:"result"`
				Error  map[string]any `json:"error"`
			}
			if err := json.Unmarshal([]byte(line), &msg); err != nil {
				t.Fatalf("decode app-server response %q: %v", line, err)
			}
			if msg.ID != id {
				continue
			}
			if msg.Error != nil {
				t.Fatalf("app-server response %d failed: %v", id, msg.Error)
			}
			return msg.Result
		case <-time.After(10 * time.Second):
			t.Fatalf("timed out waiting for app-server response %d", id)
		}
	}
}

func sendJSONLine(t *testing.T, w io.Writer, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal app-server message: %v", err)
	}
	if _, err := w.Write(append(data, '\n')); err != nil {
		t.Fatalf("write app-server message: %v", err)
	}
}

func anthropicReplyFor(body string) []anthropicSSE {
	switch {
	case strings.Contains(body, "FOLLOWUP_A"):
		return textAnthropicReply("FOLLOWUP_ANSWER_A")
	case strings.Contains(body, "Wait completed."):
		return textAnthropicReply("PARENT_FINAL_A")
	case strings.Contains(body, "PARENT_TASK_A") && !strings.Contains(body, `"type":"tool_use"`):
		return textAnthropicReply("CHILD_FINAL_A")
	case strings.Contains(body, "PARENT_QUESTION_A") && strings.Contains(body, `"type":"tool_use"`):
		return toolAnthropicReply("toolu_wait_a", "collaboration__wait_agent", "{}")
	case strings.Contains(body, "PARENT_QUESTION_A"):
		return toolAnthropicReply(
			"toolu_spawn_a",
			"collaboration__spawn_agent",
			`{"message":"PARENT_TASK_A: answer with CHILD_FINAL_A only.","task_name":"child_a","fork_turns":"none"}`,
		)
	default:
		return textAnthropicReply("UNSUPPORTED_REQUEST")
	}
}

func textAnthropicReply(text string) []anthropicSSE {
	return []anthropicSSE{
		{Type: "message_start", Message: map[string]any{"id": "msg_reply", "role": "assistant"}},
		{Type: "content_block_start", Index: 0, ContentBlock: map[string]any{"type": "text"}},
		{Type: "content_block_delta", Index: 0, Delta: map[string]any{"type": "text_delta", "text": text}},
		{Type: "content_block_stop", Index: 0},
		{Type: "message_delta", Delta: map[string]any{"stop_reason": "end_turn"}},
		{Type: "message_stop"},
	}
}

func toolAnthropicReply(id, name, arguments string) []anthropicSSE {
	return []anthropicSSE{
		{Type: "message_start", Message: map[string]any{"id": "msg_reply", "role": "assistant"}},
		{Type: "content_block_start", Index: 0, ContentBlock: map[string]any{"type": "tool_use", "id": id, "name": name, "input": map[string]any{}}},
		{Type: "content_block_delta", Index: 0, Delta: map[string]any{"type": "input_json_delta", "partial_json": arguments}},
		{Type: "content_block_stop", Index: 0},
		{Type: "message_delta", Delta: map[string]any{"stop_reason": "tool_use"}},
		{Type: "message_stop"},
	}
}

type anthropicSSE struct {
	Type         string         `json:"type"`
	Index        int            `json:"index,omitempty"`
	Message      map[string]any `json:"message,omitempty"`
	ContentBlock map[string]any `json:"content_block,omitempty"`
	Delta        map[string]any `json:"delta,omitempty"`
}

func writeAnthropicSSE(w io.Writer, flusher http.Flusher, events []anthropicSSE) {
	encoder := json.NewEncoder(w)
	for _, event := range events {
		_, _ = io.WriteString(w, "data: ")
		_ = encoder.Encode(event)
		_, _ = io.WriteString(w, "\n")
		flusher.Flush()
	}
}

func responsesReplyFor(body string) []string {
	switch {
	case strings.Contains(body, "Wait completed."):
		return textResponsesReply("msg_parent_r", "PARENT_FINAL_R")
	case strings.Contains(body, "PARENT_TASK_R") && !strings.Contains(body, `"type":"function_call"`):
		return textResponsesReply("msg_child_r", "CHILD_FINAL_R")
	case strings.Contains(body, "PARENT_QUESTION_R") && strings.Contains(body, `"type":"function_call"`):
		return toolResponsesReply("fc_wait_r", "call_wait_r", "wait_agent", "{}")
	case strings.Contains(body, "PARENT_QUESTION_R"):
		return toolResponsesReply(
			"fc_spawn_r",
			"call_spawn_r",
			"spawn_agent",
			`{"message":"PARENT_TASK_R: answer with CHILD_FINAL_R only.","task_name":"child_r","fork_turns":"none"}`,
		)
	default:
		return textResponsesReply("msg_unsupported_r", "UNSUPPORTED_REQUEST_R")
	}
}

func textResponsesReply(itemID, text string) []string {
	content := fmt.Sprintf(`[{"type":"output_text","text":%s}]`, jsonString(text))
	item := fmt.Sprintf(`{"type":"message","id":%s,"role":"assistant","content":%s}`, jsonString(itemID), content)
	return []string{
		`{"type":"response.created","response":{"id":"resp_reply_r"}}`,
		fmt.Sprintf(`{"type":"response.output_item.added","output_index":0,"item":%s}`, item),
		fmt.Sprintf(`{"type":"response.output_text.delta","item_id":%s,"output_index":0,"content_index":0,"delta":%s}`, jsonString(itemID), jsonString(text)),
		fmt.Sprintf(`{"type":"response.output_text.done","item_id":%s,"output_index":0,"content_index":0,"text":%s}`, jsonString(itemID), jsonString(text)),
		fmt.Sprintf(`{"type":"response.output_item.done","output_index":0,"item":%s}`, item),
		`{"type":"response.completed","response":{"id":"resp_reply_r"}}`,
	}
}

func toolResponsesReply(itemID, callID, name, arguments string) []string {
	item := fmt.Sprintf(
		`{"type":"function_call","id":%s,"call_id":%s,"name":%s,"namespace":"collaboration","arguments":%s}`,
		jsonString(itemID), jsonString(callID), jsonString(name), jsonString(arguments),
	)
	return []string{
		`{"type":"response.created","response":{"id":"resp_reply_r"}}`,
		fmt.Sprintf(`{"type":"response.output_item.added","output_index":0,"item":%s}`, item),
		fmt.Sprintf(`{"type":"response.function_call_arguments.delta","item_id":%s,"output_index":0,"delta":%s}`, jsonString(itemID), jsonString(arguments)),
		fmt.Sprintf(`{"type":"response.function_call_arguments.done","item_id":%s,"output_index":0,"arguments":%s}`, jsonString(itemID), jsonString(arguments)),
		fmt.Sprintf(`{"type":"response.output_item.done","output_index":0,"item":%s}`, item),
		`{"type":"response.completed","response":{"id":"resp_reply_r"}}`,
	}
}

func chatReplyFor(body []byte) []string {
	text := string(body)
	switch {
	case strings.Contains(text, "FOLLOWUP_C"):
		return textChatReply("FOLLOWUP_ANSWER_C")
	case strings.Contains(text, "PARENT_TASK_C") && !strings.Contains(text, `"tool_calls"`):
		return textChatReply("CHILD_FINAL_C")
	case strings.Contains(text, "Wait completed."):
		return textChatReply("PARENT_FINAL_C")
	case strings.Contains(text, "PARENT_QUESTION_C") && strings.Contains(text, `"tool_calls"`):
		return toolChatReply("chatu_wait_c", "collaboration__wait_agent", `{"timeout_ms":1000}`)
	case strings.Contains(text, "PARENT_QUESTION_C"):
		return toolChatReply(
			"chatu_spawn_c",
			"collaboration__spawn_agent",
			`{"message":"PARENT_TASK_C: answer with CHILD_FINAL_C only.","task_name":"child_c","fork_turns":"none"}`,
		)
	default:
		return textChatReply("UNSUPPORTED_CHAT")
	}
}

func textChatReply(text string) []string {
	return []string{
		fmt.Sprintf(`{"id":"chatcmpl-reply","choices":[{"delta":{"role":"assistant","content":%s}}]}`, jsonString(text)),
		fmt.Sprintf(`{"id":"chatcmpl-reply","choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`),
		`data: [DONE]`,
	}
}

func toolChatReply(id, name, arguments string) []string {
	call := fmt.Sprintf(`{"id":%s,"type":"function","function":{"name":%s,"arguments":%s}}`,
		jsonString(id), jsonString(name), jsonString(arguments))
	return []string{
		fmt.Sprintf(`{"id":"chatcmpl-reply","choices":[{"delta":{"role":"assistant","tool_calls":[%s]}}]}`, call),
		fmt.Sprintf(`{"id":"chatcmpl-reply","choices":[{"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`),
		`data: [DONE]`,
	}
}

func writeChatSSE(w io.Writer, flusher http.Flusher, events []string) {
	for _, event := range events {
		_, _ = io.WriteString(w, "data: "+event+"\n\n")
		flusher.Flush()
	}
}

func jsonString(value string) string {
	data, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return string(data)
}

func writeResponsesSSE(w io.Writer, flusher http.Flusher, events []string) {
	for _, event := range events {
		_, _ = io.WriteString(w, "data: "+event+"\n\n")
		flusher.Flush()
	}
}

func int64Ptr(v int64) *int64 { return &v }

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
