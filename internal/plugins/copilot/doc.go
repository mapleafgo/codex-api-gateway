// Package copilot 实现 GitHub Copilot 源插件。
//
// 把 Copilot 的 endpoint 发现、模型目录筛选缓存、按模型能力的协议路由、
// Zed 风格请求头、Device Flow 授权和连通性探测全部收拢在本包内。共享核心
// 不感知 Copilot 专属事实；该插件通过插件契约按稳定 ID 委托其他 Backend。
package copilot
