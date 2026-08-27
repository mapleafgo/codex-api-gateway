package copilot

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mapleafgo/codex-api-gateway/internal/config"
)

// modelsPayload 是缓存测试共用的最小模型目录。
const modelsPayload = `{"data":[{"id":"m","model_picker_enabled":true,"supported_endpoints":["/responses"],"capabilities":{"type":"chat"}}]}`

func TestModelCacheDefaultTTLIsFiveMinutes(t *testing.T) {
	cache := newModelCache(nil, 0)
	if cache.ttl != modelsTTL {
		t.Fatalf("default ttl = %v, want %v", cache.ttl, modelsTTL)
	}
}

// TestClientPerSourceStateIsolated 验证 Client 的目录/端点是按源名隔离的：
// 两个不同名称的源各自拉取，互不复用缓存。
func TestClientPerSourceStateIsolated(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if got := r.Header.Get("Authorization"); !strings.HasPrefix(got, "Bearer ") {
			t.Errorf("Authorization = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(modelsPayload))
	}))
	defer srv.Close()

	client := NewWithHTTP(srv.Client(), srv.URL)
	ctx := context.Background()
	one := tokenSource("one", "token-one", srv.URL)
	two := tokenSource("two", "token-two", srv.URL)

	if _, err := client.Directory(ctx, one); err != nil {
		t.Fatalf("Directory(one): %v", err)
	}
	if _, err := client.Directory(ctx, one); err != nil {
		t.Fatalf("Directory(one) cached: %v", err)
	}
	if _, err := client.Directory(ctx, two); err != nil {
		t.Fatalf("Directory(two): %v", err)
	}
	if got := hits.Load(); got != 2 {
		t.Fatalf("upstream hits = %d, want 2 (per-source cache)", got)
	}
}

// TestClientTokenChangeInvalidatesState 验证 token 变化时该源的缓存重建：
// 旧 token 的模型目录不能被新 token 的请求复用。
func TestClientTokenChangeInvalidatesState(t *testing.T) {
	var auth atomic.Value
	auth.Store("")
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		auth.Store(r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(modelsPayload))
	}))
	defer srv.Close()

	client := NewWithHTTP(srv.Client(), srv.URL)
	ctx := context.Background()
	src := tokenSource("s", "token-a", srv.URL)
	if _, err := client.Directory(ctx, src); err != nil {
		t.Fatalf("Directory: %v", err)
	}
	if got := auth.Load().(string); got != "Bearer token-a" {
		t.Fatalf("Authorization = %q, want Bearer token-a", got)
	}

	src.Options["github_token"] = "token-b"
	if _, err := client.Directory(ctx, src); err != nil {
		t.Fatalf("Directory after token change: %v", err)
	}
	if got := auth.Load().(string); got != "Bearer token-b" {
		t.Fatalf("Authorization after change = %q, want Bearer token-b", got)
	}
}

// TestClientEndpointChangeInvalidatesState 验证显式 endpoint（BaseURL）变化时
// 走新的端点拉取，而不是沿用旧端点的模型缓存。
func TestClientEndpointChangeInvalidatesState(t *testing.T) {
	var endpoints atomic.Value
	endpoints.Store("")
	srvA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		endpoints.Store("A")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(modelsPayload))
	}))
	defer srvA.Close()
	srvB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		endpoints.Store("B")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(modelsPayload))
	}))
	defer srvB.Close()

	client := NewWithHTTP(srvA.Client(), "")
	ctx := context.Background()
	src := tokenSource("s", "token", "")
	src.BaseURL = srvA.URL
	if _, err := client.Directory(ctx, src); err != nil {
		t.Fatalf("Directory(A): %v", err)
	}
	if got := endpoints.Load().(string); got != "A" {
		t.Fatalf("first fetch endpoint = %q, want A", got)
	}

	src.BaseURL = srvB.URL
	if _, err := client.Directory(ctx, src); err != nil {
		t.Fatalf("Directory(B): %v", err)
	}
	if got := endpoints.Load().(string); got != "B" {
		t.Fatalf("second fetch endpoint = %q, want B", got)
	}
}

// TestClientConcurrentDirectoryReads 验证已缓存目录的并发读安全（配合 -race），
// 且并发读不会触发额外上游请求。
func TestClientConcurrentDirectoryReads(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(modelsPayload))
	}))
	defer srv.Close()

	client := NewWithHTTP(srv.Client(), srv.URL)
	ctx := context.Background()
	src := tokenSource("c", "token", srv.URL)
	if _, err := client.Directory(ctx, src); err != nil {
		t.Fatalf("warm Directory: %v", err)
	}

	const readers = 32
	var wg sync.WaitGroup
	errs := make(chan error, readers)
	for range readers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			dir, err := client.Directory(ctx, src)
			if err != nil {
				errs <- err
				return
			}
			if len(dir.Models) != 1 || dir.Models[0].ID != "m" {
				errs <- &unexpectedDirectory{dir: dir}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent read: %v", err)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("upstream hits = %d, want 1", got)
	}
}

// TestClientConcurrentMissRefreshFetchesOnce 验证空缓存上的并发 miss 经 singleflight
// 折叠为一次上游请求：N 个 goroutine 同时刷新，hits 必须为 1。
func TestClientConcurrentMissRefreshFetchesOnce(t *testing.T) {
	var hits atomic.Int32
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		<-release // 持住首个请求，确保其余 goroutine 在缓存未就绪时并发入场
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(modelsPayload))
	}))
	defer srv.Close()

	client := NewWithHTTP(srv.Client(), srv.URL)
	ctx := context.Background()
	src := tokenSource("singleflight", "token", srv.URL)

	const workers = 16
	start := make(chan struct{})
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			dir, err := client.Directory(ctx, src)
			if err != nil {
				errs <- err
				return
			}
			if len(dir.Models) != 1 || dir.Models[0].ID != "m" {
				errs <- &unexpectedDirectory{dir: dir}
			}
		}()
	}
	close(start)

	deadline := time.After(5 * time.Second)
	for hits.Load() == 0 {
		select {
		case <-deadline:
			t.Fatal("no upstream request observed")
		case <-time.After(10 * time.Millisecond):
		}
	}
	close(release)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent miss: %v", err)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("upstream hits = %d, want 1 (singleflight)", got)
	}
}

type unexpectedDirectory struct {
	dir Directory
}

func (e *unexpectedDirectory) Error() string {
	b, _ := json.Marshal(e.dir.Models)
	return "unexpected models: " + string(b)
}

func tokenSource(name, token, baseURL string) config.Source {
	return config.Source{
		Name:    name,
		BaseURL: baseURL,
		Backend: pluginIDCopilot,
		Options: map[string]any{"github_token": token},
	}
}

// pluginIDCopilot 与插件稳定 ID 保持一致，避免测试文件重复导入 internal/plugin。
const pluginIDCopilot = "github-copilot"
