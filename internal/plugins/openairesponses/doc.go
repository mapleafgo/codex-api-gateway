// Package openairesponses 实现 OpenAI Responses 透传源插件。
//
// 对提供原生 OpenAI Responses API 的上游做最小改写透传，保持上游 SSE
// 事件原样转发，不参与 EventGate 缓冲。
package openairesponses
