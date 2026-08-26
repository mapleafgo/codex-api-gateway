package plugin

import "errors"

// ErrDelegateNotFound 表示委托目标 Backend 未在宿主中注册。
var ErrDelegateNotFound = errors.New("delegate backend not found")

// DelegateHost 允许分发型插件按稳定 ID 委托其他 Backend，而不 import 插件实现。
type DelegateHost interface {
	BackendByID(id ID) (Backend, error)
}

// DelegateConsumer 在注册表组装完成后由宿主向分发型插件注入 DelegateHost。
type DelegateConsumer interface {
	SetDelegateHost(DelegateHost)
}

// MapDelegateHost 是测试与最小宿主使用的内存委托表。
type MapDelegateHost map[ID]Backend

// BackendByID 按 ID 查找被委托的 Backend；未注册时返回 ErrDelegateNotFound。
func (h MapDelegateHost) BackendByID(id ID) (Backend, error) {
	b, ok := h[id]
	if !ok {
		return nil, ErrDelegateNotFound
	}
	return b, nil
}

// WrapDelegatedEvent 把被委托 Backend 的观测事件重写为当前插件的稳定 ID。
// 只替换 Backend 字段，其余观测字段原样保留。
func WrapDelegatedEvent(id string, ev UpstreamEvent) UpstreamEvent {
	ev.Backend = id
	return ev
}
