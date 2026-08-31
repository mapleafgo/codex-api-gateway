package imagemapper

import (
	"testing"

	oparam "github.com/openai/openai-go/v3/packages/param"
	oairesponses "github.com/openai/openai-go/v3/responses"
)

func TestInspect(t *testing.T) {
	urlOpt := func(s string) oparam.Opt[string] {
		return oparam.NewOpt(s)
	}
	cases := []struct {
		name     string
		imageURL oparam.Opt[string]
		fileID   oparam.Opt[string]
		detail   string
		want     Decision
	}{
		{
			name:     "url mapped",
			imageURL: urlOpt("https://example.com/a.png"),
			detail:   "high",
			want:     Decision{Kind: KindMapped, URL: "https://example.com/a.png", Detail: "high"},
		},
		{
			name:     "data uri mapped",
			imageURL: urlOpt("data:image/png;base64,AAEC"),
			detail:   "low",
			want:     Decision{Kind: KindMapped, URL: "data:image/png;base64,AAEC", DataURI: true, Detail: "low"},
		},
		{
			name:     "url wins over file id",
			imageURL: urlOpt("https://example.com/a.png"),
			fileID:   urlOpt("file_1"),
			detail:   "auto",
			want:     Decision{Kind: KindMapped, URL: "https://example.com/a.png", Detail: "auto"},
		},
		{
			name:   "file id only",
			fileID: urlOpt("file_abc"),
			detail: "original",
			want:   Decision{Kind: KindFileID, FileID: "file_abc", Detail: "original"},
		},
		{
			name:   "malformed no url no file id",
			detail: "auto",
			want:   Decision{Kind: KindMalformed, Detail: "auto"},
		},
		{
			name:     "empty strings malformed",
			imageURL: urlOpt(""),
			fileID:   urlOpt(""),
			want:     Decision{Kind: KindMalformed},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Inspect(tc.imageURL, tc.fileID, tc.detail)
			if got != tc.want {
				t.Fatalf("Inspect()=%+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestInspectParam(t *testing.T) {
	img := &oairesponses.ResponseInputImageParam{
		Detail:   oairesponses.ResponseInputImageDetailOriginal,
		ImageURL: oparam.NewOpt("https://example.com/a.png"),
	}
	got := InspectParam(img)
	if got.Kind != KindMapped || got.URL != "https://example.com/a.png" || got.Detail != "original" {
		t.Fatalf("InspectParam()=%+v", got)
	}
}

func TestInspectContentParam(t *testing.T) {
	img := &oairesponses.ResponseInputImageContentParam{
		Detail:   oairesponses.ResponseInputImageContentDetailHigh,
		FileID:   oparam.NewOpt("file_abc"),
		ImageURL: oparam.Opt[string]{},
	}
	got := InspectContentParam(img)
	if got.Kind != KindFileID || got.FileID != "file_abc" || got.Detail != "high" {
		t.Fatalf("InspectContentParam()=%+v", got)
	}
}

func TestInspectNil(t *testing.T) {
	if got := InspectParam(nil); got.Kind != KindMalformed {
		t.Fatalf("InspectParam(nil)=%+v", got)
	}
	if got := InspectContentParam(nil); got.Kind != KindMalformed {
		t.Fatalf("InspectContentParam(nil)=%+v", got)
	}
}
