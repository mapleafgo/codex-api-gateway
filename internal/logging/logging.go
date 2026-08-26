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
	"github.com/mapleafgo/logrotate"
)

// 日志 sink 全局状态：进程生命周期内只创建一次 AsyncWriter，
// 热重载时复用同一 sink，仅重建 slog handler（等级/格式），
// 避免关闭旧 writer 导致在途请求（捕获了旧 handler 的 logger）日志静默丢失。
var (
	configureMu sync.Mutex
	currentFile string // 当前日志文件路径；空表示 stderr
	currentSink io.Writer
	currentAW   *logrotate.AsyncWriter // 文件模式下的异步 writer；stderr 模式为 nil
)

// Configure 将配置指定的 slog handler 安装为进程默认 logger。
// File 非空时日志写入该文件（经 logrotate 按大小滚动 + 异步刷盘）；为空则写 stderr。
// 重复调用（热重载场景）复用同一 AsyncWriter，仅重建 handler 以应用新的等级/格式，
// 不关闭旧 writer——在途请求可能仍持有旧 handler 引用，关闭会导致其日志写入失败被静默吞掉。
// 仅当日志文件路径发生变化时才创建新的 AsyncWriter（旧的不关闭，由 GC 回收 fd）。
func Configure(cfg config.LoggingCfg) error {
	configureMu.Lock()
	defer configureMu.Unlock()

	if cfg.File != currentFile || currentSink == nil {
		// 文件路径变化（含首次配置、file↔stderr 切换）：创建新 sink。
		// 旧 AsyncWriter 不关闭——其 Close 会拒绝新写入，而捕获了旧 handler 的
		// 在途请求仍可能向其 Write；让旧 writer 自然排空队列后由 GC 回收 fd。
		if cfg.File != "" {
			aw, err := openAsyncLogWriter(cfg)
			if err != nil {
				return err
			}
			currentAW = aw
			currentSink = aw
		} else {
			currentAW = nil
			currentSink = os.Stderr
		}
		currentFile = cfg.File
	}

	handler := NewHandler(currentSink, cfg)
	slog.SetDefault(slog.New(handler))
	log.SetOutput(io.Discard)
	return nil
}

// openAsyncLogWriter 创建 logrotate Writer 并包装为 AsyncWriter。
// FullDropNewest 策略保证队列满时不阻塞转发热路径，仅丢弃当前日志条目。
func openAsyncLogWriter(cfg config.LoggingCfg) (*logrotate.AsyncWriter, error) {
	w, err := logrotate.Open(logrotate.Config{
		Filename:      cfg.File,
		MaxFileSizeMB: cfg.MaxSizeMB,
		MaxBackups:    cfg.MaxBackups,
	})
	if err != nil {
		return nil, err
	}
	return logrotate.OpenAsync(w, logrotate.AsyncConfig{
		QueueSize:  8192,
		FullPolicy: logrotate.FullDropNewest,
	})
}

// Close 刷盘并关闭当前日志 writer。供进程优雅关闭调用，确保退出前日志落盘。
// stderr 模式为 no-op。重复调用安全。
func Close() error {
	configureMu.Lock()
	defer configureMu.Unlock()
	if currentAW != nil {
		err := currentAW.Close()
		currentAW = nil
		return err
	}
	return nil
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
