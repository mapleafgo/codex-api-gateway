package codexconfig

import "strings"

// isTableHeader 判断行是否为 TOML 表头（[xxx] 或 [[xxx]]）。
func isTableHeader(line string) bool {
	trimmed := strings.TrimSpace(line)
	return strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]")
}

// topLevelKey 读取顶层键值（只扫描首个表头之前的行），返回 (值, 是否存在)。
func topLevelKey(lines []string, key string) (string, bool) {
	prefix := key + " ="
	for _, line := range lines {
		if isTableHeader(line) {
			return "", false
		}
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, prefix) {
			return parseStringValue(trimmed[len(prefix):]), true
		}
	}
	return "", false
}

// parseStringValue 从 "xxx" 形式提取字符串值；无法解析时返回空串。
func parseStringValue(rest string) string {
	start := strings.IndexByte(rest, '"')
	if start < 0 {
		return ""
	}
	end := strings.LastIndexByte(rest, '"')
	if end <= start {
		return ""
	}
	return rest[start+1 : end]
}

// upsertTopLevelKey 设置顶层键：已存在则整行替换，否则插入到首个表头之前。
func upsertTopLevelKey(lines []string, key, value string) []string {
	replacement := key + ` = "` + value + `"`
	for i, line := range lines {
		if isTableHeader(line) {
			lines = append(lines[:i], append([]string{replacement}, lines[i:]...)...)
			return lines
		}
		if strings.HasPrefix(strings.TrimSpace(line), key+" =") {
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
		if strings.HasPrefix(strings.TrimSpace(line), key+" =") {
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
			if strings.HasPrefix(trimmed, key+" =") {
				return parseStringValue(trimmed[len(key)+2:]), true
			}
		}
	}
	return "", false
}
