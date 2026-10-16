#!/usr/bin/env bash
# goproxy → opencode2api 代理池 直连同步（Oracle 版）
# ---------------------------------------------------------------------------
# 目标：把 Oracle 本机 goproxy（GoProxy）已验证的 active 代理，按"第四来源文件"
#       注入 opencode2api 的池，并走 admin 热重载（不断流），最后用 healthz 校验。
#
# 池模型（沿用 OBS《OpenCode-自建反代手册》的三文件约定，本脚本只加一源）：
#   proxies_core.txt     基线池（首次运行从现有 proxies.txt 快照，之后只读）
#   proxies_goproxy.txt  本脚本维护（GoProxy 导出，幂等区块）
#   proxies.txt          merge 目标 = core + goproxy（原子写回）
#
# 与"重启式"方案的区别：优先 POST /api/config/reload 热重载；失败才兜底 restart。
# 依赖：/etc/goproxy/goproxy.env 里的 EXPORT_TOKEN；~/.opencode2api_admin 管理口令文件。
#
# 退出码：0=成功  2=导出为空  3=预筛通过数不足（保护下游池，不写）  4=写后校验失败
set -uo pipefail

OC_DIR="${OC_DIR:-/opt/opencode2api}"
GOPROXY_BASE="${GOPROXY_BASE:-http://127.0.0.1:7778}"
ENV_FILE="${ENV_FILE:-/etc/goproxy/goproxy.env}"
ADMIN="${ADMIN:-http://127.0.0.1:7874}"
HEALTHZ="${HEALTHZ:-http://127.0.0.1:7872/healthz}"
CORE="${CORE:-$OC_DIR/proxies_core.txt}"
SRC="${SRC:-$OC_DIR/proxies_goproxy.txt}"
DST="${DST:-$OC_DIR/proxies.txt}"
TEST_URL="${TEST_URL:-https://opencode.ai/zen/v1/models}"
PARALLEL="${PARALLEL:-16}"
LIMIT="${LIMIT:-200}"
MAX_LATENCY="${MAX_LATENCY:-3000}"
MIN_ALIVE="${MIN_ALIVE:-5}"
MIN_TOTAL="${MIN_TOTAL:-50}"
PROTOCOL="${PROTOCOL:-socks5}"
PASSFILE="${PASSFILE:-/root/.opencode2api_admin}"

log() { printf '%s %s\n' "$(date '+%F %T')" "$*"; }

TOKEN="${EXPORT_TOKEN:-}"
[ -n "$TOKEN" ] || TOKEN="$(sed -n 's/^EXPORT_TOKEN=//p' "$ENV_FILE" 2>/dev/null | head -1)"
if [ -z "$TOKEN" ]; then log "[!] 取不到 EXPORT_TOKEN（$ENV_FILE）"; exit 1; fi

# 0) 首次运行：给 core 打快照（基线池，只做一次）
if [ ! -f "$CORE" ]; then
  cp -a "$DST" "$CORE"
  log "[0] 已建立基线快照 $CORE（$(grep -c . "$CORE" 2>/dev/null || echo 0) 条）"
fi

# 1) 导出（只取 active：pending/degraded/disabled 一律不给下游）
raw="$(mktemp)"; alive="$(mktemp)"; trap 'rm -f "$raw" "$alive"' EXIT
curl -fsS -m 20 -H "X-Export-Token: $TOKEN" \
  "$GOPROXY_BASE/api/export?protocol=$PROTOCOL&source=free&status=active&format=uri&max_latency=$MAX_LATENCY&limit=$LIMIT" \
  -o "$raw" || { log "[!] 导出接口调用失败"; exit 2; }
total="$(grep -c . "$raw" 2>/dev/null || echo 0)"
log "[1] 导出 $PROTOCOL active 代理 $total 条"
[ "$total" -gt 0 ] || { log "[!] 导出为空，保留下游池不动"; exit 2; }

# 2) 目标站可达性预筛（GoProxy 验的是通用连通性，这步验"能不能到 opencode.ai"）
check_one() {
  local uri="$1" code
  case "$uri" in
    socks5://*)  code=$(curl -s -o /dev/null -w '%{http_code}' -m 12 --socks5-hostname "${uri#socks5://}" "$TEST_URL" 2>/dev/null || echo 000) ;;
    http://*|https://*) code=$(curl -s -o /dev/null -w '%{http_code}' -m 12 --proxy "$uri" "$TEST_URL" 2>/dev/null || echo 000) ;;
    *) code=000 ;;
  esac
  [ "$code" != "000" ] && printf '%s\n' "$uri"
}
export -f check_one; export TEST_URL
xargs -a "$raw" -P "$PARALLEL" -I{} bash -c 'check_one "$@"' _ {} > "$alive" 2>/dev/null || true
alive_count="$(grep -c . "$alive" 2>/dev/null || echo 0)"
log "[2] 目标站预筛通过 $alive_count / $total"
if [ "$alive_count" -lt "$MIN_ALIVE" ]; then
  log "[!] 预筛通过数 < MIN_ALIVE=$MIN_ALIVE，放弃写入（保护下游池）"; exit 3
