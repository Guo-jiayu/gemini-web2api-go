# 保活 429 修复、验证与回滚

日期：2026-11-10。范围：本地 WEB2API；未部署远端服务器，未推送 Git。

## 修复记录

| Git 提交 | 内容 |
| --- | --- |
| `2220f28` | Cookie 按字段合并、条件更新；晚返回的请求不能覆盖较新的凭证；写回错误明确返回 |
| `7a176ef` | 保留保活页面及 429 响应已取得的 Cookie；解析 Retry-After / identity.hfcr；限制单次刷新请求时间；拒绝无新凭证的假成功 |
| `b9f3daf` | 数据库租约协调同账号保活；持久化成功时间、下次刷新和错误；正常账号每轮仅一次 POST；网络/出口错误不触发凭证刷新；保活独立于统计调度 |
| `2e950bc` | 截止时间按秒向上取整，避免缩短 Retry-After；覆盖小数秒边界的回归测试 |

修复前基线：`624d326`，保留分支 `rollback/keepalive-before-20261110`。
原有未跟踪文件没有纳入这些提交。

## 运行行为

首次导入后自动续期。已含 `__Secure-1PSID` 的账号每轮先刷新 TS，并保存响应包含的 CC，不再立即重复请求 iframe POST。旧凭证缺少该项时仍保留 iframe 路径。

后台、管理接口和鉴权恢复使用同一数据库租约。成功后默认等待 600 秒；429 至少等待响应指定时间，网络异常从 60 秒开始递增退避。重启不会清空这些时间。

新增 `account_rotations` 表，不修改或重建原有账号表。管理接口的账号条目新增 `keepalive`，包含 `last_attempt`、`last_success`、`next_attempt`、`lease_until`、`failures`、`last_error`。冷却期手动保活返回 `deferred: true`，不会再次访问 Google。

共享同一个数据库的实例可协调刷新；不同数据库中的同一份凭证仍不能协调。账号主动退出、会话撤销或上游认证政策变化仍可能要求重新登录。

## 已完成验证

- 全量 `go test -count=1 ./...`、`go vet ./...` 通过；独立干净检出也通过。
- 回归测试覆盖旧响应覆盖、并发字段合并、同账号并发单次 POST、冷却持久化、429 部分 Cookie 和退避、租约过期恢复及过期拥有者不能释放新租约。
- Windows amd64、Linux amd64 编译通过。Linux 仅编译，未在服务器实际运行；本次没有运行 race 检测。
- 15:35:27–15:35:29 首轮自动续期：9/9 成功，TS 和 CC 实际写回，0 次 429。
- 12 个并发手动保活请求全部返回 deferred，刷新时间和 Cookie 均未变化。
- 15:36:20 重启后再次检查：9 个账号的刷新时间与 Cookie 均未变化，冷却仍有效。
- 真实文本 HTTP200，44.125 秒（含自动重试）；真实生图 HTTP200，17.484 秒，解码图片 1376×768。
- 15:45:28–15:45:30 第二轮自然自动续期：9/9 成功，实际再次更新 TS/CC，账号身份未变；两轮共 18/18 次成功，修复后日志 0 次保活 429。
- 第二轮后 9/9 账号登录页 HTTP200 且有登录 token；最终版本真实文本 HTTP200，27.594 秒（含一次首输出超时重试）；生图 HTTP200，18.719 秒，实际 JPEG 1376×768，55259 字节。
- 最终程序于 15:44:00 启动，PID 55952，监听 `127.0.0.1:18083`。测试结束保持运行。跨重启和一个完整十分钟周期已验证，多日无人值守尚未验证。

运行配置保持 5 并发、30 RPM、80 RPH。本轮不以增加上游请求频率解决保活问题。

首轮全量检查曾被历史 overlay 探针源文件和沙箱套接字权限阻挡。探针改为 `.go.txt` 保存，并修正 overlay 引用；在正常本机权限及干净检出中复验通过。

## 构建与证据

部署源码提交：`2e950bc`（完整提交及最终二进制哈希见 deployment-manifest.json）。
干净构建检出：`E:\python_code\sparkcode\gemini-keepalive-build-20261110`。
最终 Windows 程序 SHA256：`0010387a554d6fcc2434a63ee1d0d1aac726e611eea6018b34168937ce79ad00`。

Go 在本机的独立 linked worktree 构建中未自动嵌入 VCS 信息，因此用 `data/keepalive-fix-20261110/deployment-manifest.json` 记录干净源码提交、Git tree 和二进制哈希。不能只用固定版本字符串判断是否部署了这次修复。

测试结果、数据库摘要、日志及图片保存在 `data/keepalive-fix-20261110/`。该目录包含敏感数据库备份，已被 Git 忽略，不应公开上传。

## 回滚

下面命令在 `E:\python_code\sparkcode\gemini-web2api-go` 执行。先确认没有在途生成请求，再停止服务。

恢复本轮开始前的实际运行程序：

```powershell
.\stop-local.ps1
Copy-Item -LiteralPath 'data\keepalive-fix-20261110\gemini-web2api-go.before.exe' -Destination 'gemini-web2api-go.exe'
.\start-local.ps1
```

日常回滚程序时应保留当前 `data/gemini.db`，避免把已经更新的 Cookie 回滚为旧值；旧程序会忽略新增表。`before.gemini.db` 仅作灾难恢复备份。

如需同时撤销源码修复并保留 Git 历史：

```powershell
git revert 2e950bc b9f3daf 7a176ef 2220f28
```

源码撤销不会自动替换正在运行的程序。可按上面的方式恢复备份程序，或重新编译、停止服务后替换并启动。回滚将重新引入原来的重复刷新和并发覆盖缺陷。
