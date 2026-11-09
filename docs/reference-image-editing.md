# 参考图编辑验证与 Responses 丢图修复

日期：2026-11-09。当前本地版本：`4.27.1-image-reference-v2`，地址 `http://127.0.0.1:18083`。第一轮修复版本为 `4.27.1-image-reference-fix`，第二轮完整补测和新增修复见文末。

## 结论与证据

Chat 的 `image_url`（Base64 data URL、HTTP 图片直链）和 Gemini 原生的 `inlineData`，本轮均能真正参考输入图编辑。旧版 `/v1/responses` 存在确定的丢图缺陷：用户 `content` 数组被提前转换为纯文本，导致 `input_image` 在附件提取前消失。该路径可以 HTTP 200 返回完全无关的图片。

已修复并重新部署本地服务：Responses 保留原始多模态内容，之后由 `collectImages` 提取、上传图片；空图片来源或只有不支持的 `file_id` 时返回 400，不再静默忽略。前一轮的清晰度与比例功能保持有效。

对照图使用两张独立绘制的卡片：A 包含 `R7Q2`、`583 / A`、圆形、左上黄三角、右下蓝方块；B 包含 `M9L5`、`926 / B`、菱形、右上黄圆、左下蓝方块。两张图用同一个提示词，只要求中央红色形状变紫，保留其余内容；提示词没有给出具体编号和角落布局。观察输出中的独有文字和布局，可区分真正参考原图和仅凭文本生成。

| 测试 | 结果 | 耗时 | 输出尺寸 |
|---|---|---:|---|
| Chat Base64，参考图 A，1K | 上传 1 图；改色并保留 A 的编号和布局 | 18.11 秒 | 1024×1024 |
| Chat Base64，参考图 B，1K | 上传 1 图；同样提示词保留 B 的不同编号和布局 | 15.91 秒 | 1024×1024 |
| Gemini inlineData，参考图 A，2K | 上传 1 图；原图/放大下载，正确编辑 | 26.73 秒 | 2048×2048 |
| Chat HTTP 直链，参考图 A，1K | 本地图片服务收到一次 GET；上传 1 图，正确编辑 | 13.48 秒 | 1024×1024 |
| 修复前 Responses，参考图 A，1K | HTTP 200，但没有图片上传日志；返回无关紫色海报 | 15.80 秒 | 1024×1024 |
| Chat 照片编辑，1K | 红色茶壶改蓝色，几何形状、桌面和构图明显保留 | 17.14 秒 | 1024×1024 |
| 修复后 Responses，参考图 A，1K | 上传 1 图，保留 A 的编号和布局并改色 | 19.58 秒 | 1024×1024 |
| 修复后 Responses 流式，参考图 B，2K | 上传 1 图，原图/放大下载，正确编辑；收到 response.completed | 30.22 秒 | 2048×2048 |

这是视觉语义一致性的验证，不是逐像素无损编辑承诺。第一轮图片直链测试使用本地可访问的 HTTP 图片服务，第二轮另补测公网 HTTPS 和重定向。尚未收到客户失败请求的 JSON，不能将所有外部“不生效”案例都归因于 Responses 缺陷，也未验证客户实际的 CDN、鉴权网址或过期链接。

图片、参考图、上传日志摘要和脱敏报告保存在 `data/reference-edit-20261109/`，其中 `report.json` 是修复前对照，`report-fixed.json` 是修复后结果。测试未把 API key 写入报告。

## 正确的请求形状

Chat：`POST /v1/chat/completions`，图片必须作为 `content` 数组的一个独立部分，不能仅把图片网址写进文本。

```json
{
  "model": "gemini-image",
  "messages": [{
    "role": "user",
    "content": [
      {"type": "text", "text": "只把参考图中红色部分改为蓝色，保留其余内容。"},
      {"type": "image_url", "image_url": {"url": "data:image/png;base64,替换为真实Base64"}}
    ]
  }],
  "image_size": "2K",
  "aspect_ratio": "1:1"
}
```

`image_url.url` 也可以填服务端能直接下载的 HTTP(S) 图片网址。

Responses：`POST /v1/responses`，修复版支持以下格式及 `stream: true`。

```json
{
  "model": "gemini-image",
  "input": [{
    "role": "user",
    "content": [
      {"type": "input_text", "text": "只把参考图中红色部分改为蓝色，保留其余内容。"},
      {"type": "input_image", "image_url": "data:image/png;base64,替换为真实Base64"}
    ]
  }],
  "image_size": "2K",
  "aspect_ratio": "1:1"
}
```

Gemini 原生：`POST /v1beta/models/gemini-image:generateContent`。

