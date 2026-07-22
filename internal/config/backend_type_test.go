package config

import "testing"

func TestNormalizeBackendType(t *testing.T) {
	cases := []struct {
		in, want string
		ok       bool
	}{
		{"", "a", true},
		{"a", "a", true},
		{"c", "c", true},
		{" a ", "a", true},
		{" c ", "c", true},
		{"anthropic", "", false},
		{"openai_chat", "", false},
		{"x", "", false},
	}
	for _, tc := range cases {
		got, err := NormalizeBackendType(tc.in)
		if tc.ok {
			if err != nil || got != tc.want {
				t.Fatalf("in=%q got=%q err=%v want=%q", tc.in, got, err, tc.want)
			}
		} else {
			if err == nil {
				t.Fatalf("in=%q expected error", tc.in)
			}
		}
	}
}

func TestValidateRejectsUnknownBackendType(t *testing.T) {
	cfg := Config{
		Sources: []Source{{
			Name:        "test",
			BaseURL:     "https://example.com",
			BackendType: "invalid",
		}},
	}
	if err := cfg.validate(); err == nil {
		t.Fatalf("expected error for invalid backend_type, got nil")
	}
}

func TestValidateAcceptsBoth(t *testing.T) {
	cases := []string{"a", "c", ""}
	for _, bt := range cases {
		cfg := Config{
			Sources: []Source{{
				Name:        "test",
				BaseURL:     "https://example.com",
				BackendType: bt,
			}},
		}
		if err := cfg.validate(); err != nil {
			t.Fatalf("backend_type=%q: validate failed: %v", bt, err)
		}
	}
}
