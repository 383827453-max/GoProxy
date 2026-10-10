#!/usr/bin/env bash
# 把 GoProxy 池中已通过验证的代理导入 opencode2api 的"公开补充池"，并触发合并/热重载。
#
# 用法：
#   sudo cat /etc/goproxy/goproxy.env   # 取 EXPORT_TOKEN
#   EXPORT_TOKEN=xxx bash deploy/export-to-opencode2.sh
#
# 幂等：每次运行会替换上一次注入的区块（# >>> goproxy-pool:start ... end），
# 不会污染 opencode2api 原有的 master（商业认证）池。
set -euo pipefail

GOPROXY_BASE="${GOPROXY_BASE:-http://127.0.0.1:7778}"
OC_DIR="${OC_DIR:-$HOME/opencode2api}"
PUBLIC_POOL="${PUBLIC_POOL:-$OC_DIR/proxies_public.txt}"
MERGED_POOL="${MERGED_POOL:-$OC_DIR/proxies.txt}"
MASTER_POOL="${MASTER_POOL:-$OC_DIR/proxies_master.txt}"
LIMIT="${LIMIT:-300}"
MAX_LATENCY="${MAX_LATENCY:-3000}"
PROTOCOL="${PROTOCOL:-socks5}"

if [ -z "${EXPORT_TOKEN:-}" ] && [ -r /etc/goproxy/goproxy.env ]; then
  EXPORT_TOKEN="$(sudo sed -n 's/^EXPORT_TOKEN=//p' /etc/goproxy/goproxy.env | head -1)"
fi
if [ -z "${EXPORT_TOKEN:-}" ]; then
  echo "[!] 缺少 EXPORT_TOKEN（导出接口鉴权）"; exit 1
fi

tmp="$(mktemp)"
trap 'rm -f "$tmp"' EXIT

echo "[1/5] 从 GoProxy 导出 $PROTOCOL 代理（max_latency=${MAX_LATENCY}ms limit=$LIMIT）"
curl -fsS -H "X-Export-Token: $EXPORT_TOKEN" \
  "$GOPROXY_BASE/api/export?protocol=$PROTOCOL&source=free&format=uri&max_latency=$MAX_LATENCY&limit=$LIMIT" \
  -o "$tmp"
count="$(grep -c . "$tmp" || true)"
echo "      导出 $count 条"
if [ "${count:-0}" -eq 0 ]; then
  echo "[!] 导出为空（池子可能还在填充或全部被地理过滤），放弃写入以免清空下游池"
  exit 2
fi

echo "[2/5] 写入公开补充池 $PUBLIC_POOL（替换上一次注入区块）"
python3 - "$PUBLIC_POOL" "$tmp" <<'PY'
import pathlib, re, sys
pool_path, tmp_path = sys.argv[1], sys.argv[2]
p = pathlib.Path(pool_path)
body = p.read_text(encoding="utf-8") if p.exists() else ""
body = re.sub(r"\n?# >>> goproxy-pool:start\n.*?# <<< goproxy-pool:end\n?", "\n", body, flags=re.S)
new = pathlib.Path(tmp_path).read_text(encoding="utf-8").strip()
block = "# >>> goproxy-pool:start\n" + new + "\n# <<< goproxy-pool:end\n"
p.write_text(body.rstrip("\n") + "\n\n" + block, encoding="utf-8")
print(f"      [+] {p} 现有 {len([l for l in p.read_text().splitlines() if l and not l.startswith('#')])} 条")
PY

echo "[3/5] 合并 master + public → proxies.txt"
if [ -f "$MASTER_POOL" ]; then
  cat "$MASTER_POOL" "$PUBLIC_POOL" | awk 'NF' | sort -u > "$MERGED_POOL"
else
  cp "$PUBLIC_POOL" "$MERGED_POOL"
fi
echo "      proxies.txt 共 $(grep -c . "$MERGED_POOL" || true) 条"

echo "[4/5] 触发热重载"
if [ -f "$OC_DIR/proxyscraper_refresh.py" ] && systemctl list-unit-files | grep -q '^opencode2api-pool-refresh.service'; then
  sudo systemctl start opencode2api-pool-refresh.service || true
fi
sudo systemctl restart opencode2api

echo "[5/5] 校验 opencode2api"
for i in $(seq 1 15); do
  sleep 3
  if curl -sf -m 10 http://127.0.0.1:7872/healthz >/dev/null 2>&1; then break; fi
  echo "      等待 opencode2api [$i/15]"
done
echo -n "      healthz: "; curl -s -m 10 http://127.0.0.1:7872/healthz; echo
echo "[OK] 导入完成。池内 goproxy 条目：$(grep -c 'goproxy-pool' "$MERGED_POOL" || true) 行区块标记"
