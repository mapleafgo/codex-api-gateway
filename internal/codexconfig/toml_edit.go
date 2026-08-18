package codexconfig

import (
	"strconv"
	"strings"
)

// isTableHeader 判断行是否为 TOML 表头（[xxx] 或 [[xxx]]）。
func isTableHeader(line string) bool {
	trimmed := strings.TrimSpace(line)
	return strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]")
}

// topLevelKey 读取顶层键值（只扫描首个表头之前的行），返回 (值, 是否存在)。
func topLevelKey(lines []string, key string) (string, bool) {
	for _, line := range lines {
		if isTableHeader(line) {
			return "", false
		}
		if rest, ok := valueAfterKey(line, key); ok {
			return parseStringValue(rest), true
		}
	}
	return "", false
}

// valueAfterKey 从行首解析出等号后的值片段；命中 target 键时返回该片段。
// 兼容 = 两侧有无空格（如 model_provider="x" / base_url = 'x'）。
func valueAfterKey(line, target string) (string, bool) {
	idx := strings.IndexByte(line, '=')
	if idx < 0 {
		return "", false
	}
	if key := strings.TrimSpace(line[:idx]); key != target {
		return "", false
	}
	return strings.TrimSpace(line[idx+1:]), true
}

// parseStringValue 从 "xxx" 或 'xxx' 形式提取字符串值；无法解析时返回空串。
func parseStringValue(rest string) string {
	if len(rest) < 2 || (rest[0] != '"' && rest[0] != '\'') {
		return ""
	}
	q := rest[0]
	if end := strings.IndexByte(rest[1:], q); end >= 0 {
		if q == '"' {
			if unquoted, err := strconv.Unquote(rest[:1+end+1]); err == nil {
				return unquoted
			}
			return ""
		}
		return rest[1 : 1+end]
	}
	return ""
}

// tomlQuote 输出 TOML 基本字符串，转义反斜杠和双引号。
func tomlQuote(value string) string {
	escaped := strings.ReplaceAll(value, `\`, `\\`)
	escaped = strings.ReplaceAll(escaped, `"`, `\"`)
	return `"` + escaped + `"`
}

// upsertTopLevelKey 设置顶层键：已存在则整行替换，否则插入到首个表头之前。
func upsertTopLevelKey(lines []string, key, value string) []string {
	replacement := key + ` = ` + tomlQuote(value)
	for i, line := range lines {
		if isTableHeader(line) {
			lines = append(lines[:i], append([]string{replacement}, lines[i:]...)...)
			return lines
		}
		if _, ok := valueAfterKey(line, key); ok {
			lines[i] = replacement
			return lines
		}
	}
	return append(lines, replacement)
}

// removeTopLevelKey 删除顶层键行（只删首个表头之前出现的第一个）。
func removeTopLevelKey(lines []string, key string) []string {
	for i, line := range lines {
		if isTableHeader(line) {
			return lines
		}
		if _, ok := valueAfterKey(line, key); ok {
			return append(lines[:i], lines[i+1:]...)
		}
	}
	return lines
}

// blockEndIndex 返回从表头 start 开始的块结束下标：
// 遇到非当前表子表的表头或文件末尾为止。
func blockEndIndex(lines []string, start int) int {
	header := strings.TrimSpace(lines[start])
	prefix := strings.TrimSuffix(header, "]") + "."
	for i := start + 1; i < len(lines); i++ {
		trimmed := strings.TrimSpace(lines[i])
		if isTableHeader(trimmed) {
			if strings.HasPrefix(trimmed, prefix) {
				continue
			}
			return i
		}
	}
	return len(lines)
}

// upsertTableBlock 整体覆盖 table 块（含嵌套子表），不存在时追加到文件末尾。
func upsertTableBlock(lines []string, header string, block []string) []string {
	start := -1
	for i, line := range lines {
		if strings.TrimSpace(line) == header {
			start = i
			break
		}
	}
	if start < 0 {
		if len(lines) > 0 && lines[len(lines)-1] != "" {
			lines = append(lines, "")
		}
		return append(lines, block...)
	}
	end := blockEndIndex(lines, start)
	out := make([]string, 0, start+len(block)+(len(lines)-end))
	out = append(out, lines[:start]...)
	out = append(out, block...)
	out = append(out, lines[end:]...)
	return out
}

// removeTableBlock 删除 table 块（含嵌套子表）。
func removeTableBlock(lines []string, header string) []string {
	start := -1
	for i, line := range lines {
		if strings.TrimSpace(line) == header {
			start = i
			break
		}
	}
	if start < 0 {
		return lines
	}
	end := blockEndIndex(lines, start)
	out := make([]string, 0, start+(len(lines)-end))
	out = append(out, lines[:start]...)
	out = append(out, lines[end:]...)
	return out
}

// tableValue 读取 table 块内键值（含嵌套子表前）。
func tableValue(lines []string, header, key string) (string, bool) {
	for i, line := range lines {
		if strings.TrimSpace(line) != header {
			continue
		}
		for j := i + 1; j < len(lines); j++ {
			trimmed := strings.TrimSpace(lines[j])
			if isTableHeader(trimmed) {
				return "", false
			}
			if rest, ok := valueAfterKey(lines[j], key); ok {
				return parseStringValue(rest), true
			}
		}
	}
	return "", false
}
