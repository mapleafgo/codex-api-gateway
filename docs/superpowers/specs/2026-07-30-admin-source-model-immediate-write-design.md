# 管理页供应商/模型即时落盘与侧栏导航设计

## 背景

管理页「新增供应商」「新增模型」走弹窗表单，但确认后只写入前端本地 `cfg`，必须再点底部「保存并热重载」才真正写盘。这与用户对弹窗提交的预期不符，也与已有的即时写盘路径（源禁用、源/模型排序）不一致。

供应商卡片列表变长后，仅靠主滚动区定位困难，需要在配置侧栏提供可点击、可滚动高亮的源导航，并在调序后实时反映顺序。

## 目标

- 弹窗「新增供应商 / 新增模型」确认后立即写盘并热重载。
- 删除供应商 / 删除模型确认后立即写盘并热重载。
- 卡片内字段编辑（名称、URL、密钥、映射、模型属性、全局参数等）仍为草稿，依赖底部保存栏。
- 左侧配置菜单下方增加供应商 TOC：点击跳转、滚动高亮、调序/增删后顺序实时同步。
- 局部写盘不得把前端未保存的字段编辑一并提交。

## 非目标

- 不把字段级编辑改为自动保存或防抖保存。
- 本轮不为模型页增加 TOC。
- 不改 `/v1/*` 转发路径与 scheduler 选源语义（仅配置写盘与热重载副作用）。
- 不引入新的前端框架或依赖。

## 方案选择

采用**局部写盘 API，基于 `holder.Current()` 快照**（对齐 `sources/disabled`、`sources/reorder`、`models/reorder`）：

1. 后端只在当前已落盘配置上追加或删除目标项。
2. 前端成功后再 patch 本地 `cfg` 与 `cfgSnapshot` 中对应项，保留其它 dirty 字段。
3. 禁止用整包 `POST /admin/api/config` 实现增删，避免脏编辑被连带落盘。

## API 设计

全部走 `internal/admin` 旁路，`writeMu` 串行写盘，成功路径调用既有 `writeConfigYAMLLocked`（校验 → 原子写 YAML → 热重载）。

### 新增供应商

`POST /admin/api/sources`

```json
{
  "name": "zhipu",
  "base_url": "https://...",
  "api_key": "...",
  "backend_type": "a",
  "default_model": "glm-4",
  "disabled": false
}
```

- 必填：`name`、`base_url`；`backend_type` 缺省 `a` 并经 `NormalizeBackendType`。
- `model_map` 新建时为空 map；本接口不接受未保存的复杂草稿映射。
- 重名（与 holder 中已有源 name 冲突）→ `400`。
- 成功 → `200 { "ok": true, "health": [...] }`（health 可选，与 disabled/reorder 一致）。
- 新源追加到 `Sources` 末尾。

### 删除供应商

`POST /admin/api/sources/delete`

```json
{ "name": "zhipu" }
```

- 按 **已落盘 name** 删除；未知 name → `400 unknown source`。
- 成功 → `200 { "ok": true, "health": [...] }`。

选用 POST 而非 HTTP DELETE，与现有 `sources/disabled`、`sources/reorder` 风格一致，避免部分环境对 DELETE body 支持不一致。

### 新增模型

`POST /admin/api/models`

```json
{
  "slug": "glm-latest",
  "context_window": 200000,
  "supports_image": false,
  "supports_search": true
}
```

- 必填：`slug`；与现有 `ModelSlugOrder` / `ModelOverrides` 结构对齐后写入。
- 重复 slug → `400`。
- 成功追加到顺序末尾（最低 priority 一侧与当前 UI「末尾新增」一致）。
- 成功 → `200 { "ok": true }`。

### 删除模型

`POST /admin/api/models/delete`

```json
{ "slug": "glm-latest" }
```

- 按已落盘 slug 从顺序与 overrides 中移除；未知 → `400`。
- 成功 → `200 { "ok": true }`。

### 写盘语义（硬约束）

- 一律 `cur := holder.Current()` 浅拷贝 Config，对将修改的 slice/map 做必要深拷贝后再改。
- `Validate()` 失败不得写盘。
- 不得读取或合并前端全量 `cfg`（防止脏字段覆盖）。
- 与 `disabled`/`reorder` 共享 `writeMu`，禁止并行写配置。

## 前端行为

文件：`internal/admin/assets/index.html`（及嵌入用 `index_html.go` 的既有生成/同步流程，若仓库以 go:embed 直接引用 assets 则只改 html）。

### 新增

