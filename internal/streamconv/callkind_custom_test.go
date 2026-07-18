package streamconv

import (
	"encoding/json"
	"testing"
)

// TestCustomToolInputPreservesV4APatch 锁定 customToolInput 透传 V4A patch 时不
// 修改内容——尤其空内容行 "+"（单加号）必须原样保留。apply_patch 的 V4A 格式
// 要求 Add File 块每行以 + 开头、空行用 "+" 表示；若网关剥离 + 会让 codex
// 校验报 "” is not a valid hunk header"。此测试锁定网关不做此类剥离，
// 把 apply_patch 格式问题的责任界定在模型侧（上游生成的 patch 本身）。
func TestCustomToolInputPreservesV4APatch(t *testing.T) {
	patch := "*** Begin Patch\n*** Add File: /tmp/x\n+# title\n+\n+body line\n*** End Patch"
	rawBytes, err := json.Marshal(map[string]string{"input": patch})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := customToolInput(string(rawBytes))
	if got != patch {
		t.Fatalf("customToolInput 修改了 patch 内容（不应剥离 V4A 的 + 前缀）:\nwant=%q\ngot =%q", patch, got)
	}
}

// TestCustomToolInputPreservesBlankLines 验证含连续空行的 patch 也能原样透传
// （GLM 实测中生成的 patch 混入了真空行，这里确认网关不会进一步改动）。
func TestCustomToolInputPreservesBlankLines(t *testing.T) {
	patch := "*** Begin Patch\n*** Add File: /tmp/x\n+# title\n\n+\n\n+## section\n*** End Patch"
	rawBytes, _ := json.Marshal(map[string]string{"input": patch})
	got := customToolInput(string(rawBytes))
	if got != patch {
		t.Fatalf("blank-line patch not preserved:\nwant=%q\ngot =%q", patch, got)
	}
}
