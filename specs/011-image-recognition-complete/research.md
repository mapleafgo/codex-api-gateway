# Research: 图片识别三协议完全可用

## Decision 1 - 映射判定层职责与依赖

- **Decision**: 新增 `internal/imagemapper` 包，只做「这条图片该怎么处理」的判定与日志
  脱敏，输出中立 `Decision`，不产出任何目标协议 SDK 类型。
- **Rationale**: a/c 两路径都需要按 image_url / file_id / detail 做近乎相同的判定，统一
  收敛可避免双份逻辑漂移；判定层只依赖 openai-go（输入类型）与 slog，不依赖 anthropic
  SDK / chatconvert 类型，保证 L2 层内无环依赖。
- **Alternatives considered**: 在 convert / chatconvert 各自补丁（方案 A/C）——改动面小但
  判定逻辑分散；跨层共享类型放 model——但判定是转换层职责，放 L2 更贴切。

## Decision 2 - detail 档位处理

- **Decision**: `ResponseInputImageParam` 与 `ResponseInputImageContentParam` 均含
  `detail`（low / high / auto / original），Chat 的 `ChatCompletionContentPartImageImageURLParam`
  有 `detail` 槽位，Anthropic `ImageBlockParam` 无 detail。据此：a 路径有损丢弃 detail 并
  矩阵登记 lossy（图像本体必须保留）；c 路径 detail 保留透传；r 路径原生透传。
- **Rationale**: 官方 SDK 类型是权威事实源（宪法 II）；有槽位就透传，无槽位才登记损失，
  不为控制字段丢弃图像本体。
- **Alternatives considered**: 全部丢弃 detail——c/r 有槽位，丢弃属无谓损失。

## Decision 3 - file_id 与 system/developer 图片

- **Decision**: 仅 file_id 的识别图片在非透传源为协议不可映射，源级失败 + 既有换源；
  Responses 透传源原样保留引用。系统/开发者指令图片：Anthropic `System []TextBlockParam`
  仅文本、Chat system content union 仅含文本 parts，故 a/c 均为协议不可映射源级失败；
  Responses 允许 system/developer 携带图片，透传由上游裁决。
- **Rationale**: 官方 SDK 类型确认 a/c 的 system 位置无图片槽位；「不丢图发残缺指令」
  是用户定位宪法 VIII 的核心要求。
- **Alternatives considered**: a 路径 file_id 维持 WARN+丢弃——会造成模型没看到图却以为
  看到了的误导，违反宪法 VIII，已由用户明确否决。

## Decision 4 - 日志脱敏

- **Decision**: `SanitizeURL` 对普通 URL 抹掉 query/fragment 保留基础地址（协议/主机/
  路径）；data URI 只记录类型与字节数元数据；不记录凭据、完整图像本体。
- **Rationale**: 图片地址常携带签名或令牌，大体积 data URI 会污染日志；宪法 VI 要求
  凭据不落日志。
- **Alternatives considered**: 直接记录原始 URL——泄露查询参数凭据，不可接受。
