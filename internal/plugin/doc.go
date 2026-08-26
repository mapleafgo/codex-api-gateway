// Package plugin 定义源插件的共享契约与不可变注册表。
//
// 调度、服务编排、配置核心、管理框架与健康框架只依赖本包暴露的接口，
// 禁止 import internal/plugins/* 下的具体实现。具体插件只在 cmd/server
// 组装入口注册，并通过本包的 Descriptor/Backend/AdminExtension 等契约
// 对外声明能力。
package plugin
