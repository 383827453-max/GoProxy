#!/usr/bin/env bash
# 把 GoProxy 池中已通过验证的代理导入 opencode2api 的"公开补充池"，并触发合并/热重载。
#
# 关键设计：写入下游池之前，先用**下游真实目标站**做一次可达性预筛
# （默认 https://opencode.ai/zen/v1/models）。理由：GoProxy 的验证目标是
# generate_204（通用连通性），而 opencode2api 的池只服务于 opencode.ai；
# 不预筛就会把"能上网但到不了 opencode.ai"的代理灌进下游池，反而降低可用性。
#
# 用法：
#   EXPORT_TOKEN=xxx bash deploy/export-to-opencode2.sh
#   TEST_URL=https://opencode.ai/zen/v1/models PARALLEL=32 LIMIT=300 ...
#
# 幂等：每次运行替换上一次注入的区块（# >>> goproxy-pool:start ... end），
# 不污染 opencode2api 原有的 master（商业认证）池。
set -euo pipefail

GOPROXY_BASE="${GOPROXY_BASE:-http://127.0.0.1:7778}"
OC_DIR="${OC_DIR:-$HOME/opencode2api}"
PUBLIC_POOL="${PUBLIC_POOL:-$OC_DIR/proxies_public.txt}"
MERGED_POOL="${MERGED_POOL:-$OC_DIR/proxies.txt}"
MASTER_POOL="${MASTER_POOL:-$OC_DIR/proxies_master.txt}"
LIMIT="${LIMIT:-300}"
MAX_LATENCY="${MAX_LATENCY:-3000}"
PROTOCOL="${PROTOCOL:-socks5}"
TEST_URL="${TEST_URL:-https://opencode.ai/zen/v1/models}"
PARALLEL="${PARALLEL:-32}"
MIN_ALIVE="${MIN_ALIVE:-1}"

if [ -z "${EXPORT_TOKEN:-}" ]; then
  # env 文件是 0600 root:root，普通用户不可读 → 直接用 sudo 读，不做 -r 预判
  EXPORT_TOKEN="$(sudo sed -n 's/^EXPORT_TOKEN=//p' /etc/goproxy/goproxy.env 2>/dev/null | head -1)"
fi
if [ -z "${EXPORT_TOKEN:-}" ]; then
  echo "[!] 缺少 EXPORT_TOKEN（导出接口鉴权）"; exit 1
fi

raw="$(mktemp)"; alive="$(mktemp)"
trap 'rm -f "$raw" "$alive"' EXIT

echo "[1/6] 从 GoProxy 导出 $PROTOCOL 代理（active, max_latency=${MAX_LATENCY}ms limit=$LIMIT）"
curl -fsS -H "X-Export-Token: $EXPORT_TOKEN" \
  "$GOPROXY_BASE/api/export?protocol=$PROTOCOL&source=free&status=active&format=uri&max_latency=$MAX_LATENCY&limit=$LIMIT" \
  -o "$raw"
total="$(grep -c . "$raw" || true)"
echo "      导出 $total 条"
if [ "${total:-0}" -eq 0 ]; then
  echo "[!] 导出为空（池子可能还在填充），放弃写入以免清空下游池"
  exit 2
fi

echo "[2/6] 目标站可达性预筛：$TEST_URL（并发 $PARALLEL）"
check_one() {
  local uri="$1" code
  case "$uri" in
    socks5://*) code=$(curl -s -o /dev/null -w '%{http_code}' -m 12 --socks5-hostname "${uri#socks5://}" "$TEST_URL" 2>/dev/null || echo 000) ;;
    http://*)   code=$(curl -s -o /dev/null -w '%{http_code}' -m 12 --proxy "$uri" "$TEST_URL" 2>/dev/null || echo 000) ;;
    *)          code=000 ;;
  esac
  # 000 = 连不通；其余状态码（含 401/403/404）都说明目标站可达
  [ "$code" != "000" ] && printf '%s\n' "$uri"
}
export -f check_one
export TEST_URL
if command -v xargs >/dev/null; then
  xargs -a "$raw" -P "$PARALLEL" -I{} bash -c 'check_one "$@"' _ {} > "$alive" 2>/dev/null || true
else
  while read -r line; do [ -n "$line" ] && check_one "$line" >> "$alive"; done < "$raw"
fi
alive_count="$(grep -c . "$alive" || true)"
echo "      预筛通过 $alive_count / $total"
if [ "${alive_count:-0}" -lt "$MIN_ALIVE" ]; then
  echo "[!] 预筛通过数低于 MIN_ALIVE=$MIN_ALIVE，放弃写入（保护下游池）"
  exit 3
fi

echo "[3/6] 写入公开补充池 $PUBLIC_POOL（替换上一次注入区块）"
python3 - "$PUBLIC_POOL" "$alive" <<'PY'
import pathlib, re, sys
pool_path, alive_path = sys.argv[1], sys.argv[2]
p = pathlib.Path(pool_path)
body = p.read_text(encoding="utf-8") if p.exists() else ""
body = re.sub(r"\n?# >>> goproxy-pool:start\n.*?# <<< goproxy-pool:end\n?", "\n", body, flags=re.S)
new = pathlib.Path(alive_path).read_text(encoding="utf-8").strip()
block = "# >>> goproxy-pool:start\n" + new + "\n# <<< goproxy-pool:end\n"
p.write_text(body.rstrip("\n") + "\n\n" + block, encoding="utf-8")
kept = len([l for l in p.read_text().splitlines() if l and not l.startswith("#")])
print(f"      [+] {p} 现有 {kept} 条")
PY

echo "[4/6] 合并 master + public → proxies.txt"
if [ -f "$MASTER_POOL" ]; then
  cat "$MASTER_POOL" "$PUBLIC_POOL" | awk 'NF' | sort -u > "$MERGED_POOL"
else
  cp "$PUBLIC_POOL" "$MERGED_POOL"
fi
echo "      proxies.txt 共 $(grep -c . "$MERGED_POOL" || true) 条"

echo "[5/6] 触发热重载"
if [ -f "$OC_DIR/proxyscraper_refresh.py" ] && systemctl list-unit-files 2>/dev/null | grep -q '^opencode2api-pool-refresh.service'; then
  sudo systemctl start opencode2api-pool-refresh.service || true
fi
sudo systemctl restart opencode2api

echo "[6/6] 校验 opencode2api"
for i in $(seq 1 15); do
  sleep 3
  if curl -sf -m 10 http://127.0.0.1:7872/healthz >/dev/null 2>&1; then break; fi
  echo "      等待 opencode2api [$i/15]"
done
echo -n "      healthz: "; curl -s -m 10 http://127.0.0.1:7872/healthz; echo
echo "[OK] 导入完成：本次注入 $alive_count 条（预筛通过率 $alive_count/$total）"
