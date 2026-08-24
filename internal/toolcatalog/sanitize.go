package toolcatalog

import (
	"encoding/json"
	"io"
	"strconv"
	"strings"
)

// SanitizeClientToolInput 是回程 client tool 参数的统一出口。
//
// freeform=true：先从 Anthropic {"s":"..."} 解包裸文本。
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

// NormalizeToolArgs 强校验工具参数必须是合法 JSON：空串或非法 JSON 统一降级
// 为空对象，合法 JSON 原样返回。放在数据定型处调用，保证进入客户端会话
// 历史的参数合法，避免截断/畸形参数回灌时被严格上游直接 400。
func NormalizeToolArgs(raw string) string {
	if raw == "" || !json.Valid([]byte(raw)) {
		return "{}"
	}
	return raw
}

func sanitizeFreeformInput(toolName, raw string) string {
	if toolName == "apply_patch" {
		return sanitizeApplyPatchFreeform(raw)
	}
	return unwrapSingleValueJSON(raw)
}

// unwrapSingleValueJSON 从单键 JSON 对象中取出字符串值；键名不敏感。
// opencode 等上游把 freeform 文本包在 input/patch/cmd 等不同键下，
// 统一取唯一值，避免 Codex 把整段 JSON 当工具入参。多键或非 string 值原样返回。
func unwrapSingleValueJSON(raw string) string {
	var obj map[string]any
	if err := json.Unmarshal([]byte(raw), &obj); err != nil {
		return raw
	}
	if len(obj) != 1 {
		return raw
	}
	for _, v := range obj {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return raw
}

// sanitizeApplyPatchFreeform 处理 apply_patch 的 freeform 契约：
// 优先解单键对象（input/patch 等任意键名）；若模型按历史回灌形态输出 structured
// operation/path/diff JSON，兜底折成 V4A 文本，保证 Codex 可执行。
func sanitizeApplyPatchFreeform(raw string) string {
	if text := unwrapSingleValueJSON(raw); text != raw {
		return text
	}
	if patch, ok := structuredApplyPatchToV4A(raw); ok {
		return patch
	}
	return raw
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
	patch := formatApplyPatchV4A(op, path, diff)
	if patch == "" {
		return "", false
	}
	return patch, true
}

// formatApplyPatchV4A 从 apply_patch_call 的 operation 拼 V4A 文本。
func formatApplyPatchV4A(operation, path, diff string) string {
	var b strings.Builder
	b.WriteString("*** Begin Patch\n")
	switch operation {
	case "create_file":
		b.WriteString("*** Add File: ")
		b.WriteString(path)
		b.WriteByte('\n')
		b.WriteString(diff)
	case "update_file":
		b.WriteString("*** Update File: ")
		b.WriteString(path)
		b.WriteByte('\n')
		b.WriteString(diff)
	case "delete_file":
		b.WriteString("*** Delete File: ")
		b.WriteString(path)
		b.WriteByte('\n')
	default:
		return ""
	}
	if diff != "" && !strings.HasSuffix(diff, "\n") {
		b.WriteByte('\n')
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
	// 尾部有多余 token → 非整段 JSON，原样返回
	if hasTrailingJSONTokens(trimmed) {
		return raw
	}
	coerced := coerceJSONNumbers(v)
	out, err := json.Marshal(coerced)
	if err != nil {
		return raw
	}
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
