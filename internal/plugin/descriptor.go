package plugin

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/mapleafgo/codex-api-gateway/internal/config"
)

// ID 是源插件的稳定标识，进入配置 `backend` 字段、指标与日志。
type ID string

// Capability 描述源插件可承载的协议能力，而不是具体厂商身份。
type Capability string

const (
	CapabilityAnthropicMessages    Capability = "anthropic-messages"
	CapabilityChatCompletions      Capability = "chat-completions"
	CapabilityResponsesPassthrough Capability = "responses-passthrough"
)

// StreamingKind 描述上游流式结果与 Responses SSE 的关系。
type StreamingKind string

const (
	// StreamingConverted 表示上游事件需经共享转换引擎转回 Responses SSE，
	// 首个内容事件出现前需要 EventGate 缓冲非内容事件。
	StreamingConverted StreamingKind = "converted"
	// StreamingPassthrough 表示上游本身就是 Responses SSE，直接透传，不参与 EventGate。
	StreamingPassthrough StreamingKind = "passthrough"
)

// FieldType 是管理页表单字段的声明式类型。
type FieldType string

const (
	FieldTypeText      FieldType = "text"
	FieldTypePassword  FieldType = "password"
	FieldTypeBoolean   FieldType = "boolean"
	FieldTypeInteger   FieldType = "integer"
	FieldTypeSelect    FieldType = "select"
	FieldTypeStringMap FieldType = "string-map"
)

// FieldTarget 声明字段落在源配置的哪一层，共享核心不解释具体字段名。
type FieldTarget string

const (
	FieldTargetCommon  FieldTarget = "common"
	FieldTargetBaseURL FieldTarget = "base_url"
	FieldTargetAPIKey  FieldTarget = "api_key"
	FieldTargetOption  FieldTarget = "options"
)

// Field 描述插件 schema 中的一个可配置字段。
type Field struct {
	Name        string
	Label       string
	Description string
	Type        FieldType
	Required    bool
	Default     any
	Sensitive   bool
	Options     []string
	Target      FieldTarget
}

// ActionRoute 声明一个管理动作可执行的 HTTP 路由；Method 只能是 GET/POST，
// Path 相对 /admin/api/source-plugins/<id>/<action> 挂载。
type ActionRoute struct {
	ID     string
	Method string
	Path   string
}

// Action 是源插件提供给管理页的扩展操作的元数据。
type Action struct {
	ID     string
	Label  string
	Kind   string
	Routes []ActionRoute
}

// ActionKindDeviceCodeStatus 是 Copilot 插件公开 Device Flow 状态的标准动作。
const ActionKindDeviceCodeStatus = "device_code_status"

// Descriptor 是源插件能力的声明式元数据，注册后不可变。
type Descriptor struct {
	ID           ID
	Title        string
	Summary      string
	Capabilities []Capability
	Streaming    StreamingKind
	Schema       []Field
	Actions      []Action
}

// Validate 校验 Descriptor 的结构性约束，供 Registry 构造时调用。
// 不解释 schema 之外的业务语义。
func (d Descriptor) Validate() error {
	if d.ID == "" {
		return fmt.Errorf("plugin descriptor: id is required")
	}
	if strings.TrimSpace(d.Title) == "" {
		return fmt.Errorf("plugin descriptor %q: title is required", d.ID)
	}
	if d.Streaming != StreamingConverted && d.Streaming != StreamingPassthrough {
		return fmt.Errorf("plugin descriptor %q: unknown streaming kind %q", d.ID, d.Streaming)
	}
	seenCap := make(map[Capability]bool, len(d.Capabilities))
	for _, c := range d.Capabilities {
		switch c {
		case CapabilityResponsesPassthrough, CapabilityChatCompletions, CapabilityAnthropicMessages:
		default:
			return fmt.Errorf("plugin descriptor %q: unknown capability %q", d.ID, c)
		}
		if seenCap[c] {
			return fmt.Errorf("plugin descriptor %q: duplicate capability %q", d.ID, c)
		}
		seenCap[c] = true
	}
	seenField := make(map[string]bool, len(d.Schema))
	for _, f := range d.Schema {
		if f.Name == "" || seenField[f.Name] {
			return fmt.Errorf("plugin descriptor %q: field name must be unique and non-empty", d.ID)
		}
		seenField[f.Name] = true
		switch f.Type {
		case FieldTypeText, FieldTypePassword, FieldTypeBoolean, FieldTypeInteger, FieldTypeSelect, FieldTypeStringMap:
		default:
			return fmt.Errorf("plugin descriptor %q: field %s: unknown type %q", d.ID, f.Name, f.Type)
		}
		switch f.Target {
		case FieldTargetCommon, FieldTargetBaseURL, FieldTargetAPIKey, FieldTargetOption:
		default:
			return fmt.Errorf("plugin descriptor %q: field %s: unknown target %q", d.ID, f.Name, f.Target)
		}
		if f.Sensitive && f.Target != FieldTargetOption {
			return fmt.Errorf("plugin descriptor %q: field %s: sensitive fields must target options", d.ID, f.Name)
		}
	}
	seenAction := make(map[string]bool, len(d.Actions))
	for _, a := range d.Actions {
		if a.ID == "" || seenAction[a.ID] {
			return fmt.Errorf("plugin descriptor %q: duplicate action id", d.ID)
		}
		seenAction[a.ID] = true
		if len(a.Routes) == 0 {
			return fmt.Errorf("plugin descriptor %q: action %s: at least one route required", d.ID, a.ID)
		}
		for _, r := range a.Routes {
			if r.Method != http.MethodGet && r.Method != http.MethodPost {
				return fmt.Errorf("plugin descriptor %q: action %s: route method must be GET or POST", d.ID, a.ID)
			}
			if !strings.HasPrefix(r.Path, "/") {
				return fmt.Errorf("plugin descriptor %q: action %s: route path must start with /", d.ID, a.ID)
			}
			if r.ID == "" {
				return fmt.Errorf("plugin descriptor %q: action %s: route id required", d.ID, a.ID)
			}
		}
	}
	return nil
}

// validateOptions 拒绝 schema 之外的 option key。
func (d Descriptor) validateOptions(src config.Source) error {
	allowed := make(map[string]bool, len(d.Schema))
	for _, f := range d.Schema {
		if f.Target == FieldTargetOption {
			allowed[f.Name] = true
		}
	}
	for k := range src.Options {
		if !allowed[k] {
			return fmt.Errorf("source %q: options.%s: unknown option for backend %s", src.Name, k, d.ID)
		}
	}
	return nil
}
