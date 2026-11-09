# 接入 QQhuxuhui/new-api

适配版本：`4.27.1-newapi`。对照源码：[`QQhuxuhui/new-api@8faf1d45`](https://github.com/QQhuxuhui/new-api/tree/8faf1d45e8c101974dc7483324d3d91c8a462ffe)。仅修改 web2api，new-api 无需改源码。

## 渠道选择

| new-api 渠道类型 | 客户端接口 | web2api 返回格式 |
|---|---|---|
| OpenAI（类型 1） | `/v1/chat/completions` | `choices[].message` / SSE `choices[].delta`；图片保留 Markdown data URL |
| OpenAI（类型 1） | `/v1/responses` | `output[]` / `response.*` SSE 事件 |
| OpenAI（类型 1） | `/v1/images/generations` | `created` + `data[].b64_json`，或 `data[].url` |
| OpenAI（类型 1） | `/v1/images/edits` | 同上；支持 JSON 参考图和 multipart 文件 |
| Gemini（类型 24） | `/v1beta/models/{model}:generateContent` | `candidates[].content.parts[].inlineData`、`usageMetadata` |
| Gemini（类型 24） | `:streamGenerateContent?alt=sse` | 每帧 Gemini JSON，图片为 `inlineData` |

建议图片专用接口使用 **OpenAI 渠道**。该 new-api 版本的 Gemini 渠道 `ConvertImageRequest` 只接受 `imagen*` 模型，不能用 Gemini 渠道调用 `gemini-image` 的 `/v1/images/generations`；Gemini 原生 `generateContent` 则可使用 `gemini-image`。

渠道设置：

- 代理地址填 web2api 的服务根地址，例如 `http://web2api:8083`，**不要附加 `/v1` 或 `/v1beta`**。Docker 内的 `127.0.0.1` 指向当前容器，两个容器之间须使用可达的服务名或宿主机地址。
- 密钥填 web2api 管理面板中的客户端 API key。OpenAI 渠道发送 Bearer，Gemini 渠道发送 `x-goog-api-key`，两者均已支持。
- 图片模型填 `gemini-image`；文本模型以 web2api `/v1/models` 实际列表为准。生图需要已启用且有效的 Google 网页账号 Cookie。
- 若使用对外模型别名，在 new-api 渠道中把别名映射到 `gemini-image`；不要把别名当作 web2api 已实现的真实模型。
- new-api 的额度/价格由管理者按实际服务配置；web2api 不提供 Google 官方 token 或图片费用。图片专用响应不伪造图像 token usage，应按图片数量设置价格。

## 图片生成

向 new-api 或 web2api 的 `/v1/images/generations` 发送：

```json
{
  "model": "gemini-image",
  "prompt": "生成一张蓝色茶壶的产品照片",
  "n": 1,
  "response_format": "b64_json",
  "image_size": "2K",
  "aspect_ratio": "16:9"
}
```

响应示意（示意 Base64 不是可用图片）：

```json
{
  "created": 1794220000,
  "data": [{"b64_json": "图片文件的纯Base64"}]
}
```

默认 `b64_json`。`response_format: "url"` 返回 `data:image/...;base64,...`，不是 HTTP 下载链接；只接受 HTTP URL 的客户端应使用 `b64_json`。不暴露 Google 的带凭据下载地址，也不新增图片托管服务。

`stream: true` 返回 `image_generation.completed` SSE 事件，每个图片产物一个事件，最后是 `data: [DONE]`。编辑接口使用 `image_edit.completed`。上游没有可用的渐进图片，故等待完整图片生成后才发送完成事件；开始发送前的失败仍返回 HTTP 400/429/502 JSON。

## 图片编辑

`POST /v1/images/edits` 支持 JSON：

```json
{
  "model": "gemini-image",
  "prompt": "把茶壶改成红色，保留背景",
  "images": [{"image_url": "data:image/png;base64,真实Base64"}],
  "response_format": "b64_json"
}
```

参考图可使用 data URL 或 HTTP(S) 直链。也接受 `image` 字符串、`image` 数组、`{"url":"..."}` 对象；不支持 `file_id`。

OpenAI SDK 常用的 multipart 也可使用，文件字段为 `image` 或多个 `image[]`；文本字段包括 `model`、`prompt`、`n`、`stream`、`response_format`、`image_size`、`aspect_ratio`、`size`。单张参考图最多 12 MiB，请求体最多 64 MiB。文件内容按实际 MIME 校验，复用已有参考图上传链路。

## 比例与清晰度

`image_size` / `aspect_ratio` 继续遵循[生图参数说明](image-options.md)，不保证精确像素，不做本地缩放。2K–8K 是原图下载策略；不是原生分辨率承诺。

为兼容 new-api 图片渠道测试固定发送的 `size: "1024x1024"`，**仅 Images 两个新接口**还接受下列 OpenAI 画布预设，转换成比例指令：

| size | 比例 |
|---|---|
| `256x256` / `512x512` / `1024x1024` | `1:1` |
| `1536x1024` / `1024x1536` | `3:2` / `2:3` |
| `1792x1024` / `1024x1792` | `16:9` / `9:16` |

这些预设不会把结果缩放成指定像素。`size` 也可填 `auto`、`16:9` 或 `2K`。相互冲突的参数返回 400。Chat / Responses 原有的像素尺寸拒绝规则保持不变。

new-api 的 Chat DTO 保留 `imageConfig` 与 `extra_body`，可能丢弃不在 DTO 中的顶层扩展字段。经该网关使用 Chat 时优先用：

```json
{
  "model": "gemini-image",
  "messages": [{"role": "user", "content": "生成一张蓝色茶壶照片"}],
  "extra_body": {
    "google": {
      "image_config": {"image_size": "2K", "aspect_ratio": "16:9"}
    }
  }
}
```

web2api 已补充识别 `extra_body.google.image_config`。Gemini 原生请求使用 `generationConfig.imageConfig`。

## 边界与验证

- `n` 只能省略或设为 1。遮罩、输出编码转换、透明背景控制、输入保真度和渐进预览暂不支持，相应字段返回 400。
- `quality` 不代表分辨率，不控制网页模型的采样质量；选择下载方式请使用 `image_size`。
- Chat 的推理用量放入 `usage.completion_tokens_details.reasoning_tokens`，以匹配 new-api DTO。保留既有计数口径：正文 token 不包含思考和图片 Base64，数值为本地估算，非 Google 官方计量。
- 不新增 Embeddings、音频转写、Realtime 或 Imagen `predict` 能力；这些接口不是网页模型已提供的能力。
- 自动测试覆盖图片 JSON/SSE、编辑参考图、multipart、多图、认证、参数拒绝、产物缺失、上游失败和 Chat 推理用量；不替代实际 Google 账号可用性或生产 new-api 渠道验证。

2026-11-09 的本地协议验证还使用上述 new-api 提交中的原始 `relaykit/dto` 与 `kitutil`，成功解析由 web2api 实际处理函数产生的 Images JSON/SSE、Gemini `inlineData`、Chat SSE 和嵌套推理用量。生成步骤使用模拟图片产物，未启动或修改 new-api 服务。

参考代码：new-api 的 `relay/channel/openai/adaptor.go`、`relay/channel/openai/relay_image.go`、`relay/channel/gemini/adaptor.go`、`relaykit/dto/openai_image.go`、`relaykit/dto/openai_response.go`、`relaykit/relayconvert/internal/oai_chat/to_gemini_chat_req.go`、`controller/channel-test.go`。

## 从 dev 构建

```bash
git clone --branch dev https://github.com/Guo-jiayu/gemini-web2api-go.git
cd gemini-web2api-go
go test ./...
go build -o gemini-web2api-go .
```

使用项目 go.mod 指定的 Go 版本。也可从源码构建容器：

```bash
export ADMIN_TOKEN='替换为自己的管理密码'
docker compose -f deploy/compose.low-latency.yml up -d --build
```

部署模板绑定回环地址，数据库由 Docker 卷持久化；导入服务器自己的账号和代理。已有数据库中的面板设置优先于配置文件。官方 `latest` 镜像不包含此 dev 分支修改；本次推送不自动发布 Docker 镜像。
