// Package logging 配置进程级结构化日志。
package logging

import (
	"context"
	"fmt"
	"io"
	"log"
	"log/slog"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mapleafgo/codex-api-gateway/internal/config"
)

// Configure 将配置指定的 slog handler 安装为进程默认 logger。
func Configure(cfg config.LoggingCfg) {
	handler := NewHandler(os.Stderr, cfg)
	slog.SetDefault(slog.New(handler))
	log.SetOutput(io.Discard)
}

// NewHandler 根据日志等级和格式返回 slog handler。
func NewHandler(out io.Writer, cfg config.LoggingCfg) slog.Handler {
	opts := &slog.HandlerOptions{Level: slogLevel(cfg.Level)}
	if cfg.Format == "json" {
		return slog.NewJSONHandler(out, opts)
	}
	return newReadableTextHandler(out, opts.Level)
}

func slogLevel(level string) slog.Level {
	switch level {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

type readableTextHandler struct {
	out    io.Writer
	level  slog.Leveler
	attrs  []slog.Attr
	groups []string
	mu     *sync.Mutex
}

func newReadableTextHandler(out io.Writer, level slog.Leveler) slog.Handler {
	if level == nil {
		level = slog.LevelInfo
	}
	return &readableTextHandler{
		out:   out,
		level: level,
		mu:    &sync.Mutex{},
	}
}

func (h *readableTextHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.level.Level()
}

func (h *readableTextHandler) Handle(_ context.Context, r slog.Record) error {
	var b strings.Builder
	b.WriteString(r.Time.Local().Format("2006-01-02 15:04:05.000"))
	b.WriteByte(' ')
	b.WriteString(formatLevel(r.Level))
	b.WriteByte(' ')
	b.WriteString(r.Message)

	for _, attr := range h.attrs {
		appendAttr(&b, h.groups, attr)
	}
	r.Attrs(func(attr slog.Attr) bool {
		appendAttr(&b, h.groups, attr)
		return true
	})
	b.WriteByte('\n')

	h.mu.Lock()
	defer h.mu.Unlock()
	_, err := io.WriteString(h.out, b.String())
	return err
}

func (h *readableTextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	next := h.clone()
	next.attrs = append(next.attrs, attrs...)
	return next
}

func (h *readableTextHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	next := h.clone()
	next.groups = append(next.groups, name)
	return next
}

func (h *readableTextHandler) clone() *readableTextHandler {
	next := *h
	next.attrs = slices.Clone(h.attrs)
	next.groups = slices.Clone(h.groups)
	return &next
}

func appendAttr(b *strings.Builder, groups []string, attr slog.Attr) {
	attr.Value = attr.Value.Resolve()
	if attr.Equal(slog.Attr{}) {
		return
	}
	if attr.Value.Kind() == slog.KindGroup {
		appendGroup(b, groups, attr)
		return
	}
	key := attr.Key
	if len(groups) > 0 {
		key = strings.Join(append(slices.Clone(groups), key), ".")
	}
	b.WriteByte(' ')
	b.WriteString(key)
	b.WriteByte('=')
	b.WriteString(formatValue(attr.Value))
}

func appendGroup(b *strings.Builder, groups []string, attr slog.Attr) {
	nextGroups := append(slices.Clone(groups), attr.Key)
	for _, child := range attr.Value.Group() {
		appendAttr(b, nextGroups, child)
	}
}

func formatLevel(level slog.Level) string {
	switch {
	case level <= slog.LevelDebug:
		return "DEBUG"
	case level < slog.LevelWarn:
		return "INFO"
	case level < slog.LevelError:
		return "WARN"
	default:
		return "ERROR"
	}
}

func formatValue(v slog.Value) string {
	switch v.Kind() {
	case slog.KindString:
		return quoteIfNeeded(v.String())
	case slog.KindTime:
		return v.Time().Local().Format(time.RFC3339)
	case slog.KindDuration:
		return v.Duration().String()
	default:
		return quoteIfNeeded(fmt.Sprint(v.Any()))
	}
}

func quoteIfNeeded(s string) string {
	if s == "" || strings.ContainsAny(s, " \t\n\r\"=") {
		return strconv.Quote(s)
	}
	return s
}
