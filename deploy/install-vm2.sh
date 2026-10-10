#!/usr/bin/env bash
# GoProxy 一键部署脚本（Ubuntu 24.04 / x86_64，幂等，可重复执行）
#
# 用法：
#   REPO=https://github.com/<you>/GoProxy.git BRANCH=main bash deploy/install-vm2.sh
#
# 产出：
#   - /usr/local/go（Go 工具链，缺失时安装）
#   - ~/goproxy/proxy-pool（编译产物）+ data/ + subscriptions/
#   - /etc/goproxy/goproxy.env（随机口令、限速、绑定地址；已存在则保留）
#   - systemd 单元 goproxy.service（开机自启 + 崩溃重启 + 加固）
set -euo pipefail

REPO="${REPO:-https://github.com/383827453-max/GoProxy.git}"
BRANCH="${BRANCH:-main}"
APP_DIR="${APP_DIR:-$HOME/goproxy}"
ENV_DIR="${ENV_DIR:-/etc/goproxy}"
ENV_FILE="$ENV_DIR/goproxy.env"
GO_VERSION="${GO_VERSION:-1.26.9}"

echo "=== GoProxy 部署 ==="
echo "repo=$REPO branch=$BRANCH app_dir=$APP_DIR"

# 1. Go 工具链
if ! /usr/local/go/bin/go version >/dev/null 2>&1; then
  echo "[1/6] 安装 Go $GO_VERSION"
  curl -fsSL -o /tmp/go.tgz "https://go.dev/dl/go${GO_VERSION}.linux-amd64.tar.gz"
  sudo rm -rf /usr/local/go
  sudo tar -C /usr/local -xzf /tmp/go.tgz
  rm -f /tmp/go.tgz
else
  echo "[1/6] Go 已存在：$(/usr/local/go/bin/go version)"
fi
export PATH="$PATH:/usr/local/go/bin"

# 2. 源码
echo "[2/6] 同步源码"
if [ -d "$APP_DIR/.git" ]; then
  git -C "$APP_DIR" fetch --depth 1 origin "$BRANCH"
  git -C "$APP_DIR" checkout -B "$BRANCH" FETCH_HEAD
else
  git clone --depth 1 -b "$BRANCH" "$REPO" "$APP_DIR"
fi

# 3. 编译（mattn/go-sqlite3 需要 CGO + gcc）
echo "[3/6] 编译（CGO_ENABLED=1）"
cd "$APP_DIR"
CGO_ENABLED=1 go build -trimpath -ldflags "-s -w" -o proxy-pool .
mkdir -p "$APP_DIR/data" "$APP_DIR/subscriptions"
chmod 700 "$APP_DIR/data" "$APP_DIR/subscriptions"
echo "    产物：$(ls -lh proxy-pool | awk '{print $5}')"

# 4. 环境文件（随机口令；已存在则保留，避免每次部署轮换口令）
echo "[4/6] 环境文件 $ENV_FILE"
sudo mkdir -p "$ENV_DIR"
if [ ! -f "$ENV_FILE" ]; then
  gen() { openssl rand -base64 48 | tr -dc 'A-Za-z0-9' | head -c "$1"; }
  sudo tee "$ENV_FILE" >/dev/null <<EOF
# GoProxy 运行参数（$(date -Is) 自动生成；修改后 systemctl restart goproxy）
DATA_DIR=$APP_DIR/data

# 绑定：默认仅本机。同机消费（opencode2api / clash）保持 127.0.0.1 最安全；
# 需要跨机使用才改 0.0.0.0，并务必同时开启 PROXY_AUTH_ENABLED。
PROXY_BIND=127.0.0.1
WEBUI_BIND=127.0.0.1

# 管理面口令
WEBUI_PASSWORD=$(gen 24)
PROXY_AUTH_ENABLED=true
PROXY_AUTH_USERNAME=goproxy
PROXY_AUTH_PASSWORD=$(gen 24)
# /api/export 静态令牌（供下游脚本拉取池子）
EXPORT_TOKEN=$(gen 32)

# 出口 IP 查询限速：ip-api 免费档 45 次/分钟 → 0.6 rps（约 36/分钟）
EXITIP_RATE_LIMIT=0.6
EXITIP_BURST=5
# 两段式验证：先按连通性+延迟快速入池，再后台补齐出口位置并做地理过滤
REQUIRE_EXIT_INFO=false
BLOCKED_COUNTRIES=CN
ALLOWED_COUNTRIES=

# 订阅/贡献导入是否允许内网地址（SSRF 开关，默认 false）
ALLOW_PRIVATE_OUTBOUND=false
CUSTOM_PROXY_MODE=mixed
SINGBOX_PATH=sing-box
TZ=Asia/Shanghai
EOF
  sudo chmod 600 "$ENV_FILE"
  sudo chown root:root "$ENV_FILE"
  echo "    已生成（含随机口令）：sudo cat $ENV_FILE"
else
  echo "    已存在，保留现有口令"
fi

# 5. systemd
echo "[5/6] 安装 systemd 单元"
sudo cp "$APP_DIR/deploy/goproxy.service" /etc/systemd/system/goproxy.service
sudo systemctl daemon-reload
sudo systemctl enable --now goproxy

# 6. 健康检查
echo "[6/6] 健康检查"
ok=0
for i in $(seq 1 20); do
  sleep 3
  if curl -sf -m 5 "http://127.0.0.1:7778/api/stats" >/dev/null 2>&1; then ok=1; break; fi
  echo "    等待启动 [$i/20]"
done
if [ "$ok" != "1" ]; then
  echo "[!] WebUI 未就绪，最近日志："
  sudo journalctl -u goproxy -n 40 --no-pager
  exit 1
fi
echo -n "    stats       : "; curl -s -m 5 http://127.0.0.1:7778/api/stats; echo
echo -n "    pool status : "; curl -s -m 5 http://127.0.0.1:7778/api/pool/status; echo
echo -n "    监听端口    : "; ss -lnt | awk '{print $4}' | grep -E ':(7776|7777|7778|7779|7780)$' | tr '\n' ' '; echo
echo "[OK] 部署完成。日志：journalctl -u goproxy -f"
