package autostart

import "testing"

func TestValidate(t *testing.T) {
	t.Parallel()
	ok := Spec{AppID: "codex-api-gateway", DisplayName: "X", Exec: "/bin/x"}
	if err := ok.validate(); err != nil {
		t.Fatalf("合法 Spec 应通过: %v", err)
	}
	cases := []Spec{
		{DisplayName: "X", Exec: "/bin/x"},
		{AppID: "bad/id", DisplayName: "X", Exec: "/bin/x"},
		{AppID: "a", DisplayName: "X"},
		{AppID: "a", Exec: "/bin/x"},
	}
	for _, c := range cases {
		if err := c.validate(); err == nil {
			t.Errorf("应拒绝 %+v", c)
		}
	}
}

func TestJoinQuoted(t *testing.T) {
	t.Parallel()
	got := joinQuoted([]string{`/opt/app`, `-config`, `/path with space/config.yaml`})
	want := `/opt/app -config "/path with space/config.yaml"`
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	got = joinQuoted([]string{`C:\Program Files\app.exe`, `-config`, `D:\c.yaml`})
	if got != `"C:\Program Files\app.exe" -config D:\c.yaml` {
		t.Fatalf("windows path quote: %q", got)
	}
}

func TestCommandLine(t *testing.T) {
	t.Parallel()
	s := Spec{Exec: "/usr/bin/app", Args: []string{"-config", "/tmp/c.yaml"}}
	if s.commandLine() != "/usr/bin/app -config /tmp/c.yaml" {
		t.Fatalf("commandLine=%q", s.commandLine())
	}
}
