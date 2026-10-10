# GoProxy 部署与 opencode2 集成（加固版）

本目录是"把 GoProxy 变成可自托管、可被下游网关消费的代理池"的部署物料。
对应改动见仓库根 `CHANGELOG-HARDENING.md`。

## 1. 组件与端口

| 端口 | 协议 | 用途 | 默认绑定 |
|---|---|---|---|
| 7776 | HTTP | 最低延迟（稳定） | `PROXY_BIND`（默认 127.0.0.1） |
| 7777 | HTTP | 随机轮换 | 同上 |
| 7778 | HTTP | WebUI / REST API | `WEBUI_BIND`（默认 127.0.0.1） |
| 7779 | SOCKS5 | 随机轮换 | `PROXY_BIND` |
| 7780 | SOCKS5 | 最低延迟 | `PROXY_BIND` |

> 默认只监听 127.0.0.1：避免"部署完就变成公网开放代理"。
> 需要跨机使用时改 `PROXY_BIND=0.0.0.0`，**并同时开启 `PROXY_AUTH_ENABLED=true`**。

## 2. 部署（Ubuntu 24.04，幂等）

```bash
# 在目标机（例如 Azure VM2）执行
REPO=https://github.com/<you>/GoProxy.git BRANCH=main bash deploy/install-vm2.sh
```

脚本做 6 件事：
1. 安装 Go（默认 1.26.7，已存在则跳过）；
2. 同步源码到 `~/goproxy`；
3. `CGO_ENABLED=1 go build`（sqlite3 驱动需要 gcc）；
4. 生成 `/etc/goproxy/goproxy.env`（**随机** WEBUI/代理口令 + EXPORT_TOKEN；已存在则保留）；
5. 安装并启动 `goproxy.service`（systemd 加固：NoNewPrivileges / ProtectSystem / 空 CapabilityBoundingSet）；
6. 健康检查：`/api/stats`、`/api/pool/status`、端口监听。

日志：`journalctl -u goproxy -f`

## 3. 关键运行参数

| 变量 | 默认 | 说明 |
|---|---|---|
| `PROXY_BIND` / `WEBUI_BIND` | `127.0.0.1` | 监听地址；`0.0.0.0` 表示所有网卡 |
| `PROXY_AUTH_ENABLED` / `PROXY_AUTH_USERNAME` / `PROXY_AUTH_PASSWORD` | `true` / `goproxy` / 随机 | 代理端口 Basic Auth（HTTP）/ 用户名口令（SOCKS5） |
| `WEBUI_PASSWORD` | 随机 | WebUI 管理口令 |
| `EXPORT_TOKEN` | 随机 | `/api/export` 静态令牌，供下游脚本拉池 |
| `EXITIP_RATE_LIMIT` / `EXITIP_BURST` | `0.6` / `5` | 出口 IP 查询全局速率（ip-api 免费档 45 次/分钟） |
| `REQUIRE_EXIT_INFO` | `false` | **两段式验证开关**：false = 先按连通性入池，后台再补出口位置 |
| `BLOCKED_COUNTRIES` / `ALLOWED_COUNTRIES` | `CN` / 空 | 出口地理黑/白名单（白名单非空时优先） |
| `ALLOW_PRIVATE_OUTBOUND` | `false` | 订阅/贡献导入是否允许内网地址（SSRF 开关） |
| `SESSION_TTL_MINUTES` / `LOGIN_MAX_ATTEMPTS` | `720` / `10` | WebUI 会话有效期 / 同 IP 登录失败上限 |

## 4. 代理导出接口

```bash
TOKEN=$(sudo sed -n 's/^EXPORT_TOKEN=//p' /etc/goproxy/goproxy.env)
curl -H "X-Export-Token: $TOKEN" \
  "http://127.0.0.1:7778/api/export?protocol=socks5&source=free&format=uri&max_latency=3000&limit=300"
```

输出（`format=uri`）：

```
socks5://203.0.113.10:1080
socks5://198.51.100.7:1080
```

参数：`protocol=socks5|http|all`、`source=free|custom|all`、`format=uri|plain|json`、
`max_latency=<ms>`、`min_grade=S|A|B|C`、`limit=<n>`。

## 5. 集成到 opencode2api 代理池

### 5.1 为什么不能直接写 proxies_public.txt

