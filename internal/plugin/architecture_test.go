package plugin

import (
	"os/exec"
	"strings"
	"testing"
)

// sharedPackages 是只允许依赖插件契约、禁止依赖具体插件实现的共享核心包。
var sharedPackages = []string{
	"github.com/mapleafgo/codex-api-gateway/internal/admin",
	"github.com/mapleafgo/codex-api-gateway/internal/config",
	"github.com/mapleafgo/codex-api-gateway/internal/configwatch",
	"github.com/mapleafgo/codex-api-gateway/internal/health",
	"github.com/mapleafgo/codex-api-gateway/internal/scheduler",
	"github.com/mapleafgo/codex-api-gateway/internal/server",
}

const concretePluginPrefix = "github.com/mapleafgo/codex-api-gateway/internal/plugins/"

// TestSharedPackagesDoNotImportPlugins 断言共享核心包不 import 具体插件实现。
// 具体插件只能在 cmd/server 组装入口注册。
func TestSharedPackagesDoNotImportPlugins(t *testing.T) {
	args := append([]string{"list", "-deps", "-f", "{{.ImportPath}} {{join .Imports \" \"}}"}, sharedPackages...)
	out, err := exec.Command("go", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("go list deps 失败: %v\n%s", err, out)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, " ", 2)
		pkg := parts[0]
		imports := ""
		if len(parts) == 2 {
			imports = parts[1]
		}
		for _, imp := range strings.Fields(imports) {
			if strings.HasPrefix(imp, concretePluginPrefix) {
				t.Fatalf("共享包 %s 不得 import 具体插件实现 %s", pkg, imp)
			}
		}
	}
}
