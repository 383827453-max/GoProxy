# 加固改造记录（pool-hardening）

基线：上游 `isboyjc/GoProxy` v0.4.1（最后代码提交 2026-04-04，此后无代码变更）。
本分支在**不改动对外功能形态**的前提下，修掉"能编译但跑不出代理 + 默认不安全"的问题，
并补齐部署与下游集成物料。

## 一、致命问题（已修）

| # | 问题 | 证据 | 修法 |
|---|---|---|---|
| 1 | 出口 IP 查询无全局限速，被 ip-api 免费额度打爆 → 所有代理因"无出口信息"被拒，池子长期 0 | 实测日志 `503 Too many open connections`；`fetcher.IPQueryLimiter` 在活路径上无调用者（死代码），`validator.getExitIPInfo` 自己直连 | 新增 `netguard.InitExitIPLimiter` / `WaitExitIP`，验证链路统一限速（默认 0.6 rps ≈ 36/分钟，低于 45/分钟免费档） |
| 2 | 一条验证要同时做「连通 + 延迟 + 出口 IP + 地理 + CONNECT」，几万候选按 45 次/分钟根本跑不完 | 实测 12 分钟 2.98 万候选、入池 0 | **两段式验证**：第一段高并发只做连通性+延迟+CONNECT（并发 300→80）；入池后由 `enrichExitInfo` 后台补齐出口位置并做地理过滤 |
| 3 | 源列表里的 `https://x.x.x.x:port` 被当成协议 `https` 入库 | 实测 1000+ 行 `unknown protocol https for ...` 刷屏 | `netguard.NormalizeProxyAddress`：http/https→http，socks4/4a/5/5h→socks5，未知 scheme / 非字面量 IP / 保留地址整行丢弃 |
| 4 | WebUI 会话令牌 = `sha256(纳秒时间戳)`，无服务端密钥、可枚举伪造 | `webui/server.go` 原 `newSession()` | 改 `crypto/rand` 256 位；随机源不可用则拒绝签发；Cookie 加 `SameSite=Lax`（HTTPS 下加 `Secure`） |
| 5 | 登录无退避、非常量时间比较 | 原 `hash != cfg.WebUIPasswordHash` | `subtle.ConstantTimeCompare` + 同 IP 失败退避（默认 10 次/10 分钟） |
| 6 | 访客可免登录提交订阅 URL → 服务端主动拉取（SSRF）+ 落盘 0644 | 原路由 `/api/subscription/contribute` 未走 `authMiddleware` | 改为管理员权限；URL 过 `netguard.GuardOutboundURL`；落盘改 0600、目录 0700 |
| 7 | 通过免费代理拉订阅时 `InsecureSkipVerify: true` → 链路中间人可替换订阅内容 | `custom/manager.go` 原 `fetchURL` | 移除跳过校验；代理不满足就失败重试，不降级信任 |
| 8 | 默认监听 `0.0.0.0` 且代理认证默认关闭 → 部署即公网开放代理 | 原 `http.ListenAndServe(":7777")`、`PROXY_AUTH_ENABLED=false` | 新增 `PROXY_BIND`/`WEBUI_BIND`（默认 `127.0.0.1`）；`ProxyAddr()`/`WebUIAddr()` 统一合成监听地址；容器镜像显式设 `0.0.0.0` |
| 9 | SOCKS5 认证用明文 `!=` 比较 | `proxy/socks5_server.go` | 改 `subtle.ConstantTimeCompare` |
| 10 | 仓库内提交了 13 MB macOS ARM64 二进制 `proxygo`（含作者家目录）与一份**含可用节点口令的订阅 YAML** | `git log --follow proxygo`；`subscriptions/sub_*.yaml` 内 38 条同密码 anytls 节点 | 从版本库移除并加入 `.gitignore`（`proxygo`、`subscriptions/`） |

## 二、依赖与工程化

- `golang.org/x/net` v0.38.0 → **v0.61.0**（修 `GO-2026-6617`：HTTP/2 HPACK 竞态致 server crash，`govulncheck` 确认命中符号）
- `github.com/mattn/go-sqlite3` v1.14.37 → **v1.14.52**；`golang.org/x/time` v0.15.0 → **v0.16.0**
- `go` 指令 1.25.0 → **1.26.0**；Dockerfile 构建镜像 `golang:1.25` → **`golang:1.26.9`**（否则构建必失败；固定补丁号以带上标准库安全修复）
- `govulncheck` 复检：改造前 9 条（含 `golang.org/x/net@v0.38.0` 的 GO-2026-6617）；改造后 x/net 条目消失，剩余 9 条全部来自**本机 go1.26.7 标准库**（net/http、crypto/tls、mime/multipart，修复于 go1.26.9）→ 部署脚本与镜像统一使用 go1.26.9
- 新增**单元测试**（原仓库 0 个）：`netguard`（地址归一化 / SSRF 判定 / 限速器）、`fetcher`（列表解析归一化）、`config`（监听地址合成 / 槽位 / 默认安全姿态）
- CI：新增 `test` job（`go vet` + `go test`），`build-and-push` 依赖它；镜像名改为 `${{ github.repository_owner }}/goproxy`（fork 后不再因推上游命名空间而 403）

## 三、新增能力

- `GET /api/export`：按协议/来源/延迟/等级/条数导出代理，`format=uri|plain|json`；
  鉴权 = 管理员会话或 `X-Export-Token`（`EXPORT_TOKEN`）。这是给下游（opencode2api / clash）的稳定接口。
- `deploy/`：`install-vm2.sh`（幂等部署 + 随机口令 + systemd 加固）、`goproxy.service`、
  `export-to-opencode2.sh`（幂等注入下游公开池 + 合并 + 热重载 + healthz 校验）、`README-DEPLOY.md`。

## 四、尚未处理（明确留给后续）

- sing-box 仍固定 1.13.5：1.14.x 调整了部分 outbound/route 字段语义，升级前必须用 `sing-box check` 回归生成配置。
- `checker/optimizer` 未做单元测试；`webui/dashboard.go` 为内联 HTML，未做前端收敛。
- 免费源本身质量决定池子上限；如需稳定高可用，应把订阅（自建节点）作为主力、免费源作兜底。
- 上游仓库 issue 关闭、PR 不合并，本分支需自行维护（建议以 fork 为主仓）。