`opencode2api` 的池由 `proxyscraper_refresh.py` 每小时维护：它自己重写
`proxies_public.txt`，再把 `master + public` 合并写回 `proxies.txt`。
**实测**：把 26 条代理写进 `proxies_public.txt` 后，下一轮刷新即被覆盖，注入区块消失。

因此本方案用**独立文件** `proxies_goproxy.txt`（GoProxy 侧维护），并给刷新器打一次
最小补丁，让它在合并时一并读入：

```bash
python3 deploy/patch-opencode2-refresh.py        # 幂等；自动备份 + py_compile 校验 + 失败回滚
# 合并逻辑变为：master + public + proxies_goproxy.txt(去注释) → proxies.txt
```

补丁只动两处：新增常量 `GOPROXY`、合并表达式追加该文件（并过滤 `#` 注释行）。
**注意**：注释行必须过滤 —— 实测不过滤时刷新器的 `valid_proxy()` 会判定 2 条非法并
`中止（不覆盖）`，导致整轮刷新失败。

### 5.2 一次性执行

```bash
python3 deploy/patch-opencode2-refresh.py
bash deploy/export-to-opencode2.sh
```

`export-to-opencode2.sh` 的 6 步：导出 active 代理 → **目标站可达性预筛** →
写 `proxies_goproxy.txt`（幂等替换标记区块）→ 触发刷新器合并 → 等就绪 → 校验。

预筛默认 `TEST_URL=https://opencode.ai/zen/v1/models`、`PARALLEL=32`：
只把"能到达 opencode.ai"的代理注入下游池，避免用通用连通性（generate_204）污染下游。

### 5.3 定时同步

```bash
bash deploy/install-export-timer.sh      # 安装 goproxy-export-opencode2.timer（默认 15 分钟）
systemctl list-timers goproxy-export-opencode2.timer
```

### 5.4 实测验收（Azure VM2，Ubuntu 24.04）

| 项 | 结果 |
|---|---|
| GoProxy 池 | 60 条 active（HTTP 33 / SOCKS5 27），全部带出口 IP+地理位置 |
| 导出 | 25 条 SOCKS5（`status=active`、`max_latency=3000`） |
| 目标站预筛 | 25 / 25 通过（`opencode.ai/zen/v1/models` 可达） |
| 注入后 `proxies.txt` | 2034 条（master 1865 + public 144 + goproxy 25） |
| **跨刷新存活** | 刷新器完整跑完一轮后，25 条仍全部在 `proxies.txt` 中（总 2029） |
| 下游 healthz | `status: ok`，proxies total 2029、healthy 2029、unhealthy 0 |
| 端到端 | 经 `127.0.0.1:7872/v1/chat/completions` 3/3 返回 HTTP 200 |


## 6. 验证清单

```bash
# 1) GoProxy 自身
curl -s http://127.0.0.1:7778/api/stats
curl -s http://127.0.0.1:7778/api/pool/status
# 2) 出口可用性（经 GoProxy 的 SOCKS5 随机端口，带认证）
curl -s --socks5-hostname goproxy:<PROXY_AUTH_PASSWORD>@127.0.0.1:7779 https://api.ipify.org
# 3) 下游池
curl -s http://127.0.0.1:7872/healthz
# 4) 端到端（opencode2api 真实推理请求）
KEY=$(cat ~/opencode2api/.api_key)
curl -s -o /dev/null -w '%{http_code} %{time_total}s\n' --max-time 60 \
  -X POST http://127.0.0.1:7872/v1/chat/completions \
  -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d '{"model":"big-pickle","messages":[{"role":"user","content":"say ok"}],"max_tokens":8}'
```

## 7. 回滚

```bash
sudo systemctl disable --now goproxy
# 下游池回到纯 master：删除注入区块后合并
python3 - <<'PY'
import pathlib, re
p = pathlib.Path.home()/"opencode2api/proxies_public.txt"
if p.exists():
    p.write_text(re.sub(r"\n?# >>> goproxy-pool:start\n.*?# <<< goproxy-pool:end\n?", "\n", p.read_text(), flags=re.S))
PY
cat ~/opencode2api/proxies_master.txt ~/opencode2api/proxies_public.txt | awk NF | sort -u > ~/opencode2api/proxies.txt
sudo systemctl restart opencode2api
```
