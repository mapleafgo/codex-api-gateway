package toolcatalog

import (
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// SanitizeClientToolInput 是回程 client tool 参数的统一出口。
//
// freeform=true：先从 Anthropic {"input":"..."} 解包裸文本，再按工具名做
// 契约归一（目前 apply_patch V4A）。
// freeform=false：把 JSON 参数里「整数值却写成 1.0」的 number 收成整数字面量，
// 避免 Codex/Rust serde 报 floating point expected i32/i64/u64。
//
// 只做可控、可逆修复；解析失败时原样返回 raw，不发明内容。
func SanitizeClientToolInput(toolName string, freeform bool, raw string) string {
	if raw == "" {
		return raw
	}
	if freeform {
		return sanitizeFreeformInput(toolName, raw)
	}
	return SanitizeJSONIntegerNumbers(raw)
}

func sanitizeFreeformInput(toolName, raw string) string {
	text := unwrapFreeformInput(raw)
	switch toolName {
	case "apply_patch":
		return NormalizeApplyPatchInput(text)
	default:
		return text
	}
}

// unwrapFreeformInput 从 {"input":"..."} 解出文本；非该形态则原样返回。
func unwrapFreeformInput(raw string) string {
	var obj map[string]any
	if err := json.Unmarshal([]byte(raw), &obj); err != nil {
		return raw
	}
	if input, ok := obj["input"].(string); ok {
		return input
	}
	// structured apply_patch 等：保留 raw 给下游 name 特化处理
	return raw
}

// NormalizeApplyPatchInput 归一 Codex V4A freeform patch。
//
// 常见故障：模型写 "*** Begin Patch ***" / "*** End Patch ***"（多一对星），
// 客户端严格要求首行恰好 "*** Begin Patch"。
// 另：历史/声明曾暴露 structured（operation/path/diff），模型可能产出 JSON，
// 此处折回 V4A。
func NormalizeApplyPatchInput(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return raw
	}
	if s, ok := structuredApplyPatchToV4A(raw); ok {
		raw = s
	}
	// 整段被 JSON 字符串引号包住
	if len(raw) >= 2 && raw[0] == '"' {
		var s string
		if err := json.Unmarshal([]byte(raw), &s); err == nil && s != "" {
			raw = s
		}
	}
	lines := strings.Split(raw, "\n")
	for i, line := range lines {
		trim := strings.TrimRight(line, "\r")
		switch {
		case isBeginPatchMarker(trim):
			lines[i] = "*** Begin Patch"
		case isEndPatchMarker(trim):
			lines[i] = "*** End Patch"
		default:
			lines[i] = trim
		}
	}
	return strings.Join(lines, "\n")
}

func isBeginPatchMarker(line string) bool {
	s := strings.TrimSpace(line)
	if s == "*** Begin Patch" {
		return true
	}
	if !strings.HasPrefix(s, "*** Begin Patch") {
		return false
	}
	rest := strings.TrimSpace(strings.TrimPrefix(s, "*** Begin Patch"))
	return rest == "" || rest == "***" || strings.Trim(rest, "* \t") == ""
}

func isEndPatchMarker(line string) bool {
	s := strings.TrimSpace(line)
	if s == "*** End Patch" {
		return true
	}
	if !strings.HasPrefix(s, "*** End Patch") {
		return false
	}
	rest := strings.TrimSpace(strings.TrimPrefix(s, "*** End Patch"))
	return rest == "" || rest == "***" || strings.Trim(rest, "* \t") == ""
}

func structuredApplyPatchToV4A(raw string) (string, bool) {
	var obj map[string]any
	if err := json.Unmarshal([]byte(raw), &obj); err != nil {
		return "", false
	}
	op, _ := obj["operation"].(string)
	path, _ := obj["path"].(string)
	if op == "" || path == "" {
		return "", false
	}
	diff, _ := obj["diff"].(string)
	s := FormatApplyPatchV4A(op, path, diff)
	if s == "" {
		return "", false
	}
	return s, true
}

