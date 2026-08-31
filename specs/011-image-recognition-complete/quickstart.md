# Quickstart: 图片识别三协议完全可用

## 前置

- Go 1.26.5、Taskfile CLI。
- 验证识别图片需要有图像类上游凭据；无凭据时以单元测试覆盖为准。

## 1. 单元测试覆盖（核心验收）

```bash
go test ./internal/imagemapper/... ./internal/convert/... ./internal/chatconvert/...
task test-race
```

期望：新增 `imagemapper` 判定/脱敏测试与 a/c 路径改造测试全部通过；race 无告警。

## 2. 映射行为验证

对 a 路径（`internal/convert`）与 c 路径（`internal/chatconvert`）分别喂入以下
`input_image` 输入，检查转换结果：

| 输入 | a 路径期望 | c 路径期望 |
|------|-----------|-----------|
| URL 图片（user） | image block（URL） | `image_url` part |
| data URI 图片（user） | image block（base64） | `image_url` part（data 原样） |
| URL 图片 + detail=high | image block + detail 丢弃 + 矩阵 lossy | `image_url` part + detail=high |
| 仅 file_id（user） | 返回 error（源级失败） | 返回 error（源级失败） |
| URL 图片（system/developer） | 返回 error（Anthropic system 仅文本） | 返回 error（Chat system 仅文本） |
| tool 结果含图片 | tool_result image block | 聚合 user `image_url` part |

期望：判定结果与上表一致；`KindFileID` / `KindMalformed` / system 图片均不产生残缺请求。

## 3. 日志脱敏验证

对 `SanitizeURL` 断言：

- `https://h/p.png?sig=abc&x=1#frag` → `https://h/p.png`
- `data:image/png;base64,AAA...` → 含类型与字节数元数据，不含本体
- 不含 `Authorization` / API key / 完整 data 本体

期望：全部通过，图片地址不落查询参数与片段。

## 4. 端到端（可选，需凭据）

配置含 a/c/r 三源的 `config.yaml` 启动网关，发送带 URL 图片的 `/v1/responses` 请求：

```bash
task run
curl -s http://127.0.0.1:8383/v1/responses -d '{"model":"m","input":[{"role":"user","content":[{"type":"input_text","text":"what is this?"},{"type":"input_image","image_url":"https://example.com/a.png","detail":"high"}]}],"stream":true}'
```

期望：a/c 源收到等价图片 + detail 按槽位取舍；r 源透传原样；响应为正常 SSE；日志中
图片地址为基础地址。
