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

## 四、VM2 实部署与 opencode2api 集成（2026-10-10 实测）

环境：Azure VM（`ssh wb` → vm-2），Ubuntu 24.04.4，4 vCPU / 16 GB，Go 1.26.9，CGO + gcc。

| 阶段 | 结果 |
|---|---|
| 部署 | `install-vm2.sh` 幂等跑通：Go 工具链 → 编译（9.3 MB）→ 随机口令 env → systemd 加固单元 → 5 端口仅绑 127.0.0.1 |
| 池填充 | 从 0 → 60 条 active（HTTP 33 / SOCKS5 27），全部带出口 IP + 地理位置；日志无 `unknown protocol https` 刷屏 |
| 地理过滤 | 出口国家分布 US/RU/IN/DE/VN/BD…，CN 条目 0（`BLOCKED_COUNTRIES=CN` 生效） |
| 出口限速 | 12 分钟内完成两段式验证，无 `503 Too many open connections`（改造前本机实测必现） |
| 导出接口 | `/api/export` 401（无 token）/ 60 条（有 token）；`status=active` 过滤生效 |
| 下游集成 | 25 条注入 `proxies_goproxy.txt` → 合并进 `proxies.txt`（2034 条） |
| 端到端 | 经 opencode2api `:7872` 真实推理请求 3/3 HTTP 200；healthz `ok`、2032/2032 healthy |

集成中踩到并修掉的两个坑（已写入部署文档与补丁脚本）：

1. **注入被下一轮刷新覆盖**：`proxies_public.txt` 由 opencode2api 自己的
   `proxyscraper_refresh.py` 每轮重写 → 改为独立文件 `proxies_goproxy.txt` +
   给刷新器打一次性最小补丁（`deploy/patch-opencode2-refresh.py`，幂等、带备份与语法校验）。
2. **注释行导致下游整轮刷新中止**：补丁第一版把 `# >>> goproxy-pool:start` 标记行一起并入，
   刷新器 `valid_proxy()` 判为非法项并 `中止（不覆盖）` → 合并前过滤 `#` 与空行；
   同时清理了 `proxies_public.txt` 里历史残留的注入区块。

## 五、第二轮改进（三态健康 / 熔断复活 / 出口去重 / 入池节流）

按 `docs/peer-proxy-pool-research.md` 的优先级清单落地，全部带单元测试：

| 项 | 来源 | 实现 |
|---|---|---|
| P0-1 三态健康策略 | tongsq/proxy-collect | `checker.failureOutcome()` 纯函数：失败先置 `degraded` 保留，达到 `FAILURE_THRESHOLD`（默认 5，原硬编码 3）才移除；免费删除、订阅禁用 |
| P0-5 熔断复活（半开） | nfx/slrp + easy-proxies | `health_checker.reanimateDisabled()`：每轮取 `REANIMATE_BATCH` 个 `disabled` 免费代理重探，健康则 `EnableProxy + ResetFail + active`；探测成功即清失败计数 |
| P0-6 出口去重 / 同 C 段限制 | yunginnanet/prox5 拨号中间件 | 新增 `proxy/subnetTracker`（默认 60s 冷却、16 个 /24），HTTP 与 SOCKS5 两条选择路径均排除冷却网段，全池同网段时自动回退 |
| P0-4 探针去重（部分） | easy-proxies probe_scheduler | `validator.LookupExitInfo` 单飞：同地址并发只消耗一次外部 API 额度 |
| 入池节流（本轮实测驱动） | — | 实测 pending 以远快于出口验证的速度增长 → 新增 `MAX_PENDING`（默认 50）闸门 + 健康检查每轮 `DeleteStalePending(30)` |
| P1-9 源退避抖动 | slrp sources/try.go | 源禁用冷却加 0~50% 随机抖动，避免同时挂掉的源集体重试 |
| 可观测性 | — | `/api/pool/status` 追加 `ByStatus`（active/degraded/disabled/pending），填充日志新增"待验证积压"拒绝计数 |

VM2 实测（同一台机器，连续两轮健康检查）：

```
[health] 完成: 验证20 有效11 更新11 降级9 移除0 复活0   ← 旧实现会直接删掉这 9 个
[health] 🧹 清理超时候选(pending): 26 个
/api/pool/status → ByStatus {"active":85,"degraded":10,"pending":33}
出口多样性：连续 8 次经 :7779 请求，成功的 4 次出口 IP 落在 4 个不同 /24（同 C 段不重复）
```

## 六、尚未处理（明确留给后续）

- sing-box 仍固定 1.13.5：1.14.x 调整了部分 outbound/route 字段语义，升级前必须用 `sing-box check` 回归生成配置。
- `checker/optimizer` 未做单元测试；`webui/dashboard.go` 为内联 HTML，未做前端收敛。
- 免费源本身质量决定池子上限；如需稳定高可用，应把订阅（自建节点）作为主力、免费源作兜底。
- 上游仓库 issue 关闭、PR 不合并，本分支需自行维护（建议以 fork 为主仓）。
