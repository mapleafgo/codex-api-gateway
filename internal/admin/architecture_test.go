package admin

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAdminDoesNotImportForwardingComponents(t *testing.T) {
	banned := map[string]bool{
		"github.com/mapleafgo/codex-api-gateway/internal/backend":   true,
		"github.com/mapleafgo/codex-api-gateway/internal/breaker":   true,
		"github.com/mapleafgo/codex-api-gateway/internal/scheduler": true,
		"github.com/mapleafgo/codex-api-gateway/internal/server":    true,
	}

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read admin package: %v", err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, filepath.Join(".", entry.Name()), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", entry.Name(), err)
		}
		for _, imp := range file.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			if banned[path] {
				t.Errorf("%s imports forbidden forwarding component %s", entry.Name(), path)
			}
		}
	}
}
