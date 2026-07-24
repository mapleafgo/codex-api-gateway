package logging

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"
)

type requestIDKey struct{}

var requestIDFallbackCounter atomic.Uint64

// NewRequestID 返回用于单次网关请求日志关联的 24 位十六进制标识。
func NewRequestID() string {
	var raw [12]byte
	if _, err := rand.Read(raw[:]); err == nil {
		return hex.EncodeToString(raw[:])
	}
	seed := fmt.Sprintf("%d:%d", time.Now().UnixNano(), requestIDFallbackCounter.Add(1))
	sum := sha256.Sum256([]byte(seed))
	return hex.EncodeToString(sum[:12])
}

// WithRequestID 把非空请求标识写入 ctx；空标识不改变原 context。
func WithRequestID(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, requestIDKey{}, id)
}

// RequestID 返回 ctx 中的请求标识；未设置时返回空字符串。
func RequestID(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

// FromContext 返回携带 request_id 的默认 logger；ctx 未设置标识时返回默认 logger。
func FromContext(ctx context.Context) *slog.Logger {
	if id := RequestID(ctx); id != "" {
		return slog.Default().With("request_id", id)
	}
	return slog.Default()
}
