# Contract: internal/imagemapper

## Package Boundary

- `internal/imagemapper` 是 L2 转换层共享包，只做图片输入的映射判定与日志脱敏。
- 只依赖 `github.com/openai/openai-go/v3`（输入类型）与 `log/slog`，不依赖 anthropic
  SDK、`chatconvert` 类型或 `convert` 类型，禁止产生环依赖。
- `internal/convert`（a 路径）与 `internal/chatconvert`（c 路径）是消费者；Responses
  透传路径（r）不调用判定入口，仅在需要时复用 `SanitizeURL`。

## Public API

```go
type Kind int

const (
    KindMapped    Kind = iota
    KindFileID
    KindMalformed
)

type Decision struct {
    Kind    Kind
    URL     string
    DataURI bool
    Detail  string
    FileID  string
}

func Inspect(imageURL, fileID param.Opt[string], detail string) Decision
func InspectParam(img *oairesponses.ResponseInputImageParam) Decision
func InspectContentParam(img *oairesponses.ResponseInputImageContentParam) Decision
func SanitizeURL(u string) string
```

## Semantics

- `Inspect`：有非空 `image_url` 时返回 `KindMapped`（URL 原文进 `URL`，`DataURI` 标记
  `data:` 前缀，detail 原文透传）；否则有非空 `file_id` 时返回 `KindFileID`；否则返回
  `KindMalformed`。`Detail` 原样传递，不做校验。
- `InspectParam` / `InspectContentParam`：从 SDK 类型拆出 `image_url` / `file_id` /
  `detail` 后委托 `Inspect`，供 a/c 路径免手拆字段。
- `SanitizeURL`：普通 URL 抹掉 query 与 fragment，只保留协议、主机、路径；data URI 返回
  类型与字节数元数据（如 `data:image/png;base64,<bytes=NNN>`），不携带本体。
- 判定函数不写日志、不返回 error（畸形输入归入 `KindMalformed` 由调用方决定语义）。

## Consumer Contract（a / c 路径）

- `KindMapped`：按目标协议构造原生图片槽位；detail 按目标协议槽位取舍（a 丢弃+矩阵登记，
  c 透传）。
- `KindFileID` / `KindMalformed`：非透传源返回 `fmt.Errorf` 源级失败，进入既有
  Backend → scheduler 换源链路；透传源原样保留引用。
- system/developer 角色图片：由调用方在构造消息前按角色判定（a/c 均源级失败），判定层
  不感知角色。
