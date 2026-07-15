package logging

import (
	"bytes"
	"log/slog"
	"regexp"
	"strings"
	"testing"

	"github.com/mapleafgo/codex-api-gateway/internal/config"
)

func TestNewHandlerFiltersByLevel(t *testing.T) {
	var out bytes.Buffer
	logger := slog.New(NewHandler(&out, config.LoggingCfg{Level: "warn", Format: "text"}))

	logger.Info("hidden")
	logger.Warn("visible")

	got := out.String()
	if strings.Contains(got, "hidden") {
		t.Fatalf("info log should be filtered at warn level: %s", got)
	}
	if !strings.Contains(got, "visible") {
		t.Fatalf("warn log should be emitted: %s", got)
	}
}

func TestNewHandlerUsesReadableTextFormat(t *testing.T) {
	var out bytes.Buffer
	logger := slog.New(NewHandler(&out, config.LoggingCfg{Level: "debug", Format: "text"}))

	logger.Info("收到响应请求", "method", "POST", "path", "/v1/responses", "output_types", []string{"message", "reasoning"})

	got := out.String()
	if strings.Contains(got, "time=") || strings.Contains(got, "msg=") || strings.Contains(got, "level=") {
		t.Fatalf("text handler should not use default slog key=value prefix fields: %s", got)
	}
	if !regexp.MustCompile(`^\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}\.\d{3} `).MatchString(got) {
		t.Fatalf("text handler should include local date and time: %s", got)
	}
	if !strings.Contains(got, "INFO 收到响应请求") {
		t.Fatalf("text handler should put level and message first: %s", got)
	}
	if !strings.Contains(got, "method=POST") || !strings.Contains(got, "path=/v1/responses") {
		t.Fatalf("text handler missing attrs: %s", got)
	}
	if !strings.Contains(got, `output_types="[message reasoning]"`) {
		t.Fatalf("text handler should quote values with spaces: %s", got)
	}
}

func TestNewHandlerTextFormatHandlesGroups(t *testing.T) {
	var out bytes.Buffer
	logger := slog.New(NewHandler(&out, config.LoggingCfg{Level: "debug", Format: "text"})).WithGroup("request")

	logger.Info("转换完成", "model", "gpt-5", slog.Group("source", "name", "s1"))

	got := out.String()
	if !strings.Contains(got, "request.model=gpt-5") {
		t.Fatalf("grouped attr missing prefix: %s", got)
	}
	if !strings.Contains(got, "request.source.name=s1") {
		t.Fatalf("nested grouped attr missing prefix: %s", got)
	}
	if strings.Contains(got, "request.request.") {
		t.Fatalf("group prefix repeated: %s", got)
	}
}

func TestNewHandlerUsesJSONFormat(t *testing.T) {
	var out bytes.Buffer
	logger := slog.New(NewHandler(&out, config.LoggingCfg{Level: "debug", Format: "json"}))

	logger.Debug("structured", "source", "s1")

	got := out.String()
	if !strings.HasPrefix(got, "{") {
		t.Fatalf("json handler should emit JSON object, got: %s", got)
	}
	if !strings.Contains(got, `"level":"DEBUG"`) || !strings.Contains(got, `"source":"s1"`) {
		t.Fatalf("json handler missing structured fields: %s", got)
	}
}
