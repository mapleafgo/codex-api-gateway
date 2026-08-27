package admin

import (
	"net/http"
)

// sourcePluginView 是管理页用来按能力渲染源表单的插件描述符 JSON 视图。
// 只包含声明式元数据，绝不包含任何明文凭据。
type sourcePluginView struct {
	ID           string             `json:"id"`
	Title        string             `json:"title"`
	Summary      string             `json:"summary"`
	Capabilities []string           `json:"capabilities"`
	Streaming    string             `json:"streaming"`
	Schema       []pluginFieldView  `json:"schema"`
	Actions      []pluginActionView `json:"actions"`
}

// pluginFieldView 是单个配置字段的描述，不含值。
type pluginFieldView struct {
	Name        string   `json:"name"`
	Label       string   `json:"label"`
	Description string   `json:"description,omitempty"`
	Type        string   `json:"type"`
	Required    bool     `json:"required"`
	Sensitive   bool     `json:"sensitive,omitempty"`
	Target      string   `json:"target"`
	Default     any      `json:"default,omitempty"`
	Options     []string `json:"options,omitempty"`
}

// pluginActionView 是源插件声明式管理动作。
type pluginActionView struct {
	ID     string            `json:"id"`
	Label  string            `json:"label"`
	Kind   string            `json:"kind"`
	Routes []pluginRouteView `json:"routes"`
}

// pluginRouteView 是动作的可执行路由声明。
type pluginRouteView struct {
	ID     string `json:"id"`
	Method string `json:"method"`
	Path   string `json:"path"`
}

// handleSourcePlugins 返回已注册源插件描述符列表（按 ID 升序，稳定顺序），
// 供管理页按能力动态渲染表单，而不是为每个内置源写死分支。
func (h *handler) handleSourcePlugins(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, errorBody{Error: "method not allowed"})
		return
	}
	if h.deps.Registry == nil {
		writeJSON(w, http.StatusOK, []sourcePluginView{})
		return
	}
	out := make([]sourcePluginView, 0, 4)
	for _, desc := range h.deps.Registry.Descriptors() {
		view := sourcePluginView{
			ID:           string(desc.ID),
			Title:        desc.Title,
			Summary:      desc.Summary,
			Capabilities: make([]string, 0, len(desc.Capabilities)),
			Streaming:    string(desc.Streaming),
			Schema:       make([]pluginFieldView, 0, len(desc.Schema)),
			Actions:      make([]pluginActionView, 0, len(desc.Actions)),
		}
		for _, c := range desc.Capabilities {
			view.Capabilities = append(view.Capabilities, string(c))
		}
		for _, f := range desc.Schema {
			view.Schema = append(view.Schema, pluginFieldView{
				Name: f.Name, Label: f.Label, Description: f.Description,
				Type: string(f.Type), Required: f.Required, Sensitive: f.Sensitive,
				Target: string(f.Target), Default: f.Default, Options: f.Options,
			})
		}
		for _, a := range desc.Actions {
			av := pluginActionView{ID: a.ID, Label: a.Label, Kind: a.Kind}
			for _, rt := range a.Routes {
				av.Routes = append(av.Routes, pluginRouteView{ID: rt.ID, Method: rt.Method, Path: rt.Path})
			}
			view.Actions = append(view.Actions, av)
		}
		out = append(out, view)
	}
	writeJSON(w, http.StatusOK, out)
}
