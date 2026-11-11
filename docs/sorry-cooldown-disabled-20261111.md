# 关闭 Google /sorry 固定冷却及复测（2026-11-11）

用户要求取消固定两小时等待并复测。新增独立配置 `disable_sorry_cooldown`，本地运行值设为 true，清除了此前的 google_sorry 冷却记录；没有把 proxy_cooldown_min 改为 0，因为该项原有的代理熔断语义是“0 不自动恢复”。其他运行配置保持原值。

## 修改行为

- 新收到 Google /sorry 且没有 Retry-After：返回 HTTP429，code=upstream_google_sorry，不写入固定冷却，不在同一请求中换号或重复生成。
- Google 明确返回 Retry-After 时仍遵守；普通上游 HTTP429 仍保留原退避机制。
- 管理页面新增“关闭 /sorry 固定冷却”开关，启动配置与运行配置同步支持。
- 开关只影响新触发；已有冷却不会自动删除。本次用户授权的取消操作另行清除了当前一条 google_sorry 记录。

源码提交 a57f03a。go test -count=1 ./... 与 go vet ./... 均通过。由干净构建目录编译，Windows 本地进程已重启，PID 35028，端口 18083。没有部署远端服务器。

## 真实结果（10:02 香港时间）

| 对照 | 结果 |
|---|---|
| 本地 Argon 低思考生成 | 0.562 秒返回 429 upstream_google_sorry，没有 Retry-After |
| 当前代理访问 Google /app，不带 Cookie | 302 到 www.google.com/sorry/index |
| 同代理同页面，带账号 Cookie | 同样 302 到 /sorry/index |
| 无凭证直连页面 | 15 秒连接超时 |

启动预热与生成请求均真实收到 /sorry；运行结束 egress_backoff 为空。因此现在失败来自上游拒绝，不是旧的两小时本地计时。账号 Cookie 是否携带没有改变当前页面请求结果，不能靠本地换账号证明恢复；上述对照尚不能把原因唯一归为 IP 信誉或 HTTP/TLS 请求特征。

当前项目只有一个配置代理出口：127.0.0.1:10808。没有已验证可用的备用出口，也没有找到已成功的恢复路径。后续需要以其他可用出口或同出口正常浏览器访问做对照；本次没有改动全局代理、账号 Cookie 或自动尝试验证码。

## 证据与回滚

`data/sorry-disabled-20261111/summary.json` 保存请求结果、运行配置差异、源码提交和运行二进制 SHA256。目录中 before.exe、before-runtime.json、before-blocks.json、before.gemini.db 为本地备份；数据库含凭证，不提交或对外分享。

配置恢复：在管理面板关闭“关闭 /sorry 固定冷却”，后续触发恢复原策略。源码回滚使用 git revert a57f03a 后重新构建。

二进制回滚：stop-local.ps1 后将 data/sorry-disabled-20261111/before.exe 复制回 gemini-web2api-go.exe，再运行 start-local.ps1；同时恢复 disable_sorry_cooldown=false。不要为回滚此设置覆盖整个数据库，以免把新刷新的 Cookie 回退。