fi

# 3) 维护来源文件（幂等区块；下游 config 校验要求 scheme ∈ http/https/socks5/socks5h）
python3 - "$SRC" "$alive" <<'PY'
import pathlib, re, sys
src, alive = pathlib.Path(sys.argv[1]), pathlib.Path(sys.argv[2])
body = src.read_text(encoding="utf-8") if src.exists() else ""
body = re.sub(r"\n?# >>> goproxy-pool:start\n.*?# <<< goproxy-pool:end\n?", "\n", body, flags=re.S)
new = alive.read_text(encoding="utf-8").strip()
out = body.rstrip("\n")
if out:
    out += "\n\n"
out += "# >>> goproxy-pool:start\n" + new + "\n# <<< goproxy-pool:end\n"
src.write_text(out, encoding="utf-8")
kept = len([l for l in out.splitlines() if l.strip() and not l.strip().startswith("#")])
print(f"    [+] {src}: {kept} 条有效条目")
PY

# 4) 合并 core + goproxy → proxies.txt（过滤注释/空行 + 去重 + 原子写回）
merged="$(mktemp)"
{ grep -hv '^[[:space:]]*#' "$CORE" 2>/dev/null; grep -hv '^[[:space:]]*#' "$SRC" 2>/dev/null; } \
  | awk 'NF' | sort -u > "$merged"
merged_count="$(grep -c . "$merged" 2>/dev/null || echo 0)"
if [ "$merged_count" -lt "$MIN_TOTAL" ]; then
  log "[!] 合并结果 $merged_count < MIN_TOTAL=$MIN_TOTAL —— 中止（不覆盖）"; rm -f "$merged"; exit 4
fi
if cmp -s "$merged" "$DST"; then
  log "[4] 池内容无变化（$merged_count 条），跳过写回"
else
  cp -a "$DST" "$DST.bak-$(date +%Y%m%d-%H%M%S)" 2>/dev/null || true
  install -m 0644 -o oc2api -g oc2api "$merged" "$DST.tmp"
  mv -f "$DST.tmp" "$DST"
  log "[4] 已写回 $DST：$merged_count 条（其中 GoProxy $(grep -c . "$SRC" 2>/dev/null || echo 0) 行含标记）"
fi
rm -f "$merged"

# 5) 生效：热重载（登录 → csrf → /api/config/reload），失败兜底 restart
if [ -r "$PASSFILE" ]; then
  reload_out="$(python3 - "$ADMIN" "$PASSFILE" <<'PY' 2>&1
import http.cookiejar, json, sys, urllib.request
admin, pf = sys.argv[1].rstrip('/'), sys.argv[2]
pw = open(pf, encoding='utf-8').read().strip()
cj = http.cookiejar.CookieJar()
op = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(cj))
try:
    r = op.open(urllib.request.Request(f"{admin}/api/auth/login",
        data=json.dumps({"username": "admin", "password": pw}).encode(),
        headers={"Content-Type": "application/json"}, method="POST"), timeout=10)
    csrf = json.load(r).get("csrf_token", "")
    r2 = op.open(urllib.request.Request(f"{admin}/api/config/reload", data=b"{}",
        headers={"Content-Type": "application/json", "X-CSRF-Token": csrf}, method="POST"), timeout=25)
    print("HOT_RELOAD_OK", r2.status)
except Exception as e:
    print("HOT_RELOAD_FAIL", e)
PY
)"
  log "[5] 热重载：$reload_out"
  case "$reload_out" in
    *HOT_RELOAD_OK*) : ;;
    *) log "[5] 兜底 systemctl restart opencode2api"; systemctl restart opencode2api; sleep 6 ;;
  esac
else
  log "[5] 无管理口令文件（$PASSFILE），兜底 systemctl restart opencode2api"
  systemctl restart opencode2api; sleep 6
fi

# 6) 校验
hz="$(curl -s -m 10 "$HEALTHZ" || true)"
log "[6] healthz: $hz"
case "$hz" in *'"unhealthy":0'*) : ;; *) log "[!] healthz 提示存在 unhealthy 条目，请复核";; esac
log "[OK] goproxy → opencode2api 同步完成（本轮到池 $alive_count 条）"
