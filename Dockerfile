# 构建阶段（使用完整 Debian 镜像，内置 gcc，避免 alpine apk 问题）
# 注意：go.mod 要求 go >= 1.26，构建镜像必须同步升级，否则 go build 直接失败；
# 固定到 1.26.9 以带上标准库安全修复（GO-2026-6603 ~ GO-2026-6617）。
FROM golang:1.26.9 AS builder

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=1 GOOS=linux go build -trimpath -o proxy-pool .

# 下载 sing-box 二进制
# 说明：1.13.5 与 custom/singbox.go 生成的配置已验证兼容；升 1.14.x 前需先跑
# `sing-box check -c <生成配置>` 回归（1.14 调整了部分 outbound/route 字段语义）。
ARG SINGBOX_VERSION=1.13.5
RUN ARCH=$(case "$(dpkg --print-architecture)" in amd64) echo "amd64";; arm64) echo "arm64";; *) echo "amd64";; esac) && \
    curl -fsSL "https://github.com/SagerNet/sing-box/releases/download/v${SINGBOX_VERSION}/sing-box-${SINGBOX_VERSION}-linux-${ARCH}.tar.gz" \
    -o /tmp/sing-box.tar.gz && \
    tar -xzf /tmp/sing-box.tar.gz -C /tmp && \
    cp /tmp/sing-box-${SINGBOX_VERSION}-linux-${ARCH}/sing-box /app/sing-box && \
    chmod +x /app/sing-box && \
    rm -rf /tmp/sing-box*

# 运行阶段（使用轻量 debian-slim）
FROM debian:bookworm-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates tzdata curl && \
    rm -rf /var/lib/apt/lists/*

ENV TZ=Asia/Shanghai
# 容器内必须监听所有网卡，否则 compose 端口映射不可达。
# 安全提示：映射到宿主机时请配合 PROXY_AUTH_ENABLED=true，或只映射到 127.0.0.1。
ENV PROXY_BIND=0.0.0.0
ENV WEBUI_BIND=0.0.0.0

WORKDIR /app
COPY --from=builder /app/proxy-pool .
COPY --from=builder /app/sing-box /usr/local/bin/sing-box

EXPOSE 7776 7777 7778 7779 7780

CMD ["./proxy-pool"]
