// Package openaichat 实现 OpenAI Chat Completions 源插件。
//
// 把 Responses 请求转换为 Chat Completions 上游协议，流式结果经共享
// chatstreamconv 引擎转回 Responses SSE。hosted web_search 等请求形状
// 开关由插件按 schema 声明，不引入共享短码分支。
package openaichat
