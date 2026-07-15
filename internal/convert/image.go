// Package convert maps OpenAI Responses request fields to Anthropic Messages fields.
package convert

import (
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
)

func imageBlock(url string) anthropic.ContentBlockParamUnion {
	if isDataURI(url) {
		media, data := splitDataURI(url)
		return anthropic.ContentBlockParamUnion{
			OfImage: &anthropic.ImageBlockParam{
				Source: anthropic.ImageBlockParamSourceUnion{
					OfBase64: &anthropic.Base64ImageSourceParam{
						MediaType: anthropic.Base64ImageSourceMediaType(media),
						Data:      data,
					},
				},
			},
		}
	}
	return anthropic.ContentBlockParamUnion{
		OfImage: &anthropic.ImageBlockParam{
			Source: anthropic.ImageBlockParamSourceUnion{
				OfURL: &anthropic.URLImageSourceParam{URL: url},
			},
		},
	}
}

func isDataURI(s string) bool {
	return strings.HasPrefix(s, "data:")
}

func splitDataURI(s string) (mediaType, data string) {
	// data:image/png;base64,XXXX
	s = strings.TrimPrefix(s, "data:")
	semi := strings.Index(s, ",")
	if semi < 0 {
		return "application/octet-stream", s
	}
	mediaType = s[:semi]
	if i := strings.Index(mediaType, ";"); i >= 0 {
		mediaType = mediaType[:i]
	}
	return mediaType, s[semi+1:]
}
