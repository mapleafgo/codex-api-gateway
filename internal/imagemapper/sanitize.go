package imagemapper

import (
	"net/url"
	"strconv"
	"strings"
)

// SanitizeURL 返回适合入日志的图片地址：
// 普通 URL 抹掉 query 与 fragment 只保留基础地址；data URI 只返回类型与字节数元数据。
func SanitizeURL(u string) string {
	if hasDataPrefix(u) {
		return dataURIMetadata(u)
	}
	parsed, err := url.Parse(u)
	if err != nil {
		return u
	}
	parsed.RawQuery = ""
	parsed.Fragment = ""
	parsed.User = nil
	return parsed.String()
}

func hasDataPrefix(s string) bool {
	return strings.HasPrefix(s, "data:")
}

// dataURIMetadata 提取 media type 与 base64 字节数，不携带本体。
// 形如 data:image/png;base64,<bytes=NNN>。
func dataURIMetadata(s string) string {
	rest := strings.TrimPrefix(s, "data:")
	comma := strings.Index(rest, ",")
	if comma < 0 {
		return "data:<unknown>"
	}
	media := rest[:comma]
	payload := rest[comma+1:]
	bytes := len(payload)*3/4 - strings.Count(payload, "=")
	if i := strings.Index(media, ";"); i >= 0 {
		media = media[:i]
	}
	return "data:" + media + ";base64,<bytes=" + strconv.Itoa(bytes) + ">"
}
