# new-api 文本与生图兼容、性能复核

日期：2026-11-13（Asia/Hong_Kong）。用户确认目标是本地 **new-api**，不是独立的 Sub2API 项目。

## 结论

基础协议可接入本地 new-api，但当前不能把旧 `gemini-3.6-flash` 作为可用文本型号：本轮 4 次请求的上游实际均返回 `4 Hydrogen`，严格模型校验按预期返回 429。错误中的“额度耗尽”是既有通用提示，不能据此确认原因就是额度；可能涉及上游模型变更，本轮未证明具体原因。没有关闭严格校验，也没有把 Hydrogen 冒充 Flash。

当前目录已有模型目录功能及已配置的 `gemini-4-hydrogen`，单独使用该真实型号复测 4/4 成功。生图 `gemini-image` 4/4 成功。此结论覆盖并发 2 的小规模实测，不代表高并发或长期稳定性。

## 协议核对

对照本地 `../new-api`，Git HEAD 为 `8faf1d45e`；没有修改 new-api。其 `relay/channel/sub2api/adaptor.go` 继承 `newapi.Adaptor`，复用相同协议分发。

| 用途 | 接口与响应 | 检查结果 |
|---|---|---|
| 文本 Chat | `/v1/chat/completions`；JSON 或 SSE，结束 `[DONE]`；可选 usage chunk | 模拟合同与真实 Hydrogen JSON/SSE 均通过 |
| 图片生成 | `/v1/images/generations`；`created`、`data[].b64_json`；SSE `image_generation.completed` | JSON/SSE 均通过，图片完成解码 |
| 参考图编辑 | `/v1/images/edits`；JSON 或 multipart | 既有自动回归通过，本轮未追加真实编辑请求 |
| Gemini 原生 | `/v1beta/models/{model}:generateContent`、`:streamGenerateContent?alt=sse` | `candidates`、`inlineData`、`usageMetadata` 均通过 |
| Responses | `/v1/responses` | 既有实现和回归保留，本轮没有真实 Responses 请求 |

另外使用 new-api 自己的 `relaykit/dto` 和 `kitutil` 解析本次处理函数导出的 Images JSON/SSE、Gemini inlineData、Chat SSE、嵌套推理用量，全部通过。不是仅用自定义字典断言格式，也不是完整启动 new-api 的端到端测试。

本地 new-api 的 `common/init.go` 默认 `STREAMING_TIMEOUT=300` 秒、`RELAY_TIMEOUT=0`，`STREAM_SCANNER_MAX_BUFFER_MB=128`；`relay/helper/stream_scanner.go` 使用该上限。它能容纳本轮图片 SSE，但实际部署的环境变量、反向代理超时和计费配置仍需与此区分。

## 本次优化

1. Gemini 原生及 Images 直接使用文本和二进制产物，不再先构造一份最终会被丢弃的 base64 Markdown。Chat/Responses 的图片 Markdown 输出保持原样。
2. 用小型 data URL 头正则和线性字节扫描代替对整个大型 base64 字符串运行正则。清理结果与旧规则一致，图片正文不受影响，base64 仍不作为文本 token 计费。
3. Images 和 Gemini 错误处理识别包装后的错误，保留 429/400 及 Retry-After；Gemini 已开流后的错误事件保留实际错误分类，不再一律标为 502。Images 的 Retry-After 整数秒不再多加一秒。

模型目录功能、版本字符串及其 UI 是工作区另有的修改，不属于本次优化。未修改账号设置、模型配置、严格检查开关或限速配置。

## 性能基准

Windows amd64，i5-13600KF；每组两次，`-benchtime=300ms`。旧路径在基准中保留用于比较。

| 测试 | 旧路径 | 优化后 |
|---|---:|---:|
| 4 MiB base64 的日志/用量清理 | 105.3–106.7 ms | 1.91–1.92 ms |
| 3 MiB 二进制图片的原生响应准备（含最终 base64/JSON） | 116.0–118.3 ms | 4.92–5.08 ms |
| 上述响应准备的累计内存分配 | 42.0 MB/op | 14.3–15.1 MB/op |

