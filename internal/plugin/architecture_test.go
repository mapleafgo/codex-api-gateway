package plugin

import (
	"os"
	"os/exec"
	"path/filepath"
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

// forbiddenSourceFacts 是共享核心包非测试源码中禁止出现的事实词。任何涉及
// 具体源插件（Copilot）或旧版短码的标识都必须收在 internal/plugins/* 包内。
var forbiddenSourceFacts = []string{
	"copilot",
	"github_token",
	"githubtoken",
	"backend_type",
	"backendgithub",
}

// configLegacyErrors 是唯一允许保留旧标识的白名单文件：internal/config/config.go
// 的迁移错误信息必须指名旧字段，供用户对照迁移，这是合法需求而非源专属逻辑。
const configLegacyErrorsFile = "config.go"

// TestSharedCoreHasNoSourceFacts 对共享核心包做文本级守护：非 test 的 Go 源码
// 出现任何来源事实词即失败。与 TestSharedPackagesDoNotImportPlugins（import 级）
// 互补，防止把源专属分支以硬编码形式写回共享层。
func TestSharedCoreHasNoSourceFacts(t *testing.T) {
	targets := []struct {
		dir      string
		skipFile string
	}{
		{dir: "../admin"},
		{dir: "../config", skipFile: configLegacyErrorsFile},
		{dir: "../configwatch"},
		{dir: "../health"},
		{dir: "../scheduler"},
		{dir: "../server"},
	}
	for _, target := range targets {
		entries, err := os.ReadDir(target.dir)
		if err != nil {
			t.Fatalf("read %s: %v", target.dir, err)
		}
		for _, entry := range entries {
			name := entry.Name()
			if entry.IsDir() || filepath.Ext(name) != ".go" ||
				strings.HasSuffix(name, "_test.go") || name == target.skipFile {
				continue
			}
			src, err := os.ReadFile(filepath.Join(target.dir, name))
			if err != nil {
				t.Fatalf("read %s: %v", filepath.Join(target.dir, name), err)
			}
			lower := strings.ToLower(string(src))
			for _, fact := range forbiddenSourceFacts {
				if strings.Contains(lower, fact) {
					t.Errorf("%s contains source-specific identifier %q; 源事实必须收敛到 internal/plugins/*",
						filepath.Join(target.dir, name), fact)
				}
			}
		}
	}
}