func writeDiffBody(b *strings.Builder, diff string) {
	if diff == "" {
		return
	}
	trimmed := strings.TrimLeft(diff, "\n")
	if strings.HasPrefix(trimmed, "*** Add File:") ||
		strings.HasPrefix(trimmed, "*** Update File:") ||
		strings.HasPrefix(trimmed, "*** Delete File:") {
		for _, line := range strings.Split(diff, "\n") {
			t := strings.TrimRight(line, "\r")
			if isBeginPatchMarker(t) || isEndPatchMarker(t) {
				continue
			}
			b.WriteString(t)
			b.WriteByte('\n')
		}
		return
	}
	b.WriteString(diff)
	if !strings.HasSuffix(diff, "\n") {
		b.WriteByte('\n')
	}
}

// ApplyPatchDescription 声明侧简短说明，强调 V4A 标记字面量（无尾部 ***）。
func ApplyPatchDescription() string {
	return fmt.Sprintf(
		"Apply a freeform V4A patch as plain text. First line must be exactly %q and last line exactly %q (no extra stars). Use %q / %q / %q headers.",
		"*** Begin Patch",
		"*** End Patch",
		"*** Add File: path",
		"*** Update File: path",
		"*** Delete File: path",
	)
}

// FormatApplyPatchV4A 从 OpenAI apply_patch_call 的 operation 拼 V4A 文本，
// 供历史回灌与 freeform 契约对齐（不要再回灌 structured JSON）。
func FormatApplyPatchV4A(operation, path, diff string) string {
	var b strings.Builder
	b.WriteString("*** Begin Patch\n")
	switch operation {
	case "create_file":
		b.WriteString("*** Add File: ")
		b.WriteString(path)
		b.WriteByte('\n')
		writeDiffBody(&b, diff)
	case "update_file":
		b.WriteString("*** Update File: ")
		b.WriteString(path)
		b.WriteByte('\n')
		writeDiffBody(&b, diff)
	case "delete_file":
		b.WriteString("*** Delete File: ")
		b.WriteString(path)
		b.WriteByte('\n')
	default:
		return ""
	}
	b.WriteString("*** End Patch")
	return b.String()
}

// SanitizeJSONIntegerNumbers 把 JSON 中可精确表示为整数的 number（如 85100.0）
// 重新编码为无小数点的整数。递归处理 object/array。
// 非法 JSON 或非整段 JSON 原样返回。
func SanitizeJSONIntegerNumbers(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return raw
	}
	dec := json.NewDecoder(strings.NewReader(trimmed))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return raw
	}
	// 尾部非空白 → 非整段 JSON
	rest, err := io.ReadAll(dec.Buffered())
	if err != nil {
		return raw
	}
	// Decoder 可能已读完 buffer；再扫 reader 剩余
	more, _ := io.ReadAll(dec.Buffered())
	_ = more
	// 用第二个 decoder 验证无多余 token
	if hasTrailingJSONTokens(trimmed) {
		return raw
	}
	coerced := coerceJSONNumbers(v)
	out, err := json.Marshal(coerced)
	if err != nil {
		return raw
	}
	_ = rest
	return string(out)
}

func hasTrailingJSONTokens(s string) bool {
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return true
	}
	tok, err := dec.Token()
	if err == io.EOF {
		return false
	}
	return tok != nil || err == nil
}

func coerceJSONNumbers(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, child := range t {
			t[k] = coerceJSONNumbers(child)
		}
		return t
	case []any:
		for i, child := range t {
			t[i] = coerceJSONNumbers(child)
		}
		return t
	case json.Number:
		s := t.String()
		if i, err := strconv.ParseInt(s, 10, 64); err == nil {
			return i
		}
		// 1.0 / 300000.0（无科学计数）
		if strings.ContainsAny(s, "eE") {
			return t
		}
		if f, err := t.Float64(); err == nil {
			if f == float64(int64(f)) && f >= float64(-1<<63) && f < float64(uint64(1)<<63) {
				return int64(f)
			}
		}
		return t
	default:
		return v
	}
}
