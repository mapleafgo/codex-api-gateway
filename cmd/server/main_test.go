package main

import "testing"

func TestCodexBaseURL(t *testing.T) {
	cases := []struct {
		listen string
		want   string
	}{
		{":8383", "http://localhost:8383/v1"},
		{"127.0.0.1:9870", "http://127.0.0.1:9870/v1"},
		{"0.0.0.0:8383", "http://localhost:8383/v1"},
	}
	for _, tc := range cases {
		if got := codexBaseURL(tc.listen); got != tc.want {
			t.Errorf("codexBaseURL(%q)=%q want %q", tc.listen, got, tc.want)
		}
	}
}