1. `openForm` 收集字段并做本地必填/重复校验。
2. 调用对应 POST；请求期间禁用确认或展示 saving，避免双提交。
3. 成功：push 到 `cfg.sources` / `cfg.models`，同步 patch `cfgSnapshot` 追加同一项（含前端 `_id`），toast 成功，sources 滚到底部。
4. 失败：不改本地列表，toast 错误。

### 删除

1. 确认文案改为「将立即从配置中删除」，去掉「保存前可放弃」。
2. 删除身份取 **snapshot 中的已落盘 name/slug**：
   - 源：用卡片 `_id` 在 `cfgSnapshot.sources` 中定位 `name`；找不到则回退当前 `src.name`。
   - 模型：优先 snapshot 同索引或可匹配的 `slug`；找不到则回退当前 `m.slug`。
3. 这样可避免「未保存改名」后误删错误条目或对后端报 unknown。
4. 成功：本地 splice，并从 snapshot 同步移除；清理该源相关的 `testResults` 等派生状态。
5. 若后端 unknown 且本地确实无 snapshot 对应项：仅删本地并提示（防御历史脏状态）。

### dirty 状态

- 增删成功后 `JSON.stringify(cfg) === cfgSnapshot` 中与该项相关的部分保持一致，不因增删单独变 dirty。
- 其它字段未保存编辑继续使 `dirty === true`。
- 底部保存栏语义不变：只提交字段级草稿。

### 供应商侧栏 TOC

位置：`.ui-side` 内，配置菜单（sources / models / global / guidance）**下方**。

- 仅 `cfgTab === 'sources'` 时渲染。
- 列表顺序绑定 `cfg.sources`（调序、新增、删除后自动更新，无需另存顺序状态）。
- 每项展示优先级序号 + `src.name`（空名时用「新供应商」文案）。
- 源卡片增加稳定锚点：`id="src-card-" + (src._id || idx)` 或等价 `data-src-id`。
- 点击 TOC：对应卡片 `scrollIntoView({ block: 'start', behavior: 'smooth' })`，并立即将 `activeSourceId` 设为该项。
- 滚动高亮：监听 `x-ref="srcScroll"` 的 scroll（或 IntersectionObserver），取视口内最靠上的源卡片设为 active；TOC 项 `is-active` 样式与主菜单 active 区分但同色系。
- 调序：现有 `moveSource` / `reorderSourcesBackend` 成功后 TOC 因 `cfg.sources` 顺序变化自动重排；高亮跟随被移动项的 `_id`。

### 响应式

- `min-width: 768px`：左侧 TOC 可见（随 `.ui-side`）。
- 窄屏：`.ui-side` 仍隐藏；在 sources 内容区顶部增加可横滑 chips 导航，数据与点击/高亮逻辑与 TOC 共用，避免两套状态。

## 错误与并发

- 写盘中的增删与底部全量保存都走 `writeMu`；后到的请求串行执行，以 holder 最新快照为基准。
- 前端全量保存成功后 `loadConfig()` 重拉，覆盖本地；若用户在保存请求飞行中又点了删除，以后到的结果与随后 load 为准，可接受。
- 校验错误（重名、缺字段）返回 400 + `detail`，前端原样提示。

## 测试

`internal/admin`：

- 新增源：写入 YAML/holder 末尾，字段正确；重名/缺 name/缺 base_url 拒绝。
- 删除源：按 name 移除；unknown name 拒绝；删除后 health 可选断言。
- 新增模型：slug 进入顺序末尾与 overrides；重复 slug 拒绝。
- 删除模型：顺序与 overrides 同步移除；unknown slug 拒绝。
- 回归：`sources/disabled`、`sources/reorder`、`models/reorder`、全量 `POST /admin/api/config` 仍通过。

前端 TOC 为嵌入式 Alpine UI，本轮以结构存在性（锚点 class/id、侧栏容器）和 API 契约测试为主；完整滚动高亮以手工验收。

提交门禁：`task check`；涉及写盘与 holder 替换，跑 `task test-race` 中 admin 相关包或全量 race。

## 验收标准

1. 弹窗新增供应商后，不点底部保存，重启或重新打开管理页仍能看到该源。
2. 弹窗新增模型后同样立即持久化。
3. 删除供应商/模型确认后立即持久化；未保存的其它字段编辑不被这次删除提交。
4. 左侧菜单下 TOC 在 sources 页可见；点击跳转；滚动高亮；调序后 TOC 顺序与高亮正确。
5. 窄屏 chips 可跳转并高亮。
6. 卡片字段修改仍显示 dirty，需底部保存才落盘。
