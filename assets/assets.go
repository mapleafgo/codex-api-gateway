// Package assets 提供项目共享的静态资源（logo 等），通过 go:embed 内嵌。
// 托盘（tray）与管理页（admin）共用同一份 logo，避免重复维护。
package assets

import _ "embed"

//go:embed logo.png
var Logo []byte
