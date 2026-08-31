// Package imagemapper 对 OpenAI Responses 的图片识别输入做统一映射判定，
// 供 Anthropic（a）与 Chat（c）转换路径消费，并输出日志脱敏工具。
package imagemapper

import (
	oparam "github.com/openai/openai-go/v3/packages/param"
	oairesponses "github.com/openai/openai-go/v3/responses"
)

// Kind 表达一条 input_image 的可映射判定。
type Kind int

const (
	// KindMapped 表示存在可用的 image_url（URL 或 data URI），可安全映射。
	KindMapped Kind = iota
	// KindFileID 表示仅含 file_id，网关无 OpenAI Files 凭据，协议不可映射。
	KindFileID
	// KindMalformed 表示既无 image_url 也无 file_id，畸形输入。
	KindMalformed
)

// Decision 是一条 input_image 的中立判定结果，不含目标协议 SDK 类型。
type Decision struct {
	Kind    Kind
	URL     string
	DataURI bool
	Detail  string
	FileID  string
}

// Inspect 根据 image_url / file_id / detail 生成判定。detail 原样传递不做校验；
// image_url 非空即 KindMapped，否则 file_id 非空即 KindFileID，否则 KindMalformed。
func Inspect(imageURL, fileID oparam.Opt[string], detail string) Decision {
	if imageURL.Valid() && imageURL.Value != "" {
		return Decision{
			Kind:    KindMapped,
			URL:     imageURL.Value,
			DataURI: hasDataPrefix(imageURL.Value),
			Detail:  detail,
		}
	}
	if fileID.Valid() && fileID.Value != "" {
		return Decision{Kind: KindFileID, FileID: fileID.Value, Detail: detail}
	}
	return Decision{Kind: KindMalformed, Detail: detail}
}

// InspectParam 从用户消息的 input_image 提取字段后委托 Inspect。
func InspectParam(img *oairesponses.ResponseInputImageParam) Decision {
	if img == nil {
		return Decision{Kind: KindMalformed}
	}
	return Inspect(img.ImageURL, img.FileID, string(img.Detail))
}

// InspectContentParam 从工具结果 content 的 input_image 提取字段后委托 Inspect。
func InspectContentParam(img *oairesponses.ResponseInputImageContentParam) Decision {
	if img == nil {
		return Decision{Kind: KindMalformed}
	}
	return Inspect(img.ImageURL, img.FileID, string(img.Detail))
}
