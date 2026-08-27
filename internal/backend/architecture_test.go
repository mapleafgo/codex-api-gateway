package backend

import (
	"os"
	"strings"
	"testing"
)

// forbiddenFacts 是共享后端不得出现的专属源标识。任何涉及 Copilot 的 wire 事实
// 都必须收在 internal/plugins/copilot 包内，由插件在委托前完成。
var forbiddenFacts = []string{"copilot", "github_token", "githubtoken", "backendgithubcopilot"}

// TestSharedBackendHasNoSourceFacts 断言共享后端源码零 Copilot 专属标识，
// 防止后续迭代把源专属分支重新写回共享层。
func TestSharedBackendHasNoSourceFacts(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read backend package: %v", err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		src, err := os.ReadFile(entry.Name())
		if err != nil {
			t.Fatalf("read %s: %v", entry.Name(), err)
		}
		lower := strings.ToLower(string(src))
		for _, fact := range forbiddenFacts {
			if strings.Contains(lower, fact) {
				t.Errorf("%s contains source-specific identifier %q", entry.Name(), fact)
			}
		}
	}
}
