package tray

import (
	"testing"
)

// TestNewDefaultsTooltip 验证 Tooltip 为空时填充默认文案。
func TestNewDefaultsTooltip(t *testing.T) {
	t.Parallel()
	tr := New(Config{})
	if tr.cfg.Tooltip != "codex-api-gateway" {
		t.Fatalf("默认 tooltip 应为 codex-api-gateway，实际 %q", tr.cfg.Tooltip)
	}
}

// TestNewPreservesTooltip 验证显式 Tooltip 被保留。
func TestNewPreservesTooltip(t *testing.T) {
	t.Parallel()
	tr := New(Config{Tooltip: "自定义"})
	if tr.cfg.Tooltip != "自定义" {
		t.Fatalf("Tooltip 应为 自定义，实际 %q", tr.cfg.Tooltip)
	}
}

// TestQuitIdempotent 验证 Quit 可重复调用且仅触发一次 OnQuit。
func TestQuitIdempotent(t *testing.T) {
	t.Parallel()
	calls := 0
	tr := New(Config{OnQuit: func() { calls++ }})
	tr.Quit()
	tr.Quit()
	tr.Quit()
	if calls != 1 {
		t.Fatalf("OnQuit 应只触发 1 次，实际 %d 次", calls)
	}
}

// TestQuitNilOnQuit 验证 OnQuit 为 nil 时 Quit 不 panic。
func TestQuitNilOnQuit(t *testing.T) {
	t.Parallel()
	tr := New(Config{})
	tr.Quit() // 不应 panic
}

// TestQuitOnQuitPanicRecovered 验证 OnQuit panic 被 recover，不会传播。
func TestQuitOnQuitPanicRecovered(t *testing.T) {
	t.Parallel()
	tr := New(Config{OnQuit: func() { panic("boom") }})
	defer func() {
		if rec := recover(); rec != nil {
			t.Fatalf("Quit 应 recover panic，但传播出来了: %v", rec)
		}
	}()
	tr.Quit()
}

// TestDoneChannelCloses 验证 Done 在 closeDone 后关闭。
func TestDoneChannelCloses(t *testing.T) {
	t.Parallel()
	tr := New(Config{})
	select {
	case <-tr.Done():
		t.Fatal("Done 不应在 closeDone 前关闭")
	default:
	}
	tr.closeDone()
	select {
	case <-tr.Done():
		// 预期：已关闭
	default:
		t.Fatal("Done 应在 closeDone 后关闭")
	}
}

// TestOpenBrowserRejectsNonHTTP 验证 openBrowser 拒绝非 http(s) URL。
func TestOpenBrowserRejectsNonHTTP(t *testing.T) {
	t.Parallel()
	cases := []string{
		"", "javascript:alert(1)", "file:///etc/passwd",
		" data:text/html,x", "ftp://example.com/",
	}
	for _, raw := range cases {
		if err := openBrowser(raw); err == nil {
			t.Errorf("openBrowser(%q) 应返回错误，实际 nil", raw)
		}
	}
}

// TestLogoEmbedded 验证 go:embed 的 logo 非空且是 PNG 魔数。
func TestLogoEmbedded(t *testing.T) {
	t.Parallel()
	if len(logoBytes) == 0 {
		t.Fatal("内嵌的 logo.png 不应为空")
	}
	// PNG 魔数：89 50 4E 47 0D 0A 1A 0A
	pngMagic := []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a}
	for i, b := range pngMagic {
		if logoBytes[i] != b {
			t.Fatalf("logoBytes 不是 PNG（偏移 %d: got %02x want %02x）", i, logoBytes[i], b)
		}
	}
}
