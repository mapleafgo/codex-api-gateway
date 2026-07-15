# Final Whole-Branch Review - Fix Summary

Commit: `5b8e354c35e0a56786dbf52227f83319a1afcd0a`
Subject: `fix: apply model_map, default max_tokens, thinking beta header, capture streamed output`

## Fixes Applied

### IMPORTANT-1: model_map is never applied
- **File**: `internal/scheduler/scheduler.go`
- **Change**: In `trySource`, before calling `s.client.Stream`, the model is resolved via `ResolveModel(src, req.Model)` using the selected source's `ModelMap`. A shallow copy of the request is made so the original is not mutated.
- **Test**: `TestModelMapResolvedBeforeStream` in `internal/scheduler/scheduler_test.go` -- uses an httptest server that records the request body's `model` field and asserts it equals the mapped value (`claude-sonnet-4-20250514`), not the original alias (`gpt-5`).

### IMPORTANT-2: no default for max_tokens
- **File**: `internal/convert/request.go`
- **Change**: After setting `out.MaxTokens` from the request, if it is still 0 it defaults to 4096.
- **Test**: `TestToAnthropicDefaultMaxTokens` in `internal/convert/request_test.go` -- a request with no `MaxOutputTokens` yields an `AnthropicRequest` with `MaxTokens == 4096`.

### IMPORTANT-3: missing anthropic-beta header for thinking
- **File**: `internal/anthropic/client.go`
- **Change**: In `Stream`, when `req.Thinking != nil`, sets the header `anthropic-beta: interleaved-thinking-2025-05-14`.

### collectOutput stores request-side items (core scenario)
- **Files**: `internal/streamconv/converter.go`, `internal/server/server.go`
- **Change**: The converter now accumulates output items from the stream as they are produced:
  - `output_item.added(text)` -> starts a MessageItem (assistant role); text collected via `output_text.delta` into a ContentPart with type `output_text`.
  - `output_item.added(reasoning)` -> starts a ReasoningItem; thinking text collected into Summary.
  - `output_item.added(function_call)` -> starts a FunctionCallItem (CallID, Name); arguments collected via `function_call_arguments.delta`.
  - New method `OutputItems() []model.InputItem` returns the accumulated items.
- **Server**: `collectOutput(&req)` replaced with `conv.OutputItems()` as the primary source for `Save`; falls back to `collectOutput` only if the converter produced nothing.
- **Tests**: `TestConverterOutputItemsFunctionCall` and `TestConverterOutputItemsTextAndReasoning` in `internal/streamconv/converter_test.go`.

## Verification

- `go test ./...` -- all 25 tests pass (8 packages with tests, 1 package no test files).
- `go build ./...` -- clean.
- `go vet ./...` -- clean.

## Invariants Preserved

- First-byte lock: unchanged (scheduler watchdog logic intact).
- Completion dedup: unchanged (converter `completed` guard intact).
- Session-id alignment: `conv.RespID()` still used for the Save key.
