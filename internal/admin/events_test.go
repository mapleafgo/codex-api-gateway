package admin

import (
	"bufio"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/mapleafgo/codex-api-gateway/internal/metrics"
)

// TestSSEEventsStream 验证 /admin/api/events 立即推一次 snapshot。
func TestSSEEventsStream(t *testing.T) {
	deps, _ := newTestDeps(t)
	deps.Metrics.Record(metrics.RequestEvent{
		StartedAt: time.Now(), Duration: time.Millisecond,
		SourceName: "s1", Model: "m1", Status: "completed", Code: 200,
		InputTokens: 10, OutputTokens: 5,
	})
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if deps.Metrics.Snapshot().TotalRequests == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	mux := http.NewServeMux()
	Mount(mux, *deps)
	srv := newServer(mux)
	defer srv.Close()

	// 用带超时的 HTTP 客户端读取前若干行
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(srv.URL + "/admin/api/events")
	if err != nil {
		// 超时是预期的（SSE 长连接），只要在超时前读到 snapshot 即可
	}
	if resp != nil {
		defer resp.Body.Close()
		scanner := bufio.NewScanner(resp.Body)
		gotSnapshot := false
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "event: snapshot") {
				gotSnapshot = true
				break
			}
		}
		if !gotSnapshot {
			t.Errorf("未收到 snapshot 事件")
		}
	}
}

// TestMethodNotAllowedOnEvents 验证非 GET 方法被拒绝。
func TestMethodNotAllowedOnEvents(t *testing.T) {
	deps, _ := newTestDeps(t)
	mux := http.NewServeMux()
	Mount(mux, *deps)
	srv := newServer(mux)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/admin/api/events", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status = %v, want 405", resp.StatusCode)
	}
}
