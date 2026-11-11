# 浏览器与独立 HTTP 客户端同代理对照（2026-11-11）

## 浏览器观察

独立 Chrome 使用显式 --proxy-server=http://127.0.0.1:10808 启动，用户截图显示匿名 Gemini 页面正常；Cookie 尚未导入。原生控制工具无法可靠识别该窗口 URL，未自动操作其登录或 Cookie。

之后使用已连接的 Edge 标签页。Windows 系统代理为 127.0.0.1:10808，无 PAC；Edge 主进程没有 --no-proxy-server 或另一个 --proxy-server 参数，系统绕过列表仅本地网段。通过公网出口查询站点的浏览器可见结果与项目代理请求结果比对，两者 IP 相同；证据中仅保存哈希，不公开实际 IP。这个站点的相同 IP 不能单独排除按目标域名分流的复杂代理规则。

Edge 本身已有 Google 登录会话，页面可正常加载并显示历史对话。点击 4 Argon 出现订阅升级提示，未购买或修改订阅。因此使用既有“自动”模式发送无敏感信息的短测试，页面实际返回 BROWSER_PROXY_OK。未把自动模式结果算作 Argon 成功，也未确认该 Edge 账号与池中账号一致。

## 独立 HTTP 客户端对照

通过数据库中同一个代理 #1 对 https://gemini.google.com/app 做八次有界请求，分别组合：Go net/http / 项目已有 tls-client Chrome_146，原有 CORS 请求头 / 页面导航请求头，不带 Cookie / 账号 #8 Cookie。凭证仅在进程内读取。所有请求禁止跟随跳转，不执行验证码，不更换出口。

| 带池账号 Cookie | 客户端 | 请求头 | HTTP | 跳转 |
|---|---|---|---:|---|
| 否 | Go net/http | 原有 CORS | 302 | /sorry/index |
| 否 | tls-client Chrome_146 | 原有 CORS | 302 | /sorry/index |
| 否 | Go net/http | 导航 | 302 | /sorry/index |
| 否 | tls-client Chrome_146 | 导航 | 302 | /sorry/index |
| 是 | Go net/http | 原有 CORS | 302 | /sorry/index |
| 是 | tls-client Chrome_146 | 原有 CORS | 302 | /sorry/index |
| 是 | Go net/http | 导航 | 302 | /sorry/index |
| 是 | tls-client Chrome_146 | 导航 | 302 | /sorry/index |

八次均使用 HTTP/2，均 302 跳转至 www.google.com/sorry/index，没有拿到会话 token。仅换 TLS 客户端或导航请求头未恢复。

## 目前结论

当前存在普通浏览器会话可生成、独立客户端被拒的可观察差异；不能再仅凭 /sorry 就断言该出口对所有访问方式均不可用。但同账号 Cookie 对照尚未完成，尚不能唯一归因于 TLS 指纹、Cookie、浏览器初始化、脚本执行或其他会话状态。

Cookie 导入没有完成：现有浏览器连接未提供 Cookie 管理接口。已询问用户是否明确改用 Playwright 创建隔离浏览器并导入项目账号 Cookie，或由用户手动导入；没有未经确认替换日常 Edge 的 Cookie。

脚本：scripts/transport-probe/main.go，提交 a17e9a5。实测输出：data/browser-transport-probe-20261111.jsonl；出口比对：data/browser-proxy-exit-comparison-20261111.json。服务源码、服务配置及代理节点没有改动。
