# Contract: Config v2

## Overview

配置升级为破坏性变更：源类型用稳定 `backend` 标识，源专属参数进入 `options`。旧格式绝不兼容也不静默迁移，只返回可执行的迁移错误。

## Source Shape

```yaml
sources:
  - name: anthropic-official
    backend: anthropic
    base_url: https://api.anthropic.com
    api_key: ${ANTHROPIC_KEY}
    default_model: claude-sonnet-4-20250514
    model_map: { gpt-5: claude-sonnet-4-20250514 }
    options:
      default_max_tokens: 16384
      cache_enabled: true

  - name: copilot
    backend: github-copilot
    default_model: gpt-5.3-codex
    options:
      github_token: ${COPILOT_GITHUB_TOKEN}
```

## Common Fields

| 字段 | 必填 | 说明 |
|---|---|---|
| `name` | 是 | 平台唯一；用于日志、breaker、故障转移定位 |
| `backend` | 是 | 已注册插件 ID |
| `base_url` / `api_key` | 按插件 | 通用连接字段；是否省略由 schema 决定 |
| `headers` | 否 | 现有名称合法性校验保持不变 |
| `supports_web_search` | 否 | 跨源请求形状开关，留在通用区 |
| `default_model` / `model_map` | 否 | 平台级模型映射 |
| `breaker` | 否 | 继承全局并做非负校验 |
| `disabled` | 否 | 停用该源但保留配置 |
| `options` | 按插件 | 只含插件 schema 声明的键 |

## Rejected Legacy Fields

| 旧表达 | 行为 |
|---|---|
| `backend_type: a/c/g/r` | 返回迁移错误，指明应使用 `backend` 稳定标识 |
| 顶层 `github_token` | 作为未知字段拒绝，指明应移入对应源 `options` |
| 顶层 `anthropic:` | 作为未知字段拒绝，指明应迁入 `backend: anthropic` 源 `options` |
| 未注册 `backend` | 返回可用列表 |
| schema 外 option | 返回 source/field/原因 |

顶层配置采用已知字段白名单：`server`、`logging`、`breaker`、`sources`、`models`。任何未知顶层字段都拒绝；其中 `backend_type`、顶层 `github_token`、顶层 `anthropic` 必须返回专门的迁移错误，其余未知字段返回通用 unknown-field 错误。Source 通用区同样采用白名单，历史专属字段不得静默丢弃。

## Validation Injection

```go
type SourceValidator interface {
    ValidateSource(config.Source) error
}
```

- `config.Load`、admin 写盘路径、configwatch reload 统一使用同一注入的 Registry 校验。
- 校验发生并返回错误时，进程/写盘失败；`holder.Replace` 不执行，旧配置继续服务。
- `config` 自身不 import `plugin` 或具体实现，只依赖注入的窄接口。

## Defaults And Write-back

- schema 声明的 default 只在缺省时生效；写回时省略与用户未修改一致的默认噪音。
- `api_key` 与 options 敏感字段在管理读取时脱敏；特殊哨兵语义见 admin contract。
- `${VAR}` 插值先于 registry 校验，但校验错误信息不得包含临时凭据明文。

## Migration Error Example

```yaml
# config.yaml 错误示例
sources:
  - name: copilot
    backend_type: g          # 拒绝
    github_token: x          # 拒绝
```

对应错误：

```text
config: source "copilot": backend_type is removed; set backend to one of the
registered source plugins (anthropic, github-copilot, openai-chat, openai-responses)
and move backend-specific settings into options
```
