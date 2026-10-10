package netguard

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"
)

// GuardOutboundURL 校验服务端即将主动拉取的 URL（订阅导入、访客贡献、定时刷新）。
// 用于阻止 SSRF：file://、内联凭证、内网/保留段 IP、指向内网的域名都会被拒绝。
// allowPrivate=true 时只做 scheme 校验，供确有内网自建订阅的场景显式放开。
func GuardOutboundURL(raw string, allowPrivate bool) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fmt.Errorf("空 URL")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("URL 解析失败: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("仅允许 http/https，收到 %q", u.Scheme)
	}
	if u.User != nil {
		return fmt.Errorf("URL 不得包含内联凭证")
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("URL 缺少主机")
	}
	always, privateOnly := HostBlockKind(host)
	if always {
		return fmt.Errorf("目标主机被拒绝: %s", host)
	}
	if privateOnly && !allowPrivate {
		return fmt.Errorf("目标主机属于内网/环回: %s", host)
	}
	if allowPrivate {
		return nil
	}
	if ip := net.ParseIP(host); ip != nil {
		return nil
	}

	// 域名：解析后逐个校验，避免域名指向内网。
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return fmt.Errorf("DNS 解析失败: %w", err)
	}
	if len(addrs) == 0 {
		return fmt.Errorf("DNS 无结果: %s", host)
	}
	for _, a := range addrs {
		if IsAlwaysBlockedIP(a.IP) {
			return fmt.Errorf("域名 %s 解析到禁止地址 %s", host, a.IP)
		}
		if IsPrivateOnlyIP(a.IP) {
			return fmt.Errorf("域名 %s 解析到内网地址 %s", host, a.IP)
		}
	}
	return nil
}
