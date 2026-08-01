package toolcatalog

import (
	"strings"
	"testing"

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
		freeform bool
	}{
		{"function", oairesponses.ToolUnionParam{OfFunction: &oairesponses.FunctionToolParam{Name: "exec_command", Parameters: map[string]any{"type": "object"}}}, "exec_command", false},
		{"custom", oairesponses.ToolUnionParam{OfCustom: &oairesponses.CustomToolParam{Name: "my_raw"}}, "my_raw", true},
		{"apply_patch", oairesponses.ToolUnionParam{OfApplyPatch: &oairesponses.ApplyPatchToolParam{}}, "apply_patch", true},
		{"shell", oairesponses.ToolUnionParam{OfShell: &oairesponses.FunctionShellToolParam{}}, "shell", true},
		{"local_shell", oairesponses.ToolUnionParam{OfLocalShell: &oairesponses.ToolLocalShellParam{}}, "shell", true},
		{"tool_search", oairesponses.ToolUnionParam{OfToolSearch: &oairesponses.ToolSearchToolParam{}}, "tool_search", false},
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
			if tool.Type != "" {
				t.Fatalf("client tool 应省略 type 字段，got %q", tool.Type)
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

	// apply_patch 使用 freeform {s:string} schema（Codex 只消费 V4A 文本）
	decls, _ := Declare(oairesponses.ToolUnionParam{OfApplyPatch: &oairesponses.ApplyPatchToolParam{}})
	props, _ := decls[0].OfTool.InputSchema.Properties.(map[string]any)
	if props["s"] == nil {
		t.Fatalf("apply_patch schema missing freeform s: %#v", props)
	}
	if _, ok := props["operation"]; ok {
		t.Fatalf("apply_patch must not declare structured operation schema: %#v", props)
	}
	if decls[0].OfTool.Description.Valid() {
		t.Fatalf("apply_patch desc should be empty: %#v", decls[0].OfTool.Description)
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
}
