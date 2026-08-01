package anthropic

import "strings"

// ExtendedCacheTTLBetaHeader 是 Anthropic 扩展缓存 TTL 的 beta header 值。
const ExtendedCacheTTLBetaHeader = "extended-cache-ttl-2025-04-11"

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
