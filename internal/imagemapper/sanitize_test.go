package imagemapper

import (
	"strings"
	"testing"
)

func TestSanitizeURL(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "strips query and fragment",
			in:   "https://host/p.png?sig=abc&token=xyz#frag",
			want: "https://host/p.png",
		},
		{
			name: "keeps path with percent encoding",
			in:   "https://host/a%20b/c.png?x=1",
			want: "https://host/a%20b/c.png",
		},
		{
			name: "keeps plain url",
			in:   "https://example.com/a.png",
			want: "https://example.com/a.png",
		},
		{
			name: "data uri metadata only",
			in:   "data:image/png;base64,AAAA",
			want: "data:image/png;base64,<bytes=3>",
		},
		{
			name: "data uri jpeg metadata",
			in:   "data:image/jpeg;base64,AA==",
			want: "data:image/jpeg;base64,<bytes=1>",
		},
		{
			name: "data uri without comma",
			in:   "data:image/png",
			want: "data:<unknown>",
		},
		{
			name: "malformed url untouched",
			in:   "http://[::1",
			want: "http://[::1",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := SanitizeURL(tc.in)
			if got != tc.want {
				t.Fatalf("SanitizeURL(%q)=%q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestSanitizeURL_NoCredentialLeak(t *testing.T) {
	raw := "https://host/p.png?sig=SECRET_TOKEN"
	got := SanitizeURL(raw)
	if strings.Contains(got, "SECRET_TOKEN") {
		t.Fatalf("SanitizeURL leaked query param: %q", got)
	}

	data := "data:image/png;base64," + strings.Repeat("A", 128)
	gotData := SanitizeURL(data)
	if strings.Contains(gotData, strings.Repeat("A", 8)) {
		t.Fatalf("SanitizeURL leaked data uri payload: %q", gotData)
	}
	if !strings.Contains(gotData, "<bytes=") {
		t.Fatalf("SanitizeURL missing byte metadata: %q", gotData)
	}
}
