package plugin

import (
	"fmt"
	"slices"

	"github.com/mapleafgo/codex-api-gateway/internal/config"
)

// SourcePlugin 是内置或第三方源在网关中的统一边界。
type SourcePlugin interface {
	Descriptor() Descriptor
	ValidateSource(config.Source) error
	Backend() Backend
}

// Registry 是从插件构造完成即不可变的注册表，读取无需锁。
type Registry struct {
	byID        map[ID]SourcePlugin
	descriptors []Descriptor
}

// New 校验并收集插件，返回不可变注册表。nil Backend、空/重复 ID、非法
// Descriptor 都会导致构造失败。
func New(plugins ...SourcePlugin) (*Registry, error) {
	byID := make(map[ID]SourcePlugin, len(plugins))
	descs := make([]Descriptor, 0, len(plugins))
	for _, p := range plugins {
		if p == nil {
			return nil, fmt.Errorf("plugin registry: nil plugin")
		}
		d := p.Descriptor()
		if err := d.Validate(); err != nil {
			return nil, err
		}
		if _, dup := byID[d.ID]; dup {
			return nil, fmt.Errorf("plugin registry: duplicate plugin id %q", d.ID)
		}
		if p.Backend() == nil {
			return nil, fmt.Errorf("plugin registry: plugin %q has nil backend", d.ID)
		}
		byID[d.ID] = p
		descs = append(descs, d)
	}
	slices.SortFunc(descs, func(a, b Descriptor) int {
		switch {
		case a.ID < b.ID:
			return -1
		case a.ID > b.ID:
			return 1
		default:
			return 0
		}
	})
	return &Registry{byID: byID, descriptors: descs}, nil
}

// Get 按稳定插件 ID 查询插件，未注册时返回 false。
func (r *Registry) Get(id string) (SourcePlugin, bool) {
	if r == nil {
		return nil, false
	}
	p, ok := r.byID[ID(id)]
	return p, ok
}

// Descriptors 返回按 ID 升序排列的只读描述符副本。
func (r *Registry) Descriptors() []Descriptor {
	if r == nil {
		return nil
	}
	return slices.Clone(r.descriptors)
}

// ValidateSource 按契约顺序校验 source：backend 已注册、name 非空、
// schema 外 option 拒绝、最后委托插件自身校验。
func (r *Registry) ValidateSource(src config.Source) error {
	if r == nil {
		return fmt.Errorf("source %q: registry is nil", src.Name)
	}
	if src.Name == "" {
		return fmt.Errorf("source: name is required")
	}
	p, ok := r.Get(src.Backend)
	if !ok {
		return fmt.Errorf("source %q: unknown backend %q; registered backends: %s",
			src.Name, src.Backend, r.RegisteredBackends())
	}
	if err := p.Descriptor().validateOptions(src); err != nil {
		return err
	}
	if err := p.ValidateSource(src); err != nil {
		return fmt.Errorf("source %q: %w", src.Name, err)
	}
	return nil
}

// RegisteredBackends 返回按 ID 升序排列的已注册 backend 列表，用于错误提示。
func (r *Registry) RegisteredBackends() string {
	ids := make([]string, 0, len(r.descriptors))
	for _, d := range r.descriptors {
		ids = append(ids, string(d.ID))
	}
	return joinString(ids)
}

func joinString(parts []string) string {
	out := ""
	for i, s := range parts {
		if i > 0 {
			out += ", "
		}
		out += s
	}
	return out
}
