#!/usr/bin/env bash
# Oracle 一次性引导：建 admin 口令 → 装 goproxy→opencode2api 同步脚本与 timer → 首跑验证
# 幂等：重复执行只覆盖文件与单元，不轮换已有口令
set -euo pipefail

OC_DIR=/opt/opencode2api
PASSFILE=/root/.opencode2api_admin
SCRIPT=/usr/local/bin/goproxy-pool-sync.sh

echo "=== [1/5] admin 管理口令（热重载用） ==="
if [ -s "$PASSFILE" ]; then
  echo "  已存在 $PASSFILE，保留"
else
  cp -a "$OC_DIR/config.json" "$OC_DIR/config.json.bak-goproxy-sync-$(date +%Y%m%d-%H%M%S)"
  PW="$(openssl rand -base64 48 | tr -dc 'A-Za-z0-9' | head -c 28)"
  printf '%s' "$PW" > "$PASSFILE"; chmod 600 "$PASSFILE"
  python3 - "$OC_DIR/config.json" "$PW" <<'PY'
import json, sys
p, pw = sys.argv[1], sys.argv[2]
c = json.load(open(p, encoding='utf-8'))
c.setdefault('webui', {})['password'] = pw          # 应用启动时自行转 argon2id 哈希
json.dump(c, open(p, 'w', encoding='utf-8'), indent=2, ensure_ascii=False)
print(f"  config.json webui.password 已写入（listen={c['webui'].get('listen')}）")
PY
  chown oc2api:oc2api "$OC_DIR/config.json"
  systemctl restart opencode2api
  for i in $(seq 1 15); do sleep 2; systemctl is-active --quiet opencode2api && break; done
  echo "  opencode2api: $(systemctl is-active opencode2api)"
fi

echo "=== [2/5] 安装同步脚本 ==="
install -m 0755 -o root -g root /tmp/goproxy-pool-sync.sh "$SCRIPT"
bash -n "$SCRIPT" && echo "  语法校验 OK → $SCRIPT"

echo "=== [3/5] 安装 systemd 单元（15 分钟一轮） ==="
cat > /etc/systemd/system/goproxy-pool-sync.service <<'EOF'
[Unit]
Description=Sync GoProxy verified proxies into opencode2api pool (hot reload)
After=network-online.target goproxy.service opencode2api.service
Wants=network-online.target

[Service]
Type=oneshot
User=root
Environment=OC_DIR=/opt/opencode2api
Environment=PARALLEL=16
Environment=LIMIT=200
Environment=MAX_LATENCY=3000
Environment=TEST_URL=https://opencode.ai/zen/v1/models
Environment=PASSFILE=/root/.opencode2api_admin
ExecStart=/usr/local/bin/goproxy-pool-sync.sh
TimeoutStartSec=900
Nice=10
EOF

cat > /etc/systemd/system/goproxy-pool-sync.timer <<'EOF'
[Unit]
Description=Periodic sync of GoProxy verified proxies into opencode2api pool

[Timer]
OnBootSec=5min
OnUnitActiveSec=15min
Persistent=true
Unit=goproxy-pool-sync.service

[Install]
WantedBy=timers.target
EOF
systemctl daemon-reload
systemctl enable --now goproxy-pool-sync.timer
systemctl list-timers goproxy-pool-sync.timer --no-pager | head -3

echo "=== [4/5] 首跑 ==="
systemctl start goproxy-pool-sync.service
sleep 45
systemctl is-active goproxy-pool-sync.service || true
journalctl -u goproxy-pool-sync.service -n 20 --no-pager | tail -14

echo "=== [5/5] 结果核对 ==="
echo -n "  proxies.txt 总条数: "; grep -c . "$OC_DIR/proxies.txt"
echo -n "  其中 goproxy 来源: "; grep -c . "$OC_DIR/proxies_goproxy.txt" 2>/dev/null || echo 0
echo -n "  healthz: "; curl -s -m 10 http://127.0.0.1:7872/healthz
echo
echo "[DONE] admin 口令在 $PASSFILE（改过 webui 口令，旧登录失效）"
