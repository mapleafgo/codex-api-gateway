package anthropic

import "strings"

// appendBeta 把 beta 值合并到现有列表，去重后逗号分隔。
func appendBeta(existing, value string) string {
	if value == "" {
		return existing
	}
	if existing == "" {
		return value
	}
	if strings.Contains(existing, value) {
		return existing
	}
	return existing + "," + value
}
