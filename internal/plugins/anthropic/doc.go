// Package anthropic 实现 Anthropic Messages 源插件。
//
// 将 Responses 请求转换为 Anthropic Messages 上游协议，流式结果经共享
// streamconv 引擎转回 Responses SSE。专属配置（默认输出上限、prompt
// cache 开关）通过 options 承载，不泄漏到共享核心。
package anthropic