第二项减少约 96% 的本地准备时间和约 64%–66% 的累计分配量；不是峰值 RSS，也不是 Google 生成耗时缩短 96%。实际响应还受上游生成、下载、网络和账号轮转影响。

## 真实接口复测

临时启动当前工作区构建，返回版本 `4.27.2-model-catalog`。测试结束后已停止该临时进程；未替换常用可执行文件或部署远程服务。运行数据与图片位于被 Git 忽略的 `data/`。

| 请求 | 成功数 | 总耗时范围 |
|---|---:|---:|
| 旧 `gemini-3.6-flash`，Chat/Gemini 各 JSON+SSE | 0/4，全部 HTTP 429 | 5.5–41.6 秒 |
| `gemini-image`，Images/Gemini 各 JSON+SSE | 4/4 | 14.2–17.8 秒 |
| `gemini-4-hydrogen`，Chat/Gemini 各 JSON+SSE | 4/4 | 4.2–13.0 秒 |

每轮并发 2。Hydrogen 两次流式首个事件分别 5.14、6.33 秒。所有成功文本回复含指定标记及 usage，成功流式有完整结束标志；四张图片均完成解码，尺寸为 1376×768 或 1408×768（请求未指定比例）。服务日志上游模型：旧 Flash 请求返回 `4 Hydrogen`；Hydrogen 请求实际为 `4 Hydrogen`；生图为 `Nano Banana 2` / `Nano Banana 2.1`。

证据：

- `data/newapi-smoke-20261113-192433/results.json`：第一轮八个请求。
- `data/newapi-smoke-20261113-192618/results.json`：四个 Hydrogen 文本请求。
- 数据库请求日志 ID 721–732。
- `data/newapi-contract/`：当前处理函数导出的模拟合同响应；消费者程序位于本地 `../newapi-contract-check`。

## 接入建议和边界

- new-api 文本模型使用已验证的实际 ID。若业务必须提供 Flash，目前应先恢复并验证对应的真实上游路由，不能靠关闭 `strict_model` 宣称恢复。`gemini-4-hydrogen` 是本轮验证可用的另一型号，不保证与 Flash 同等能力或价格。
- 生图优先使用 `/v1/images/generations` 或 Gemini 原生接口；`n=1`，图片按张配置计费。Images 响应没有虚构的 Google 图片 token 用量，不宜只按文本 token 对图片计费。
- `response_format=url` 返回 data URL；需要外部 HTTP 图片地址的调用方仍需自行存储。像素尺寸参数映射为比例，不保证精确分辨率；遮罩、指定透明背景、渐进预览等既有限制未改变。
- 读取到的运行配置仍为每出口并发 5、每分钟 30、每小时 80；这只是本地限额，不是持续吞吐保证。生图和文本共享出口预算，本轮没有调高阈值，也没有压力测试上游风控。
- `request_timeout_sec=180` 是每次请求超时，`retry_attempts=4`，准备和重试可能使端到端等待更长。流式首块之前保留正常 HTTP 错误，生图 SSE 为最终结果事件，没有中间图片预览；反向代理需要允许等待最终图片。

## 验证命令

```powershell
go test ./...
go vet ./...
go test ./internal/app -run '^$' -fuzz FuzzStripDataURLs -fuzztime=5s -parallel=2
go test ./internal/app -run '^$' -bench 'BenchmarkStripImageDataURL|BenchmarkNativeImagePreparation' -benchmem -benchtime=300ms -count=2
go build -o data/gemini-web2api-go-newapi-optimized.exe .

$env:WEB2API_CONTRACT_DIR = "$PWD/data/newapi-contract"
go test ./internal/app -run TestNewAPIExportContract -count=1
# 在相邻 newapi-contract-check 目录运行 go run .

# 需已启动本地服务；读取本地配置和密钥，不输出凭证
python scripts/check-newapi-flash-images.py
python scripts/check-newapi-flash-images.py --text-model gemini-4-hydrogen --only text
```

完整测试、vet、Windows 构建及 new-api DTO 合同解析通过。模糊测试执行 41,107 次，无语义差异。实时实测的成功/失败按上表记录，不将 Flash 的四次失败计为协议成功或可用性通过。
