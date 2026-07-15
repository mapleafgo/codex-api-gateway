# CodexApiGateway

A gateway that lets **Codex CLI** use Anthropic-compatible backends via the OpenAI Responses API, with multi-source failover and circuit breaking.

## Run

```bash
cp config.example.yaml config.yaml  # fill api_key via ${ENV}
go run ./cmd/server -config config.yaml
```

Point Codex at `http://127.0.0.1:8080/v1/responses`.

Use a Codex/OpenAI-side model alias such as `gpt-5` or `gpt-5.5`, then map it
to the provider model in `config.yaml`:

```yaml
model_map: { gpt-5: glm-5.2 }
default_model: glm-5.2
```

Avoid setting Codex's model directly to provider-only names like `glm-5.2`;
Codex may not have metadata for those names.

## Endpoints

- `POST /v1/responses` — OpenAI Responses API（核心转发入口）
- `GET /v1/models` — 返回上游可用模型 + 本地 `model_map` 别名的合并列表

## How it works

Codex → `POST /v1/responses` → enrich session (`previous_response_id`) → convert Response→Anthropic → pick highest-priority healthy source → stream Anthropic SSE → convert back to Response SSE → Codex. Failover happens before the first byte; after that the source is locked.

`GET /v1/models` 按优先级遍历上游源，取首个健康源的 `/v1/models` 响应，再合并 config 中 `model_map` 的本地别名，去重后以 OpenAI 格式返回。

See `docs/superpowers/specs/2026-07-14-codex-api-gateway-design.md` for the full design.

## Known limitations

Protocol fields with no Anthropic equivalent are accepted but not mapped (full audit in `.superpowers/sdd/mapping-audit.md`). Notable ones:

- **`input_image.file_id`** — not supported. Anthropic image blocks only take `base64`/`url`; a `file_id` (OpenAI Files) can't be resolved without OpenAI credentials. Use `url` / data-URI images.
- **`tool_choice: {type: "allowed_tools", tools: [...]}`** — degrades to no restriction. Anthropic `tool_choice` has no "allowed subset" variant (only `auto` / `any` / `tool{name}`), so all declared tools remain callable.
