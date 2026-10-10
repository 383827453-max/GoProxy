# 代理池 / 代理网关 7 仓库实现经验对比 —— 面向 GoProxy 改造

## 0. 证据边界（先说清哪些是读到的，哪些是推的）

| 项 | 说明 |
|---|---|
| 读取方式 | GitHub REST API（tree / commits / releases）+ `raw.githubusercontent.com` 全量拉取源码到 `%TEMP%\proxres\` 后逐文件阅读，共读 60+ 源文件 |
| 已实际读到 | slrp（16 文件）、socks5-proxy（11）、easy-proxies（关键 8）、PoolX（server.go / config / README / service.go 摘读）、prox5（10）、proxy-collect（关键 12）、phpgao/proxy_pool（关键 10） |
| 未取到 | PoolX `backend/internal/domain/clash/*.go`、`transport/http/system/*`（路径猜测失败，无 tree 全量）。其对外接口结论来自 `transport/http/server.go` 路由注册 + README |
| 路径记录 | jsdelivr CDN 对 `.go` 返回 `application/octet-stream` → web_fetch 不支持，改用 `Invoke-WebRequest` 直连 raw；raw 首次请求偶发 `fetch failed` → 重试成功；GitHub API 未认证限额 60/h 触发过一次 403 → 改用 raw 绕开 |

---

## 1. 速览对比表

| # | 项目 | 语言 / 依赖 | ★ / fork / open issues | 最后提交 | Release | 存储 | 对外入口 |
|---|---|---|---|---|---|---|---|
| 1 | [nfx/slrp](https://github.com/nfx/slrp) | Go；zerolog / bluemonday / uarand / htmltable / maxminddb / wireguard-go / React UI | 198 / 30 / **56 open** | `7707ba5` 2026-09-21（dependabot 依赖 bump；最后人工提交 2024-05-25） | v0.3.0 (2023-07-30)，有 goreleaser + release workflow | 磁盘 gob 状态快照（跨重启）+ 内存分片池 | HTTP 代理 `localhost:8090`（MITM）；UI/REST `localhost:8089` |
| 2 | [yukkcat/socks5-proxy](https://github.com/yukkcat/socks5-proxy) | Go，**纯标准库，零外部依赖**（go.mod 仅 module 行） | 195 / 89 / 0 open | `c43a737` 2026-06-18 | 无 | 内存 | SOCKS5 `127.0.0.1:1080`；面板 `127.0.0.1:8080` |
| 3 | [daimon3332/easy-proxies](https://github.com/daimon3332/easy-proxies) | Go + sing-box（sagernet）；SQLite；WebUI | 196 / 36 / 2 open | `ea88a5d` 2026-10-06 | **v2.5.0 (2026-10-06)**，v1.0→v2.5 共 20+ 版，迭代最活跃 | SQLite / 文件（state_db + nodes.txt + settings） | mixed 单端口 `2323` + 多端口 `base_port=24000` 递增 + 管理 API `9091` |
| 4 | [Rain-kl/PoolX](https://github.com/Rain-kl/PoolX) | Go(Gin+GORM) + React/TS；Mihomo 内核 | 294 / 42 / 0 open | `259b85d` 2026-09-01 | **v1.1.2 (2026-09-01)** | SQLite（默认）/ Postgres | 管理 API `127.0.0.1:8000`；Mihomo 内核多端口（每端口=一个池 profile） |
| 5 | [yunginnanet/prox5](https://github.com/yunginnanet/prox5) | Go；ants / concurrent-map / Rate5 / go-socks5 | 81 / 8 / 3 open | `d3b01cd` 2024-07-19 | v0.9.52 (2023-10-28) | 内存（proxyMap + 4 条 list） | **库**：Dial/DialTimeout/DialContext + StartSOCKS5Server + http.RoundTripper |
| 6 | [tongsq/proxy-collect](https://github.com/tongsq/proxy-collect) | Go；gin / gorm / gost / go-lib | 195 / 48 / 3 open | `327c996` 2024-06-23 | 无 | **Redis 或 MySQL 双实现**（dao 接口切换） | API `8090`（token 鉴权）+ 隧道代理多端口（内嵌 gost，示例 8888） |
| 7 | [phpgao/proxy_pool](https://github.com/phpgao/proxy_pool) | Go；gin / go-redis / robfig-cron | 77 / 12 / 1 open | `6def0fe` 2021-11-09（**停更约 5 年**） | 无 | Redis（Hash + Score） | API `8088` + 正向代理 `8089`（无认证） |

---

## 2. 逐项目拆解

### 2.1 nfx/slrp —— 工程完整度最高，熔断/复活模型最值得抄

**代理来源管理**
- 源统一抽象为异步生成器 `Src{Generate(ctx) <-chan Signal; Err(); Len()}`，见 `sources/try.go`。
- 源出错随机退避再重试（不是固定间隔）：
```go
// sources/try.go:117-121
if err != nil {
    log.Trace().Err(err).Msg("intermediate source failure")
    sleep := rand.Intn(60)
    next = time.Now().Add(time.Duration(sleep) * time.Second)
    continue
}
```
- 源错误分级：`sourceError{skip:true}` 表示"跳过重试直接退出"，并可携带出错代理回传池子。
- 多源合并 `mergeSrc`：每源一 goroutine 转发到同一 out channel，`Len()` 供进度估算。
- **来源去重靠"代理×源"二维表**：`probe/internal.go` 的 `SeenSources map[pmux.Proxy]map[int]bool`，README 明确"保留 per-source exclusive proxies"。

**验证策略**
- 三策略可配：`simple`（默认，随机挑一个回显 IP 的站）/ `headers`（查 X-Forwarded-For 泄漏）/ `twopass`（先 simple 再 headers），`checker/checker.go:85-98`。
- 校验站 7 个，每次**随机挑一个** → 天然分摊单站压力：
```go
// checker/checker.go:190-193
func (f federated) Check(ctx context.Context, proxy pmux.Proxy) (time.Duration, error) {
	choice := rand.Intn(len(f))
	return f[choice].Check(ctx, proxy)
}
```
- **验证前先探测本机出口 IP**（`thisIP()`），响应体里出现本机 IP ⇒ 透明代理，直接 `ErrNotAnonymous`：
```go
// checker/checker.go:242-244
if strings.Contains(body, sc.ip) {
    return ErrNotAnonymous
}
```
- 限流/封禁页识别（决定"进重试队列"还是"进黑名单"）：
```go
// checker/checker.go:236-241
if strings.Contains(body, "client does not have permission to get URL") { return errGoogleRatelimit } // temporary → 重试队列
if strings.Contains(body, "Cloudflare") { return errCloudFlare }                                      // temporary → 重试队列
```
- 超时 5s 可配；`temporary` 错误进 reverify 队列最多 5 次，其余进 blacklist 永不再探。

**存储与调度**
- `pool/shard.go` 分片，随机挑分片降锁竞争；`pool/entry.go` 每代理一个 entry。
- **滑动窗口计数器**：5 分钟短窗 + 24 小时长窗 × (Offer/Success/Timeout/Failure)，另有 `HourOffered[24]/HourSucceed[24]` 按小时成功率画像。
- 三种排序策略（`pool/sort_strategy.go`）：路由用 `fastestUnseen`、展示用 `sortForDisplay`、池饱和用 `saturate`：
```go
// pool/sort_strategy.go:11-22
func fastestUnseen(entries []*entry) {
	sorter.Slice(entries, func(i int) sorter.Cmp {
		e := entries[i]
		return sorter.Chain{
			sorter.IntAsc(e.ReanimateAfter.Unix()),
			sorter.IntAsc(e.Offered),
			sorter.IntDesc(e.SinceLastSeen().Round(time.Minute)),
			sorter.IntAsc(e.Speed.Round(time.Second)),
		}
	})
}
```
- 熔断/复活三件套在 `pool/entry.go`：`ConsiderSkip`（offer 配额 + 连败 3 次 → 冷却 30s）、`MarkFailure`（写 `ReanimateAfter`）、`ReanimateIfNeeded`/`ForceReanimate`、`Eviction`（超时阈值 / 失败阈值 / 复活次数阈值三级淘汰）。

**对外接口**
- HTTP 代理 `localhost:8090`（`serve/http_proxy.go`，自签 CA MITM + CONNECT 隧道，`handleConnect` 走 `Hijacker`）。
- UI + REST `localhost:8089`：`GET /api`、`/api/dashboard`、`/api/pool`、`POST|DELETE /api/refresher/{source}`、`/api/history`、`/api/history/{id}`、`/api/reverify`、`/api/blacklist`。
- **每个响应带诊断头**（`patterns/rotate.go:66-71`）：`X-Proxy-Serial` / `X-Proxy-Attempt` / `X-Proxy-Offered` / `X-Proxy-Succeed` / `X-Proxy-Through`。
- 池耗尽返回 **552** 做背压，客户端侧据此重试（`sources/util.go:163-167`）。

**抗限流 / 抗封禁**
- 每次 attempt 随机换分片换代理；失败 suspend 5 分钟。
- 源级随机退避 0-60s，源内错误 0-15s。
- `blockers` 列表识别云封禁页（cloudflare / contentkeeper.net），命中即判定源被拦。
- MaxMind GeoIP 支持按国家筛选。

**最值得抄**
1. `checker/checker.go:233-252 validate()` —— 透明代理检测 + 限流页识别（决定重试 vs 拉黑）。
2. `pool/entry.go:155-194 ConsiderSkip()` + `pool/sort_strategy.go fastestUnseen` —— 熔断 + 冷却 + 复活。
3. `sources/util.go:143-222 req.Do()` —— 统一请求封装：552 重试、blocker 检测、空 body 校验、`ExpectInResponse` 断言。

---

### 2.2 yukkcat/socks5-proxy —— 195★ 的极简样板，零依赖

**代理来源管理**
- 单 URL + 正则抓取 + 内存去重：
```go
// scraper.go:12
var proxyRegex = regexp.MustCompile(`socks5://(\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}):(\d+)`)
// scraper.go:51-59
seen := make(map[string]bool)
for _, m := range matches {
    addr := m[1] + ":" + m[2]
    if seen[addr] { continue }
    seen[addr] = true
```
- 注册 `file://` scheme（社区 PR #1）→ **本地文件即源**，离线可测：
```go
// scraper.go:30-31
t := &http.Transport{}
t.RegisterProtocol("file", http.NewFileTransport(http.Dir("/")))
```
- 无源健康/熔断，唯一兜底是"池空 → 立刻触发刷新"。

**验证策略**
- **手写 SOCKS5 握手**（不引 `x/net/proxy`）：greeting `05 01 00` → CONNECT `www.google.com:80` → `GET /generate_204` → 判首 4 字节 `"HTTP"`：
```go
// checker.go:80-82
req := []byte{0x05, 0x01, 0x00, 0x03, byte(len(target))}
req = append(req, []byte(target)...)
req = append(req, 0x00, 0x50) // port 80
// checker.go:107
return string(respBuf[:4]) == "HTTP"
```
- 先 `ip-api.com/csv/<ip>?fields=country,city` 查 geo，CN/HK 直接丢（因为上不了 Google）。
- 并发用信号量，默认 20：
```go
// checker.go:26-34
sem = make(chan struct{}, maxConcurrent)
for _, p := range proxies {
    wg.Add(1); sem <- struct{}{}
    go func(px Proxy) { defer wg.Done(); defer func() { <-sem }()
```
- 超时 `-check-timeout` 默认 10s。

**存储与调度**
- 内存 `[]Proxy` + `current int` 索引。
- 轮换 = **定时轮换（3-6 分钟随机）+ 粘性**，不是每请求换：
```go
// main.go:59-70
go func() {
    for {
        delay := 3*time.Minute + time.Duration(rand.Intn(4))*time.Minute
        time.Sleep(delay)
        if pool.Size() == 0 { TriggerRefresh() } else if pool.Size() > 1 { pool.SwitchNext() }
    }
}()
```
- 非阻塞手动刷新（channel 满即丢弃，防刷新风暴）：
```go
// main.go:104-111
func TriggerRefresh() {
	select {
	case refreshChan <- struct{}{}:
	default: // already pending
	}
}
```

**对外接口**：SOCKS5 `1080` + 面板 `8080`；`GET /api/status`、`POST /api/refresh`、`GET /api/switch[?index=N]`；**无认证**。

**抗封禁**：仅出口 IP 去重（scraper 层）+ CN/HK 过滤；无 C 段限制、无退避。

**最值得抄**
1. `scraper.go:29-38` —— `file://` 本地文件当源（离线回归测试直接用）。
2. `main.go:104-111 TriggerRefresh()` —— 非阻塞触发语义。
3. `checker.go:61-108 checkGoogle()` —— 零依赖手写 SOCKS5 校验，GoProxy 的 SOCKS5 validator 可省一个依赖。

---

### 2.3 daimon3332/easy-proxies —— 与我们形态最接近（订阅导入 + 多端口节点池网关）

**代理来源管理**
- 订阅 + 本地节点 + 链式 profile，全部 YAML：
```yaml
# config.example.yaml:28-36
subscription_refresh:
  test_204: true
  site_targets: []
  enabled: false
  interval: 24h
  timeout: 30s
  health_check_timeout: 5s
  drain_timeout: 30s
  min_available_nodes: 1
```
- 订阅抓取独立包 `internal/subfetch/fetch.go`：URL 只允许 http/https、10MB body 上限、**状态码可重试分类**：
```go
// internal/subfetch/fetch.go:26-28
func (e *HTTPStatusError) Retryable() bool {
	return e.Code == http.StatusRequestTimeout || e.Code == http.StatusTooManyRequests || e.Code >= 500
}
```
- 直连失败 → 经代理 fallback 再抓一次（`opts.ProxyFallback`）。
- 订阅热更新不停机：`Manager.UpdateConfig(urls, enabled, interval)` 重建 loop 并 `SaveSettings()`。

**验证策略**
- 独立 `NodeTester`（`internal/importer/tester.go`），默认并发 32、**3 轮重试**：
```go
// tester.go:36-42
DefaultProbeTarget      = "https://www.gstatic.com/generate_204"
AlternateProbeTarget    = "https://cp.cloudflare.com/generate_204"
DefaultProbeTimeout     = 5 * time.Second
probeRounds             = 3
defaultProbeConcurrency = 32
defaultProbeRetryDelay  = time.Second
```
- 判定严格到"必须 204"：
```go
// tester.go:960-962
if resp.StatusCode != http.StatusNoContent {
	return fmt.Errorf("probe got HTTP %d, expected 204", resp.StatusCode)
}
```
- 错误二分：permanent（配置类，不重试）vs retryable（网络类，进下一轮）：
```go
// tester.go:68-74
func retryableProbeFailure(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) { return false }
	var permanent *permanentProbeError
	return !errors.As(err, &permanent)
}
```
- 轮次降并发 `probeRoundConcurrency`（第 1 轮满并发，后续折半）。
- **探针去重调度器**（`internal/importer/probe_scheduler.go`）：同 key 任务合并 waiter、双优先级队列（high/normal）、`deduplicated` 计数：
```go
// probe_scheduler.go:151-163
func (s *probeScheduler) attachLocked(task *probeTask, priority probePriority, timeout time.Duration, fn probeTaskFunc, waiter probeWaiter) {
	s.deduplicated.Add(1)
	if task.running {
		task.nextWaiters = append(task.nextWaiters, waiter)
		task.nextFn = fn
		task.nextTimeout = timeout
		if priority > task.nextPriority { task.nextPriority = priority }
		return
	}
	task.waiters = append(task.waiters, waiter)
}
```
- 校验目标可切换：204 测速 + 站点可达性（`verifyNodes` / `verifySiteTargets`，`internal/importer/verification.go`）；未开 204 时用站点延迟均值当延迟。
- **出口 IP 归属地三级 fallback**：ipinfo.io/json → ip-api.com/json → api.country.is（`tester.go:1057-1076 lookupCountry`）。

**存储与调度**
- 池调度 `internal/outbound/pool/pool.go`，4 模式：
```go
// pool.go:173-184
switch strings.ToLower(strings.TrimSpace(options.Mode)) {
case "", modeRotate: options.Mode = modeRotate
case modeSequential: options.Mode = modeSequential
case modeRandom:     options.Mode = modeRandom
case modeBalance:    options.Mode = modeBalance
default:             options.Mode = modeSequential
}
```
- rotate 带**粘性窗口**（`rotation_interval`，默认 2 分钟）：
```go
// pool.go:417-419
if p.rotateMember != nil && now.Sub(p.rotateSince) < interval && p.memberAvailable(p.rotateMember, now, network) {
	return p.rotateMember
}
```
- 熔断 + 半开（`internal/outbound/pool/shared_state.go`）：
```go
// shared_state.go:119-130
func (s *sharedMemberState) tryAcquire(now time.Time) bool {
	s.mu.Lock(); defer s.mu.Unlock()
	if !s.blacklisted { return true }
	if now.Before(s.blacklistedUntil) || s.halfOpen { return false }
	s.halfOpen = true
	return true
}
```
- 关键细节 —— **探测成功就清池黑名单**（否则"探针说好、池里还黑着"）：
```go
// pool.go:510-514
// Clear pool blacklist on successful probe — a node that passes
// health check should be available for selection immediately,
// not remain blacklisted for the full duration (fixes #8, #9).
if member.shared != nil { member.shared.forceRelease() }
```
- 多端口靠 **inbound→outbound 运行时热映射**（`internal/outbound/dispatch/dispatch.go`）：
```go
// dispatch.go:80-88
func (d *outbound) resolve(ctx context.Context) (adapter.Outbound, error) {
	tag := d.fallback
	if metadata := adapter.ContextFrom(ctx); metadata != nil {
		if mappings, ok := d.mappings.Load().(map[string]string); ok {
			if mapped := mappings[metadata.Inbound]; mapped != "" { tag = mapped }
		}
	}
	...
}
```
- 端口占用检测用 **TCP 拨号**而非"能否 bind"：
```go
// internal/monitor/ports_handlers.go:17-29
func isPortListening(address string, port uint16) bool {
	probe := address
	if probe == "" || probe == "0.0.0.0" || probe == "::" { probe = "127.0.0.1" }
	addr := net.JoinHostPort(probe, strconv.Itoa(int(port)))
	conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
	if err != nil { return false }
	_ = conn.Close()
	return true
}
```

**对外接口**：mixed 单端口 `2323`（HTTP+SOCKS 同端口）+ 多端口 `24000` 起递增 + 管理 API `9091`。
管理 API 路由（`internal/monitor/server.go:154-217`，全部走 `withAuth`）：`/api/auth`、`/api/settings`、`/api/nodes*`、`/api/subscription/{status,refresh,config,delete}`、`/api/import/*`、`/api/chain-profiles*`、`/api/connectivity/{scopes,jobs/*,results,history,ports/*}`、`/api/ports/status`、`/api/ui/*`、`/api/reload`、`/api/traffic`、`/api/logs*`、`/api/backup/webdav/*`、`/api/data-export/*`；可选 pprof。

**抗限流/抗封禁**：`/api/connectivity/history` 站点连通性历史；`drain_timeout` 优雅下线；`min_available_nodes` 保底节点数，低于阈值拒绝 apply（最新 commit 就是"跳过不可构建节点而不是整批回滚"）。

**最值得抄**
1. `internal/importer/probe_scheduler.go:89-138 Submit()` —— 探测去重调度，直接掐掉"同节点被并发重复探测"这个探针站限流主因。
2. `internal/outbound/pool/shared_state.go:66-93 recordFailure` + `tryAcquire` —— 失败计数 + 黑名单时长 + halfOpen 试探的完整闭环。
3. `internal/outbound/dispatch/dispatch.go` —— inbound→outbound 热映射，正好承载 GoProxy 的 7776/7777/7779/7780 四端口到不同池的映射。

---

### 2.4 Rain-kl/PoolX —— 控制面架构 + 端口 profile 概念最好

**代理来源管理**
- 节点导入（上传配置文件 / 订阅 URL 拉取），`backend/internal/application/clash/service.go`：`UploadSourceConfig` / `FetchSubscription` / `syncSourceNodes` / `ConfirmSourceConfig` / `RefreshSourceConfig` / `ReuploadSourceConfig`。
- 节点是 DB 实体：`ListNodes` / `UpdateNode` / `DeleteNodesBatch` / `ToggleNodesBatch`（批量启停）。
- 不自建池，交给 Mihomo 内核。

**验证策略**
- 借内核测延迟：`TestNodesBatch`，默认测速 URL：
```go
// application/clash/service.go:77-83
func (s *Service) getDefaultTestURL(ctx context.Context) string {
	...
	return "https://cp.cloudflare.com/generate_204"
}
```
- 结果落库：`UpdateTestStatus(ctx, n.ID, status, latencyPtr, errMsg)`，`TestType: "delay"`。

**存储与调度**
- SQLite（默认）/ Postgres；分层清晰：`domain / application / infra / repository / transport / shared`。
- 调度交给内核（负载均衡 / 自动回退 / 基于延迟选择）。
- **PortProfile = 一个池 profile**：节点子集 + 独立 `TestURL` + 独立策略，`CreatePortProfile` / `SetProfileNodes` / `RenderProfilePreview` / `RenderAggregatedConfig`。

**对外接口**（`backend/internal/transport/http/server.go:63-166`）
- 中间件链：`Recovery → ErrorHandler → RequestID → SecurityHeaders → MaxBodyBytes → Timeout → AccessLog`，业务组再叠 `AdminAuth`。
- 路径：`/healthz`、`/readyz`、`/api/health`、`/api/v1/user/{login,refresh,logout,self,change-password}`、`/api/v1/config/public`、`/api/v1/admin/*`（status/version/settings/examples/clash）。
- 反代 zashboard：`/api/zashboard/clash/*path`（需 AdminAuth）。
- 认证：JWT access 15m / refresh 720h + cookie；`bootstrapAdmin` 首启建号。

**抗限流/抗封禁**：自身不做，靠内核 + 端口 profile 按目标站分流（README 明确"面向不同目标站点的工作台式代理编排"）。

**最值得抄**
1. `backend/internal/transport/http/server.go:63-166 New()` —— 给 GoProxy 的 webui 一个现成的认证/超时/请求 ID/限流/安全头骨架。
2. PortProfile 概念 —— "端口 = 池 profile（节点子集 + 独立测速 URL + 独立策略）"，与 GoProxy 四端口（HTTP 7776/7777、SOCKS5 7779/7780）承载不同用途的需求天然对齐。
3. `TestNodesBatch` + `UpdateTestStatus` —— 延迟数据持久化，重启不丢，比纯内存延迟表可靠。

---

### 2.5 yunginnanet/prox5 —— 作为"库"的抽象最干净，钩子设计最实用

**代理来源管理**
- 无内置源。只吃文件/字符串：`LoadProxyTXT(seedFile)` / `LoadSingleProxy` / `LoadMultiLineString`。
- 入口格式容错：`ip:port`、`ip:port:user:pass`、`[v6]:port:user:pass`。
- `proxyMap`（concurrent-map）去重，`Pending` list 做待验证队列。

**验证策略**
- 两段式：先 TCP `DialTimeout`，再经代理 HTTP GET 随机 "what is my ip" 端点，**响应必须能 `net.ParseIP` 成功**：
```go
// validator_engine.go:180-200
conn, err := net.DialTimeout("tcp", endpoint, p5.GetValidationTimeout())
...
if newip := net.ParseIP(resp); newip == nil {
	p5.badProx.Check(sock)
	return errors.New("bad response from http request: " + resp)
}
sock.ProxiedIP = resp
```
- 8 个校验端点（`defs.go:115-124`）全 https，注释明确"保证验证过的代理具备 SSL 能力"；超时 9s（校验）/15s（服务端）。
- **协议自动嗅探**：首次验证依次试 SOCKS4/4a/5，通用哪个：
```go
// validator_engine.go:233-250
case sock.timesValidated == 0, sock.protocol.Get() == ProtoNull:
	for tryProto := range protoMap {
		if tryProto == ProtoNull { continue }
		if err := pe.singleProxyCheck(sock, tryProto); err != nil { continue }
		sock.protocol.set(tryProto)
		break
	}
```
- 55 个 UA 随机 + `sync.Pool` 复用 header（`validator_engine.go:20-29`）。

**存储与调度**
- 内存：4 条 list（SOCKS4/4a/5/HTTP）+ proxyMap。
- 取用前 `stillGood()` 三重复核（bad 计数上限 / badProx 惩罚缓存 / stale 超时）：
```go
// dispense.go:112-135
if p5.GetRemoveAfter() != -1 && atomic.LoadInt64(&sock.timesBad) > int64(p5.GetRemoveAfter()) {
	... p5.proxyMap.delete(sock.Endpoint); return false
}
if p5.badProx.Peek(sock) { p5.msgBadProxRate(sock); return false }
if time.Since(sock.lastValidated) > p5.opt.stale { ...; p5.stats.stale(); return false }
```
- **双 Rate5 限流器**：`useProx`（用太频繁的暂跳过）+ `badProx`（坏代理惩罚缓存）—— 这就是它的熔断。
- 自动扩缩：`internal/scaler.AutoScaler`，按 "dial 次数 vs 已验证代理数" 阈值调 worker；worker 池用 ants。
- 回收：`recycle` 开关 + `removeafter`（默认 25 次失败彻底删）+ `shuffle`。

**对外接口**（纯库）
- `DialContext/Dial/DialTimeout`（每请求换代理）、`GetHTTPClient()` / `RoundTrip`（实现 `http.RoundTripper`）、`StartSOCKS5Server(listen, user, pass)`（go-socks5，支持 UserPass 认证）。无 REST、无面板。

**抗限流/抗封禁**
- badProx 惩罚缓存避免反复撞同一坏 IP。
- `dialerBailout`（默认 20）：一次拨号最多换 20 个代理才放弃：
```go
// mystery_dialer.go:122-128
for {
	maxBail := p5.GetDialerBailout()
	switch {
	case count > maxBail:
		return nil, fmt.Errorf("giving up after %d tries", maxBail)
```
- **拨号中间件**（做出口 IP 去重 / 同 C 段限制的完美钩子）：
```go
// setters.go:115-123
// SetDispenseMiddleware will add a function that sits within the dialing process of the mysteryDialer and anyhing using it.
// This means this function will be called mid-dial during connections. Return true to approve proxy, false to skip it.
func (p5 *ProxyEngine) SetDispenseMiddleware(f func(*Proxy) (*Proxy, bool)) {
	p5.mu.Lock()
	p5.dispenseMiddleware = f
	p5.mu.Unlock()
```

**最值得抄**
1. `setters.go:115-123 SetDispenseMiddleware` —— 拨号中途否决/改写代理，用来做出口 IP 去重、同 C 段限流、目标站白名单。
2. `dispense.go:103-138 stillGood()` —— 取用前复核三连（bad 上限 / 惩罚缓存 / stale）。
3. `validator_engine.go:193-198` —— "响应必须是合法 IP"这一条严格判定，杜绝被劫持页/广告页误判成活代理。

---

### 2.6 tongsq/proxy-collect —— 三态状态机 + 隧道限速最实用

**代理来源管理**
- Getter 接口 + 每站一个文件（`service/proxy_getter/*.go`，约 30 站）+ YAML 自定义 getter：
```go
// service/proxy_getter_interface.go:3-12
type ProxyGetterInterface interface {
	GetContentHtml(requestUrl string) string
	ParseHtml(body string) [][]string   // ["ip","port","protocol","user","password"]
	GetUrlList() []string
}
```
```yaml
# conf.yaml:42-51
getters:
  - method: GET
    regexp: (\d+\.\d+\.\d+\.\d+):(\d+)
    proto: http
    proxy: false      # 是否通过代理获取（防源站封本机 IP）
    agent: Mozilla/...
    urls:
      - https://www.zdaye.com/dayProxy/1.html
```
- 每轮抓完 `time.Sleep(10s)` 再继续（`service/proxy_service.go:136-140`）。

**验证策略**
- 唯一目标 URL `https://www.baidu.com`，耗时当 ping：
```go
// service/proxy_service.go:38-47
func (s *proxyService) CheckIpStatus(proxy *dto.ProxyDto) (bool, int64) {
	u := "https://www.baidu.com"
	t := time.Now()
	_, err := request.Get(u, request.NewOptions().WithHeader(h).WithProxy(s.TransferProxyDto(proxy)).WithTimeout(global.MaxPing))
	duration := time.Now().Sub(t)
	return err == nil, int64(duration / time.Millisecond)
}
```
- 超时 `maxPing: 5000`（ms）；并发靠全局 `global.Pool` 任务池。
- **弱点（不要抄）**：只判断能否 200，不校验出口 IP ⇒ 透明代理/劫持页会被当活的。

**存储与调度**
- **三态状态机 + 四集合**（Redis 版 `dao/redis/proxy_dao.go:15-18`，另有 MySQL 实现同一接口）：
```go
const PROXY_FAIL_SET = "proxy_fail_set"
const PROXY_INFO_MAP = "proxy_info_map"
const PROXY_SUCCESS_SET = "proxy_success_set"
const PROXY_RECHECK_SET = "proxy_recheck_set"
```
- 状态流转（`service/proxy_service.go:77-104`）：YES → 失败 → RECHECK（`check_count = recheckCount`，默认 10）→ 继续失败递减 → `< 0` 才删除。**不是一次失败就删**，这是抗抖动关键。
- 字段：`CheckCount` 成功时上限 20、`ActiveTime`、`Country/Region/City/Isp` 异步回查。
- 调度：`scheduler` 包 5 个定时任务（get_ip / check_active_ip / check_fail_ip / recheck_ip / update_ip_info）。

**对外接口**
- API `8090`（gin）：`/ping`、`/all?city=&duration=&proto=&count=&token=`、`/start`、`/stop`；`TokenAuthMiddleware` 支持 query 或 form token。
- 隧道代理：内嵌 gost（`service/tunnel/gost.go`），`tunnel.tunnelLevel`（链层数）、`strategy: round`、`maxFails: 100`、`failTimeout: 10`。
- **隧道账号限速**：
```yaml
# conf.yaml:32-41
tunnels:
  - proto: http
    host: 0.0.0.0
    port: 8888
    users:
      - username: root
        password: 123
        limiter: 1,10,2      # gost 限速规则
```
```go
// service/tunnel/cfg.go:109-121
func parseLimiter(users []config.TunnelUserConfig) (gost.Limiter, error) {
	l, _ := gost.NewLocalLimiter("", "")
	for _, user := range users {
		err := l.AddRule(user.Username, user.Limiter)
	}
	return l, nil
}
```
- 节点组选择器带 `FailFilter{MaxFails, FailTimeout}` + `InvalidFilter`，熔断交给 gost（`service/tunnel/gost.go:131-144`）。

**最值得抄**
1. `service/proxy_service.go:77-104 CheckProxyAndSave` —— 三态（YES/RECHECK/NO + check_count 递减）状态机，GoProxy 的 validator 从"二元死活"升级成三态最省事。
2. `service/tunnel/cfg.go:109-121 parseLimiter` + `config.TunnelUserConfig.Limiter` —— 隧道代理按账号限速，字段与规则可直接照搬。
3. `dao/redis/proxy_dao.go:155-173 updateProxySet` —— Redis Set 维护三个状态桶 + Hash 存明细，状态迁移 O(1)，比全表扫快。

---

### 2.7 phpgao/proxy_pool —— 分数制 + 双通道校验，虽停更但点子好

**代理来源管理**
- `job` 包每站一文件（`job/html_*.go` / `job/re_*.go`，约 40 个），robfig/cron 调度，每 spider 自带 `Cron()`：
```go
// schedule/schedule.go:26-35
for _, _spider := range s.spiders {
	go _spider.Run()                                  // 启动先跑一次
	cronID, err := s.cron.AddJob(_spider.Cron(), _spider)
	s.cronMap[_spider.Name()] = cronID
}
```
- 抓到的代理进 `NewProxyChan`（容量 `NewQueue=200`）→ validator 消费；另有 `OldProxyChan` + internalJob 做存量复检。

**验证策略** —— 三段式 + 双通道：
```go
// validator/validator__new.go:43-61
err = p.SimpleTcpTest(config.GetTcpTestTimeOut())   // ① TCP 可连（5s）
if err != nil { return }
err = p.TestProxy(false)                            // ② HTTP 经代理取 http://ip.cip.cc
if err != nil { return }
err := p.TestProxy(true)                            // ③ HTTPS 经代理取 https://ip.cip.cc
if err != nil { /* ③ 失败不淘汰，只是不标 https */ }
```
- **出口 IP 严格比对**：
```go
// model/proxy.go:167-170
html := strings.TrimSpace(string(b))
if html != p.GetIp() {
	return errors.New("html doesn't match")
}
```
- 独立 CONNECT 隧道验证（`model/proxy.go:195-229`）：发 `CONNECT cip.cc:443 HTTP/1.1`，解析首行必须 `HTTP/1.1 200`。
- 并发 300 goroutine 常驻消费；**sync.Map 去重锁**防同代理重复验证：
```go
// validator/validator__new.go:30-37
if _, ok := lockMap.Load(key); ok { return }
lockMap.Store(key, 1)
defer func() { lockMap.Delete(key) }()
if storeEngine.Exists(*proxy) { return }
```

**存储与调度** —— Redis Hash + 分数制：
```go
// db/redis.go:124-147
func (r *redisDB) AddScore(p model.HttpProxy, score int) (err error) {
	v := r.GetScore(p)
	rs := v + score
	if rs <= 0 { err = r.Remove(p); return }             // 分数归零 → 删除
	if rs >= 100 { rs = 100; err = r.ExpireDefault(p) }  // 满分 → 续命 TTL
	p.Score = rs
	return r.SetScore(p)
}
```
- 复检成功 +10 分；`ScoreAtLeast` 作为取用门槛；`validator/max.go` 每分钟 `Update()`，池超 `MaxProxy` 就按分数排序 `Wash()` 淘汰低分。
- 取用走 cache 层带 TTL 全量缓存（http/https 两组），过期时用自研 `Once` 保证只有一个 goroutine 刷新：
```go
// cache/proxy.go:72-80
func (c *Cached) Get() *map[string][]model.HttpProxy {
	c.m.RLock(); defer c.m.RUnlock()
	now := time.Now()
	if now.Sub(c.expire) >= 0 { once.Do(c.Update) }
	return &Cache.proxy
}
```

**对外接口**
- API `8088`（gin）：`GET /`（统计）、`GET /get?schema=&cn=&score=&source=&country=&limit=`、`GET /random`、`GET /random_text`。
- 正向代理 `8089`（`server/reverse.go`）—— **池空自动退化为直连**：
```go
// server/reverse.go:25-49
p := *cache.Cache.Get()
proxies := p["https"]
if proxies == nil {
	logger.Debug("serve as a https proxy")
	destConn, err = net.DialTimeout("tcp", r.Host, timeOut)      // 退化为直连
} else {
	proxy := proxies[rand.Intn(l)]
	msg := fmt.Sprintf(model.ConnectCommand, http.MethodConnect, r.Host, "HTTP/1.1", r.Host)
	destConn, err = net.DialTimeout("tcp", proxy.GetProxyUrl(), timeOut)
	destConn.Write([]byte(msg))
}
```
- 无认证。

**最值得抄**
1. `model/proxy.go:127-179 TestProxy` + `195-229 TestConnectMethod` —— HTTP 与 CONNECT 双通道分别验证且严格比对出口 IP。
2. `db/redis.go:124-147 AddScore` —— 分数制 + 满分续 TTL + 归零删除，比二元死活更能表达"质量"。
3. `validator/max.go:51-72 Wash()` + `validator__new.go` 的 `lockMap` 去重锁 —— 池容量治理 + 并发去重。

---

## 3. 综合吸收清单（12 条，按优先级）

| 优先级 | 改造点 | 来源 | 为什么 | 落到模块 |
|---|---|---|---|---|
| **P0-1** | 二元死活 → **三态状态机**（OK / RECHECK / DEAD + `check_count` 递减，连续失败 N 次才删） | proxy-collect `service/proxy_service.go:77-104` | 免费代理抖动极大，一次失败即删会周期性清空池；RECHECK 桶能保住"偶发失败但整体可用"的代理 | `validator` + `storage` |
| **P0-2** | **出口 IP 严格校验 + 透明代理识别**：验证前先取本机出口 IP，响应体命中本机 IP ⇒ 拉黑；响应必须能解析成合法 IP | slrp `checker/checker.go:233-252`；phpgao `model/proxy.go:167-170`；prox5 `validator_engine.go:193-198` | GoProxy 现在只判 HTTP 200，会把劫持页/透明代理当活代理入池，污染池子且经其转发会泄漏本机真实 IP | `validator` |
| **P0-3** | **校验站随机分摊 + 多站 fallback**：每次校验随机挑一个目标站；归属地/延迟查询走三级 fallback | slrp `checker/checker.go:190-193`；prox5 `defs.go:115-124`；easy-proxies `tester.go:1057-1076` | 单目标站验证 1000+ 代理必被限流；随机分摊 + fallback 是最低成本的抗限流 | `validator` |
| **P0-4** | **探针去重调度器**：同 `(node, target)` 的探测任务合并 waiter，双优先级队列，记录 `deduplicated` | easy-proxies `probe_scheduler.go:89-163` | 定时全量复检 + 手动复检 + 导入复检会并发打同一节点，是探针站限流的真正主因 | `validator` |
| **P0-5** | **熔断 / 冷却 / 半开 / 复活四件套**，且"探测成功即清池黑名单" | slrp `pool/entry.go:155-194`；easy-proxies `shared_state.go:66-130` + `pool.go:510-514` | 只靠"失败即删"没有恢复通道；`ReanimateAfter` + `halfOpen` 让代理有第二次机会，`forceRelease` 修掉"探针说好但池里还黑着"的经典 bug | `pool` |
| **P0-6** | **拨号中间件**：在建立连接的中途回调，可现场否决/改写选中的代理 | prox5 `setters.go:115-123` + `mystery_dialer.go:157-165` | GoProxy 目前完全没有出口 IP 去重、同 C 段限制、单源限速的落点；中间件是唯一能同时拿到"目标地址 + 代理"的位置 | `proxy` |
| **P1-7** | **inbound→outbound 运行时热映射** + "端口 = 池 profile（节点子集 + 独立测速 URL + 独立策略）" | easy-proxies `dispatch.go:56-96`；PoolX `PortProfile` / `RenderProfilePreview` | GoProxy 的 7776/7777/7779/7780 四端口现在只是四个固定监听；抽象成 profile 后可以按端口分配不同池、不同策略、不同测速目标，且改配置不用重启 | `proxy` + `webui` |
| **P1-8** | **订阅/源抓取独立成包**：URL 白名单、body 上限、`Retryable()` 状态码分类、直连失败自动走代理 fallback | easy-proxies `internal/subfetch/fetch.go:26-68` | Clash/V2ray 订阅站经常 429/5xx；"哪些状态码该重试"必须显式建模，否则要么疯狂重试要么误判失效 | `fetcher` |
| **P1-9** | **源级退避 + 阻断页识别 + 背压状态码**：源出错随机退避 0-60s；命中 cloudflare 等阻断页标记源异常；池空返回 552 | slrp `sources/try.go:117-121`、`sources/util.go:143-222` | 免费源站挂了是常态；固定间隔重试会把自己 IP 打进源站黑名单 | `fetcher` |
| **P1-10** | **滑动窗口成功率 + 分数制质量分**：5 分钟短窗 + 24 小时长窗 × (Offer/Success/Timeout/Failure)；分数满则续 TTL、归零则删除 | slrp `pool/entry.go:16-127`；phpgao `db/redis.go:124-147` | 累计计数会被历史数据污染；滑动窗口才能反映"最近好不好"，分数制让"淘汰"有连续梯度而不是硬阈值 | `pool` + `storage` |
| **P1-11** | **多端口 / 隧道代理 + 按账号限速**（`limiter: "1,10,2"` 式规则） | proxy-collect `service/tunnel/cfg.go:109-121` + `conf.yaml:32-41` | GoProxy 对外是裸端口无认证无限速；加上按用户限速才能多租户使用，也顺带抑制单账号把池打空 | `proxy` + `webui` |
| **P2-12** | **零依赖手写 SOCKS5 校验 + `file://` 本地源 + 非阻塞刷新触发** | yukkcat `checker.go:61-108`、`scraper.go:29-38`、`main.go:104-111` | ① SOCKS5 validator 少一个依赖、行为完全可控；② 本地文件当源让回归测试不依赖外网；③ 非阻塞刷新避免手动连点造成刷新风暴 | `validator` + `fetcher` + `webui` |

### 建议落地顺序（最小改动优先）

1. 先做 **P0-2 + P0-1**（validator 改三态 + 出口 IP 严格校验）—— 只动 `validator` 与 `storage` 的字段，收益最大、风险最低。
2. 再做 **P0-5 + P0-4**（pool 加熔断复活 + validator 加去重调度）—— 池子开始"自愈"。
3. 然后 **P0-6 + P1-7**（proxy 加拨号中间件 + 端口 profile 热映射）—— 四端口才真正变成四种策略。
4. 最后 **P1-8/P1-9/P1-10**（fetcher 独立包 + 滑动窗口 + 分数制）—— 长期稳定性工程化。

---

## 附：本次调研的原始文件落点

- 源码快照目录：`%TEMP%\proxres\`（含 7 个仓库的 `*.go` / `*.yaml` / `*.md`，文件名以 `__` 代替路径分隔符）
- 本报告：`E:\AI\Buddytests\AA日常\DSH\Github\代理池7仓库调研-GoProxy改造吸收清单.md`