```json
{
  "contents": [{"role": "user", "parts": [
    {"text": "只把参考图中红色部分改为蓝色，保留其余内容。"},
    {"inlineData": {"mimeType": "image/png", "data": "替换为真实Base64，不含data前缀"}}
  ]}],
  "generationConfig": {"imageConfig": {"imageSize": "2K", "aspectRatio": "1:1"}}
}
```

## 回归与部署

回归测试先在旧代码上复现失败，再在修复后通过：普通/流式 Responses 的错误图片 Base64 必须在请求校验阶段返回 400；缺失来源的附件不能静默丢弃。`go test ./...` 和 `go vet ./...` 通过。部署后另测：只有 `file_id` 的参考图返回 400；纯文本 Responses 返回 200 和 `OK`。

第一轮程序 SHA256：`315601F415154B98E407B376FE56228607F31B354A9BC65A4095BDCD7237EAED`。第一轮旧程序备份：`data/gemini-web2api-go.before-reference-fix.exe`；旧日志备份：`data/before-reference-fix.stderr.log`。本次只部署本机，未操作远程服务器。

## 第二轮：协议、流式、多图、格式与异常输入

按用户“都测测”的要求完成补测。正常样本逐张查看输出，不仅检查 HTTP 状态；单图保留原图的独有编号和布局，多图检查 A、B 两张图的编号和形状均出现。

| 能力 | Chat | Gemini 原生 | Responses |
|---|---|---|---|
| Base64 单图 | 普通/流式通过 | inlineData 普通/流式通过 | 普通/流式通过 |
| HTTP 图片直链 | 普通/流式通过 | fileData 普通/流式通过 | 普通/流式通过 |
| 公网 HTTPS 图片直链 | 通过，15.88 秒 | 通过，14.86 秒 | 通过，14.75 秒 |
| 两张参考图共同生成 | 通过，16.59 秒 | 2K 通过，25.50 秒 | 流式通过，16.62 秒 |
| 302 跳转至图片 | 修复后通过，18.91 秒 | 修复后通过，12.31 秒 | 流式通过，20.76 秒 |
| 非法图片输入 | 7 类均返回 400 | 7 类均返回 400 | 7 类均返回 400 |

格式补测：PNG、JPEG、WebP 均成功；Chat 的 `image_url` 对象格式和字符串格式均成功；Gemini 故意将 JPEG 标成 `image/png` 后仍可正常编辑，因为服务端会识别实际 MIME。多图输出保留两张图的不同编号和角落形状，但可能出现轻微颜色/构图变化，不属于逐像素复制。

公网 HTTPS 使用 [Google 公开标志图片](https://www.gstatic.com/images/branding/googlelogo/2x/googlelogo_color_272x92dp.png)，只要求将红色区域改紫色。三个接口均上传了图片，返回 1750×592 的改色结果，字形、排列和其他字母颜色保留。单一公开域名成功不代表所有客户网址、私有鉴权和过期签名都可访问。

### 第二轮发现并修复的问题

1. **直链重定向未跟随**：旧版收到 302 直接返回 400。新增参考图专用的有限跳转逻辑，最多跟随 5 次，支持相对 Location 和 301/302/303/307/308；不改变 Google 上游客户端的全局重定向策略。循环、非 HTTP(S) 目标会报错。
2. **HTML 被误当图片上传**：旧版对 HTTP 200 的登录页改用默认 PNG MIME 并上传，实测最终 502。现在按字节识别图片类型；PNG/JPEG/GIF 进一步检查图片头。HTML、普通文本和图片头损坏在上传前返回 400；常见图片 MIME 标错时按实际类型纠正。

21 组异常请求 = 三个协议 × 七种输入：404、403、空图片、非法 Base64、HTTP 200 HTML、伪装成图片的 Base64 HTML、截断 PNG。新版全部返回 400，全部没有 `[vision]` 上传日志，不消耗生成请求。

新版本地实测：6 组正常输入 + 21 组异常输入 + 3 组公网 HTTPS，共 30 组结果均符合预期。此前补测中的常规单图流式、多图结果也已保存。完整回归 `go test ./...`、`go vet ./...` 通过；重定向单元测试覆盖直连和代理两条下载路径。测试中顺便修正了原视频参考图测试里本来就损坏的 1×1 PNG 样本。

本轮数据在 `data/reference-matrix-20261109/`：`report.json` 是修复前矩阵，`report-v2.json` 是新版 27 组测试，`report-public.json` 是公网 HTTPS 测试。

当前程序 SHA256：`95FCA1C4CFFD78AA8966BA8103A058578B08D3AD1D9FEBE8BB0C4C08CF3BAD63`。本轮旧程序备份：`data/gemini-web2api-go.before-reference-v2.exe`；旧日志：`data/before-reference-v2.stderr.log`。本地服务端口仍为 18083，全局生图放大设置未修改。
