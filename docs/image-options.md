# 生图清晰度和比例参数

版本：`4.27.1-image-options`。日期：2026-11-09。

模型名仍为 `gemini-image`。本扩展适用于 `/v1/chat/completions`、`/v1/responses`、`/v1beta/models/gemini-image:generateContent` 及其流式模式。

## OpenAI Chat 格式

```json
{
  "model": "gemini-image",
  "messages": [{"role": "user", "content": "生成一张红色茶壶的产品摄影图"}],
  "image_size": "2K",
  "aspect_ratio": "16:9",
  "stream": false
}
```

Responses 使用相同参数，将 `messages` 换为 `"input": "生成一张红色茶壶的产品摄影图"`。

OpenAI 兼容扩展同时接受 `imageSize` / `resolution` 作为清晰度字段、`aspectRatio` 作为比例字段，也接受 `image_config`、`imageConfig`、`generation_config.image_config`、`generationConfig.imageConfig` 或 `extra_body` 内的这些字段。SDK 的 `extra_body` 通常会合并到 HTTP 请求顶层。

`size` 可填 `2K` 或 `16:9`；建议明确使用 `image_size` 和 `aspect_ratio`。多个字段表达同一含义时必须一致，否则返回 400。`auto` 表示不指定。`quality=hd` 不等同于分辨率档位，请明确传 `image_size`。

## Gemini 原生格式

```json
{
  "contents": [{"role": "user", "parts": [{"text": "生成一张红色茶壶的产品摄影图"}]}],
  "generationConfig": {
    "responseModalities": ["TEXT", "IMAGE"],
    "imageConfig": {"imageSize": "4K", "aspectRatio": "9:16"}
  }
}
```

## 下载方式

| 请求 | 下载方式 |
|---|---|
| 未传清晰度 / auto | 沿用管理面板全局 `image_upscale` |
| 1K | 普通预览下载，即使全局开关开启 |
| 2K 或以上（2K–8K 整数档） | 本次请求使用网页“下载原图/放大”，即使全局开关关闭 |

所有参数保存在请求独立的模型配置副本里，不修改全局运行配置或共享模型定义。

这里的 2K / 4K 是下载策略选择，不是对最终像素大小或原生细节的保证。上游 Ultra 账号通常可将宽高各放大 2 倍；其他账号可能只拿到同尺寸、压缩更少的原图。不会在本地插值放大以凑足 4K。

客户明确请求 2K 以上时，原图/放大下载失败将返回包含 `image_original_download_failed` 的错误，不退回预览图。不传参数、只开全局开关的旧调用保留原有失败回退行为。

## 比例与错误

只支持常用比例：`1:1`、`16:9`、`9:16`、`4:3`、`3:4`、`3:2`、`2:3`、`4:5`、`5:4`、`21:9`。不支持像素尺寸（例如 `3840x2160`），也不进行本地裁切或缩放。

比例通过附加生图指令传给上游。返回前解码图片尺寸，允许 2% 的相对比例偏差，以兼容上游固定像素网格（例如 1376×768 对应 16:9）。如果上游忽略指令导致比例明显不符，返回包含 `image_aspect_ratio_mismatch` 的错误。不会反复生成消耗额外额度。

非法字段值、冲突字段、不支持的比例和像素尺寸返回 HTTP 400；下载失败或上游比例不符返回 HTTP 502。如果流式响应已开始，则通过流内错误结束，不能再修改 HTTP 状态。

日志 `[image-output]` 记录本次下载方式、请求清晰度和请求比例，不记录 Cookie 或图片下载凭据。

## 2026-11-09 部署和实测

已替换并重启本地 `127.0.0.1:18083` 服务，根路径返回版本 `4.27.1-image-options`。全局 `image_upscale=false` 保持不变。

以下十次使用同一条不含比例描述的茶壶提示词，仅通过接口参数指定比例。全部 HTTP 200、图片可解码、比例偏差小于 2%。首两次同时执行，用于核验不同请求的参数隔离；下载方式已与服务端 `[image-output]` 日志逐条对应。

| 接口 | 清晰度 | 比例 | 下载方式 | 返回尺寸 | 耗时 |
|---|---|---|---|---|---|
| Chat | 1K | 1:1 | 预览 | 1024×1024 | 15.45 秒 |
| Chat | 2K | 16:9 | 原图/放大 | 2752×1536 | 23.50 秒 |
| Gemini 原生 | 4K | 9:16 | 原图/放大 | 1536×2752 | 24.17 秒 |
| Gemini 原生 | 2K | 21:9 | 原图/放大 | 3168×1344 | 22.55 秒 |
| Chat | 4K | 4:3 | 原图/放大 | 2400×1792 | 22.05 秒 |
| Chat 流式 | 1K | 3:4 | 预览 | 896×1200 | 12.05 秒 |
| Chat | 2K | 3:2 | 原图/放大 | 2528×1696 | 23.28 秒 |
| Gemini 原生流式 | 1K | 2:3 | 预览 | 848×1264 | 12.70 秒 |
| Gemini 原生 | 2K | 4:5 | 原图/放大 | 1856×2304 | 22.17 秒 |
| Responses | 1K | 5:4 | 预览 | 1152×928 | 13.28 秒 |

这轮实测验证了所有支持比例、两档下载路径、2K/4K 请求、并发隔离及两个协议的流式生图。不代表上游每次都能遵守比例；不符时会明确报错。

完整图片及请求报告保存在本地 `data/image-options-20261109/`（不提交凭据或运行数据库）。另外已验证：像素尺寸、不支持的比例和清晰度冲突均返回 400；Flash 文本冒烟返回 200 和 `OK`；`go test ./...`、`go vet ./...` 通过。

Windows 程序 SHA256：`32CD7CDC8243B716EC3BF8451DF4588ED5581CD702BE11B2C882B70D716E59A8`。旧程序备份为 `data/gemini-web2api-go.before-image-options.exe`。本轮部署目标是本机服务，未操作远程服务器。
