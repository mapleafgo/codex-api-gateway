package backend

import "github.com/mapleafgo/codex-api-gateway/internal/config"

// resolveModel 按源 ModelMap / DefaultModel 解析上游模型名。
func resolveModel(src *config.Source, reqModel string) string {
	if m, ok := src.ModelMap[reqModel]; ok {
		return m
	}
	if src.DefaultModel != "" {
		return src.DefaultModel
	}
	return reqModel
}
