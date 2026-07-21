package toolcatalog

import (
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	oairesponses "github.com/openai/openai-go/v3/responses"
)

// TestCompletionMatrixDeclareClientTools 锁定 Codex 客户端主路径工具声明：
// function / custom / shell / local_shell / apply_patch / tool_search / namespace。
// skill 加载不走独立 tool type，由 Codex 用 function(exec_command) 读 SKILL.md。
func TestCompletionMatrixDeclareClientTools(t *testing.T) {
	cases := []struct {
		name     string
		tool     oairesponses.ToolUnionParam
		wantName string
		custom   bool
		freeform bool
	}{
		{"function", oairesponses.ToolUnionParam{OfFunction: &oairesponses.FunctionToolParam{Name: "exec_command", Parameters: map[string]any{"type": "object"}}}, "exec_command", false, false},
		{"custom", oairesponses.ToolUnionParam{OfCustom: &oairesponses.CustomToolParam{Name: "my_raw"}}, "my_raw", true, true},
		{"apply_patch", oairesponses.ToolUnionParam{OfApplyPatch: &oairesponses.ApplyPatchToolParam{}}, "apply_patch", true, true},
		{"shell", oairesponses.ToolUnionParam{OfShell: &oairesponses.FunctionShellToolParam{}}, "shell", true, true},
		{"local_shell", oairesponses.ToolUnionParam{OfLocalShell: &oairesponses.ToolLocalShellParam{}}, "shell", true, true},
		{"tool_search", oairesponses.ToolUnionParam{OfToolSearch: &oairesponses.ToolSearchToolParam{}}, "tool_search", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			decls, err := Declare(tc.tool)
			if err != nil {
				t.Fatal(err)
			}
			if len(decls) != 1 || decls[0].OfTool == nil {
				t.Fatalf("decls=%+v", decls)
			}
			tool := decls[0].OfTool
			if tool.Name != tc.wantName {
				t.Fatalf("name=%q want %q", tool.Name, tc.wantName)
			}
			if tc.custom && tool.Type != anthropic.ToolTypeCustom {
				t.Fatalf("want custom type, got %q", tool.Type)
			}
			ids, err := Inspect(tc.tool)
			if err != nil {
				t.Fatal(err)
			}
			if ids[0].Freeform != tc.freeform {
				t.Fatalf("freeform=%v want %v", ids[0].Freeform, tc.freeform)
			}
		})
	}

	// apply_patch 必须 freeform schema（input 字符串），不得 structured operation
	decls, _ := Declare(oairesponses.ToolUnionParam{OfApplyPatch: &oairesponses.ApplyPatchToolParam{}})
	props, _ := decls[0].OfTool.InputSchema.Properties.(map[string]any)
	if props["input"] == nil {
		t.Fatalf("apply_patch schema missing freeform input: %#v", props)
	}
	if _, ok := props["operation"]; ok {
		t.Fatalf("apply_patch must not be structured: %#v", props)
	}
	if !decls[0].OfTool.Description.Valid() || !strings.Contains(decls[0].OfTool.Description.Value, "Begin Patch") {
		t.Fatalf("apply_patch desc should document V4A: %#v", decls[0].OfTool.Description)
	}
}

// TestCompletionMatrixSanitizeSkillAndPatchPaths 覆盖 skill 读文件（function 大参数）
// 与 apply_patch 两条最易坏的回程路径。
func TestCompletionMatrixSanitizeSkillAndPatchPaths(t *testing.T) {
	// skill 路径：exec_command 参数里若带整型 float，必须收成整数
	got := SanitizeClientToolInput("exec_command", false, `{"cmd":"cat SKILL.md","yield_time_ms":120000.0}`)
	if strings.Contains(got, "120000.0") {
		t.Fatalf("skill-related function args still float: %s", got)
	}
	// apply_patch 多星
	raw := `{"input":"*** Begin Patch ***\n*** Add File: skills/x/SKILL.md\n+# hi\n*** End Patch ***"}`
	patch := SanitizeClientToolInput("apply_patch", true, raw)
	if !strings.HasPrefix(patch, "*** Begin Patch\n") || strings.Contains(patch, "Patch ***") {
		t.Fatalf("patch not normalized: %q", patch)
	}
}
