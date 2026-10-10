#!/usr/bin/env bash
# 安装"定时把 GoProxy 已验证代理同步进 opencode2api 池"的 systemd timer。
# 幂等：重复执行只覆盖单元文件并 restart timer。
set -euo pipefail

APP_DIR="${APP_DIR:-$HOME/goproxy}"
INTERVAL="${INTERVAL:-15min}"
OC_DIR="${OC_DIR:-$HOME/opencode2api}"

sudo tee /etc/systemd/system/goproxy-export-opencode2.service >/dev/null <<EOF
[Unit]
Description=Export verified GoProxy pool entries into opencode2api pool
After=network-online.target goproxy.service opencode2api.service
Wants=network-online.target

[Service]
Type=oneshot
User=$(id -un)
WorkingDirectory=$APP_DIR
# 目标站预筛并发：免费代理质量参差，32 路足够且不会打爆目标站
Environment=PARALLEL=32
Environment=LIMIT=300
Environment=MAX_LATENCY=3000
Environment=TEST_URL=https://opencode.ai/zen/v1/models
Environment=OC_DIR=$OC_DIR
ExecStart=/usr/bin/env bash $APP_DIR/deploy/export-to-opencode2.sh
TimeoutStartSec=900
Nice=10
EOF

sudo tee /etc/systemd/system/goproxy-export-opencode2.timer >/dev/null <<EOF
[Unit]
Description=Periodic sync of GoProxy verified proxies into opencode2api pool

[Timer]
OnBootSec=5min
OnUnitActiveSec=$INTERVAL
Persistent=true
Unit=goproxy-export-opencode2.service

[Install]
WantedBy=timers.target
EOF

sudo systemctl daemon-reload
sudo systemctl enable --now goproxy-export-opencode2.timer
echo "=== timer 状态 ==="
systemctl list-timers goproxy-export-opencode2.timer --no-pager
echo "=== 立即跑一次（验证单元可用）==="
sudo systemctl start goproxy-export-opencode2.service
sleep 45
systemctl is-active goproxy-export-opencode2.service || true
sudo journalctl -u goproxy-export-opencode2.service -n 12 --no-pager
