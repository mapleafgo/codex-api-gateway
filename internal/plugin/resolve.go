package plugin

import "github.com/mapleafgo/codex-api-gateway/internal/config"

// ResolveModel 按源 ModelMap / DefaultModel 解析上游模型名；
// 无映射时原样返回客户端模型名。
func ResolveModel(src *config.Source, reqModel string) string {
	if m, ok := src.ModelMap[reqModel]; ok {
		return m
	}
	if src.DefaultModel != "" {
		return src.DefaultModel
	}
	return reqModel
}
