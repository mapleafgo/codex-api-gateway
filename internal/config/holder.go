// Package config 加载、校验 YAML 配置，并提供 *Config 的热重载持有者。
//
// 设计目标：
//   - API 请求路径调用 Current() 拿到最新配置指针，全程无锁（atomic load）。
//   - 管理端（或 fsnotify）调用 Replace() 原子替换配置，下一个请求自动用新配置。
//   - Scheduler 与 Server 持有 *Holder 而非 *Config，支持热替换。
package config

import (
	"sync/atomic"
)

// Holder 持有当前生效的 *Config，支持热替换。
type Holder struct {
	p atomic.Pointer[Config]
}

// NewHolder 包装一个初始 Config。
func NewHolder(cfg *Config) *Holder {
	h := &Holder{}
	h.p.Store(cfg)
	return h
}

// Current 返回当前生效的 Config 指针。永远非 nil。
func (h *Holder) Current() *Config {
	return h.p.Load()
}

// Replace 原子替换当前 Config。传入 nil 会被忽略（保护 API 拿到空指针）。
func (h *Holder) Replace(cfg *Config) {
	if cfg == nil {
		return
	}
	h.p.Store(cfg)
}
