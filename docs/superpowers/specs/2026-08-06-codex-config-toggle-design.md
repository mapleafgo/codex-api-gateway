# Codex「应用到 Codex」托盘开关设计

## 概述

系统托盘新增可勾选项「应用到 Codex」：勾选后把本机 Codex CLI 的用户配置
`$CODEX_HOME/config.toml` 指向本网关；取消勾选恢复启用前的
`model_provider` 原值。我们的 provider 块常驻文件、不随开关增删，
但每次启用都会整体覆盖为当前网关地址，保证端口变更后重新勾选即可刷新。
若 `config.toml` 不存在，不做任何处理、不自动创建。

## 权威依据

本地 codex-cli 0.146.0 对应 codex-rs tag `rust-v0.146.0-alpha.9.2`：

- 配置目录：`codex-rs/utils/home-dir/src/lib.rs` 的 `find_codex_home()`。
  `CODEX_HOME` 非空时必须存在且是目录并 canonicalize，否则报错；
  未设置时默认 `home_dir()/.codex`，不校验存在。
- 配置文件：`codex-rs/core/src/config/mod.rs` 的
  `CONFIG_TOML_FILE = "config.toml"`；用户层即 `$CODEX_HOME/config.toml`。
- provider 字段：`codex-rs/model-provider-info/src/lib.rs` 的
  `ModelProviderInfo`，`wire_api` 仅支持 `responses`；
  `requires_openai_auth` 默认 `false`（跳过登录界面、凭据不走 auth.json），
  且 `auth` 与 `requires_openai_auth = true` 冲突（validate 报
  `provider auth cannot be combined with requires_openai_auth`），
  因此我们的块不写该字段，保持默认 false 语义。
- 编辑方式：codex 自身用 `ConfigEditsBuilder`（`core/src/config/edit.rs`）
  做文档级精确编辑并原子写回；本设计对齐该思路，行级精确编辑，不重排文件。

## 决策

- 新增 `internal/codexconfig` 包，纯标准库，零新依赖。
- `FindCodexHome()` 一比一复刻 codex 的 `CODEX_HOME` 判定逻辑。
- `config.toml` 采用行级精确编辑：只增删/覆盖我们负责的键和表块，
  保留注释、顺序与用户的其他配置；原子写回（临时文件 + rename）。
- provider 标识固定 `codex-api-gateway`，不动用户已有的 `custom` 等块。
- 备份文件 `~/.codex/codex-api-gateway-backup.json`（0600）只存启用前的
  `model_provider` 原值。
- 禁用只恢复 `model_provider`；我们的 provider 块保留，不增不删。
- provider 块只写 `auth.command`，不写 `requires_openai_auth`，
  避免与 codex 的 auth 冲突校验相撞。
- 不自动创建 `~/.codex` 或 `config.toml`；配置文件缺失时启用直接报错。

## API

```go
package codexconfig

func FindCodexHome() (string, error)

type Manager struct{ /* baseURL 闭包 + 互斥锁 */ }

func New(baseURL func() string) *Manager
func (m *Manager) IsEnabled() (bool, error)
func (m *Manager) Enable() error
func (m *Manager) Disable() error
```

base URL 由 main 注入：`adminURLFromListen(cfg.Server.Listen) + "/v1"`。

## 启用流程

0. 若已处于启用态（`model_provider` 与 `base_url` 均已一致）→ no-op，
   不写备份、不覆盖块，避免把我们的值误当原值备份。
1. `FindCodexHome()`；`CODEX_HOME` 缺失或非目录按 codex 语义返回错误。
2. 读取 `config.toml`；不存在时返回明确错误，不自建文件、不写备份。
3. 若备份文件尚不存在，写入当前 `model_provider` 原值（无则 `null`）。
4. 整体覆盖 `[model_providers.codex-api-gateway]` 块（含嵌套 `auth` 表），
   内容为当前 base URL、`wire_api = "responses"`、
   `auth.command = "echo codex-local"`；不写 `requires_openai_auth`。
5. 设置顶层 `model_provider = "codex-api-gateway"`。
6. 原子写回，保留原文件权限（新文件 0600）。

```toml
[model_providers.codex-api-gateway]
name = "Codex API Gateway"
base_url = "http://127.0.0.1:9870/v1"
wire_api = "responses"

[model_providers.codex-api-gateway.auth]
command = "echo codex-local"
```

## 禁用流程

1. 读取备份文件；不存在时：当前为启用态 → 返回错误并保持现状；
   未启用 → no-op。`config.toml` 同样缺失时 no-op，不创建文件。
2. 把顶层 `model_provider` 恢复为备份中的原值（原值为空则删除该行）。
3. 原子写回；删除备份文件；`codex-api-gateway` 块保留。

## 勾选态判定

启用态 = 顶层 `model_provider == "codex-api-gateway"` **且**
块内 `base_url` 与当前网关 base URL 一致。任一不满足视为未启用，
重新勾选即整体覆盖刷新（覆盖端口变更等场景）。`config.toml`
不存在时直接视为未启用。

## 错误处理

- 查询/切换失败：`slog.Warn`，托盘勾选保持原状。
- 禁用时备份缺失且当前启用：报错不写入，防止无法恢复。
- base URL 闭包为空（config 尚未加载完成）：`Enable` 返回明确错误。

## 托盘 UI 与 main 注入

- `tray.Config` 增加 `Codex *codexconfig.Manager` 字段；
  非 nil 时显示「应用到 Codex」勾选项。
- 菜单顺序：打开 → 分隔 → ☑ 应用到 Codex → ☑ 开机自启 → 分隔 → 退出。
- 点击逻辑复用「开机自启」模式：查询状态 → 切换 → 成功重建菜单。
- `cmd/server` 在创建托盘时注入 Manager（base URL 闭包），
  `config.Load` 完成后写入实际地址；托盘仍最先启动，不破坏现有退出体验。

## 测试

- `home_test.go`：对齐 codex 单测的四个用例：`CODEX_HOME` 缺失路径、
  `CODEX_HOME` 是文件、目录 canonicalize、未设置时默认 `~/.codex`。
- `toml_edit_test.go`：顶层键替换/插入/删除、表块覆盖/插入、
  注释与其他配置保留。
- `manager_test.go`：最小已有配置启用、已有配置启用（其他内容保留）、
  重复启用幂等、端口变更后 `IsEnabled=false` 且重新启用覆盖 `base_url`、
  禁用恢复原值/删除原行、备份缺失报错、禁用后块保留、备份删除。
  `config.toml` 缺失时 `Enable` 报错且不创建文件、不写备份。
- 门禁：`task check` + `task test-race`。

## 文档

- README「系统托盘」节补充「应用到 Codex」说明：备份文件路径、
  恢复语义、端口变更后重新勾选刷新。
- 本设计文档。

## 不做

- 不删除/改写用户的其它 `model_providers`。
- 不监听 codex `config.toml` 热更新；端口变更需重新勾选刷新。
- 不写 systemd、不自动重启 Codex CLI。
- 不引入 TOML 解析依赖。
