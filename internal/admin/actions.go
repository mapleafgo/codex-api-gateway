package admin

// actions.go 实现通用 AdminExtension 动作路由：从插件 Descriptor 读取声明的
// method/path 对，挂载到 mux 并分发到 plugin.InvokeAction。共享 admin 不为
// 任何具体源写分支。

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"github.com/mapleafgo/codex-api-gateway/internal/config"
	"github.com/mapleafgo/codex-api-gateway/internal/plugin"
)

// mountActions 从 Registry 读取所有声明了 Actions 的插件，注入配置回调并
// 挂载它们的 action 路由。路由冲突在挂载阶段直接 panic（组装期编程错误）。
func (h *handler) mountActions(mux *http.ServeMux, wrap func(string, http.HandlerFunc) http.HandlerFunc) {
	if h.deps.Registry == nil {
		return
	}
	claimed := map[string]plugin.ActionRoute{}
	for _, desc := range h.deps.Registry.Descriptors() {
		p, ok := h.deps.Registry.Get(string(desc.ID))
		if !ok {
			continue
		}
		// 注入配置回调给需要它的插件。
		if inj, ok := p.(plugin.CallbackInjector); ok {
			inj.InjectCallbacks(plugin.AdminCallbacks{
				Snapshot: func() *config.Config { return h.deps.Holder.Current() },
				Write:    h.persistConfig,
			})
		}
		ext, ok := p.(plugin.AdminExtension)
		if !ok {
			continue
		}
		for _, action := range desc.Actions {
			for _, route := range action.Routes {
				key := route.Method + " " + route.Path
				if prev, dup := claimed[key]; dup {
					panic(fmt.Sprintf("admin: action route conflict %s (plugin %s vs %s)", key, prev.ID, route.ID))
				}
				claimed[key] = route
				mux.HandleFunc(route.Path, wrap(action.ID+"-"+route.ID, h.makeActionHandler(ext, desc.ID, action.ID, route)))
			}
		}
	}
}

// makeActionHandler 构造一个转发到 plugin.InvokeAction 的 http.HandlerFunc。
func (h *handler) makeActionHandler(ext plugin.AdminExtension, pluginID plugin.ID, actionID string, route plugin.ActionRoute) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != route.Method {
			writeJSON(w, http.StatusMethodNotAllowed, errorBody{Error: "method not allowed"})
			return
		}
		var body []byte
		if r.Method == http.MethodPost {
			b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, adminBodyLimit))
			if err != nil {
				writeJSON(w, http.StatusBadRequest, errorBody{Error: "read body", Detail: err.Error()})
				return
			}
			body = b
		}
		ctx := context.WithValue(r.Context(), actionCtxKey{}, route)
		result, err := ext.InvokeAction(ctx, plugin.ActionRequest{
			PluginID: string(pluginID),
			ActionID: actionID,
			RouteID:  route.ID,
			Method:   r.Method,
			Body:     body,
		})
		if err != nil {
			slog.Warn("admin action error", "plugin", pluginID, "action", actionID, "route", route.ID, "error", err)
			writeJSON(w, http.StatusInternalServerError, errorBody{Error: "action failed"})
			return
		}
		code := result.Code
		if code == 0 {
			code = http.StatusOK
		}
		if result.Error != "" {
			writeJSON(w, code, errorBody{Error: result.Error})
			return
		}
		writeJSON(w, code, result.Data)
	}
}

// actionCtxKey 用于在 context 中携带 route 元数据（预留扩展）。
type actionCtxKey struct{}

// sensitiveOptionKeys 返回指定 backend 的敏感 option 键集合（来自插件 Descriptor Schema）。
// 管理页全量保存时用它判断哪些 option 需要从已保存配置中恢复。
func (h *handler) sensitiveOptionKeys(backend string) map[string]bool {
	keys := map[string]bool{}
	if h.deps.Registry == nil {
		return keys
	}
	p, ok := h.deps.Registry.Get(backend)
	if !ok {
		return keys
	}
	for _, f := range p.Descriptor().Schema {
		if f.Sensitive && f.Target == plugin.FieldTargetOption {
			keys[f.Name] = true
		}
	}
	return keys
}

// persistConfig 持久化一个完整的新配置并触发热重载（序列化写回）。
func (h *handler) persistConfig(next *config.Config) error {
	h.writeMu.Lock()
	defer h.writeMu.Unlock()
	return h.writeConfigYAMLLocked(next)
}

// EnsureJSONReExport avoids unused import when json is needed by callers.
var _ = json.Marshal
