# 托盘「开机自启」设计

## 概述

在系统托盘增加可勾选的「开机自启」菜单项，用户登录图形会话后自动启动网关。全平台（Linux / Windows / macOS）支持；真相源是 OS 自启注册，不写入 `config.yaml`。

## 背景

- 网关以托盘常驻为主入口；此前依赖手动 `packaging/install-autostart.sh` 或 systemd user service。
- systemd --user 默认无 `DISPLAY`/`WAYLAND_DISPLAY`，导致托盘「打开」冷启动浏览器失败，已改为推荐 desktop 自启。
- 需在托盘内一键开关，且 release 构建保持 `CGO_ENABLED=0` 交叉编译。

## 决策

**自研 `internal/autostart`，不引入第三方库。**

| 候选 | 结论 |
|------|------|
| emersion/go-autostart | Windows 用 CGO 建 `.lnk`，破坏 `CGO_ENABLED=0` 发布 |
| snail007/autostart 等 | 停更或非库形态 |
| 自研 | 纯 Go；Linux desktop / Win 注册表 / mac LaunchAgent；可单测 |

## API

```go
package autostart

type Spec struct {
    AppID       string   // 文件名/注册表键/plist Label，固定 "codex-api-gateway"
    DisplayName string   // "Codex API Gateway"
    Exec        string   // 绝对路径，通常 os.Executable()
    Args        []string // 如 {"-config", absConfigPath}
    WorkDir     string   // 可选；Linux desktop Path=
}

func (s Spec) IsEnabled() (bool, error)
func (s Spec) Enable() error
func (s Spec) Disable() error
```

### 平台实现

| 平台 | 机制 | 路径/键 |
|------|------|---------|
| Linux | XDG autostart `.desktop` | `$XDG_CONFIG_HOME/autostart/<AppID>.desktop` |
| Windows | `HKCU\Software\Microsoft\Windows\CurrentVersion\Run` | 值名=`AppID`，数据=带引号的命令行 |
| macOS | LaunchAgent plist | `~/Library/LaunchAgents/<AppID>.plist`，`RunAtLoad=true` |

Linux desktop 字段至少包含：`Type`、`Name`、`Exec`（参数正确引用）、`Path`（若 WorkDir 非空）、`Terminal=false`、`X-GNOME-Autostart-enabled=true`。文件名与 `packaging/install-autostart.sh` 一致，互为读写同一文件。

Windows 使用 `golang.org/x/sys/windows/registry`，**禁止 CGO**。

## 托盘 UI

菜单顺序：

1. 打开  
2. ──  
3. ☑ 开机自启（`AddCheckbox`）  
4. ──  
5. 退出  

- 启动 `runTray` 时 `IsEnabled()` 决定初始勾选；查询失败则未勾选 + `slog.Debug`。
- 点击：目标状态 = 当前 `!IsEnabled()`；成功 `Enable`/`Disable` 后**重建菜单**并 `SetMenu`（systray 不自动翻转 Checked）。
- 失败：`slog.Warn`，勾选保持原状。
- `Config.Autostart` 为 `nil` 时不显示该项。

## main 注入

`cmd/server` 在创建 tray 时：

- `Exec` = `os.Executable()` 失败则跳过 autostart 菜单  
- `Args` = `{"-config", absConfigPath}`  
- `WorkDir` = 可执行文件所在目录（便于相对路径 config/log）

## 不做

- 不在 `config.yaml` 持久化自启开关  
- 不自动打开浏览器  
- headless 无托盘时无 UI（本就无菜单）  
- 不写 systemd unit

## 测试

- Linux：临时 `XDG_CONFIG_HOME`，断言 desktop 内容与 Enable/Disable/IsEnabled  
- macOS：临时 `HOME`，断言 plist 含 `ProgramArguments` 与 `RunAtLoad`  
- Windows：可测命令行拼接；注册表读写需构建标签或跳过无权限 CI  
- 托盘：`onAutostartToggle` 逻辑可抽测；不依赖真实 systray

## 文档

README「开机自启」节改为：优先托盘勾选；`task install-autostart` 仍可作为安装 applications 入口的补充。
