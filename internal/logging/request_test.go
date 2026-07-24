package logging

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/mapleafgo/codex-api-gateway/internal/config"
)

func TestNewRequestID(t *testing.T) {
	first := NewRequestID()
	second := NewRequestID()
	if len(first) != 24 || len(second) != 24 {
		t.Fatalf("request id lengths=%d/%d want 24", len(first), len(second))
	}
	if first == second {
		t.Fatalf("request ids must be unique: %q", first)
	}
}

func TestRequestIDContextRoundTrip(t *testing.T) {
	ctx := WithRequestID(context.Background(), "req-1")
	if got := RequestID(ctx); got != "req-1" {
		t.Fatalf("RequestID=%q want req-1", got)
	}
}

func TestFromContextIncludesRequestID(t *testing.T) {
	var out bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(NewHandler(&out, config.LoggingCfg{Level: "debug", Format: "json"})))
	t.Cleanup(func() { slog.SetDefault(previous) })

	FromContext(WithRequestID(context.Background(), "req-1")).Info("marker")

	if got := out.String(); !strings.Contains(got, `"request_id":"req-1"`) {
		t.Fatalf("log=%s want request_id", got)
	}
}
