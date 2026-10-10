// Package netguard 提供代理池的地址归一化、出站 URL 防护与外部 API 限流。
//
// 该包被 fetcher / validator / webui 共用：
//   - NormalizeProxyAddress 负责把公开源里的脏行归一化成 host:port + 协议；
//   - IsBlockedIP / GuardOutboundURL 负责阻止服务端被诱导访问内网（SSRF）；
//   - InitExitIPLimiter / WaitExitIP 负责给免费额度极低的出口 IP 查询做全局限速。
package netguard

import (
	"fmt"
	"net"
	"strconv"
	"strings"
)

// NormalizeProxyAddress 归一化一条代理列表行，返回 host:port 与协议（http / socks5）。
// 无法安全使用时 ok=false：未知 scheme、非字面量 IP、端口越界、内网/保留段地址一律丢弃。
func NormalizeProxyAddress(line, defaultProtocol string) (address string, protocol string, ok bool) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return "", "", false
	}

	proto := defaultProtocol
	rest := line
	if i := strings.Index(line, "://"); i >= 0 {
		scheme := strings.ToLower(strings.TrimSpace(line[:i]))
		rest = line[i+3:]
		switch scheme {
		case "http", "https":
			proto = "http"
		case "socks5", "socks5h", "socks4", "socks4a":
			proto = "socks5"
		default:
			// vmess:// / trojan:// / 源站自己发明的标记：一律丢弃，
			// 避免出现 "unknown protocol https" 这类刷屏并浪费验证预算。
			return "", "", false
		}
	}

	if i := strings.IndexAny(rest, "/?#"); i >= 0 {
		rest = rest[:i]
	}
	if at := strings.LastIndex(rest, "@"); at >= 0 {
		rest = rest[at+1:]
	}

	host, portStr, err := net.SplitHostPort(rest)
	if err != nil {
		return "", "", false
	}
	port, err := strconv.Atoi(strings.TrimSpace(portStr))
	if err != nil || port < 1 || port > 65535 {
		return "", "", false
	}

	ip := net.ParseIP(host)
	if ip == nil {
		// 只接受字面量 IP：免费源里的域名条目绝大多数是自指或垃圾。
		return "", "", false
	}
	if IsBlockedIP(ip) {
		return "", "", false
	}
	if proto != "http" && proto != "socks5" {
		return "", "", false
	}

	return net.JoinHostPort(ip.String(), strconv.Itoa(port)), proto, true
}

// IsBlockedIP 判断是否为环回 / 未指定 / 组播 / 链路本地 / 私网 / 云元数据等不可路由地址。
func IsBlockedIP(ip net.IP) bool {
	return IsAlwaysBlockedIP(ip) || IsPrivateOnlyIP(ip)
}

// IsAlwaysBlockedIP 无论是否显式放开内网都必须拒绝的地址：
// 未指定、组播、链路本地（含 169.254.169.254 云元数据）、0.0.0.0/8、224.0.0.0/4。
func IsAlwaysBlockedIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if ip.IsUnspecified() || ip.IsMulticast() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return true
	}
	v4 := ip.To4()
	if v4 == nil {
		return false
	}
	switch {
	case v4[0] == 0, v4[0] >= 224:
		return true
	case v4[0] == 169 && v4[1] == 254:
		return true
	}
	return false
}

// IsPrivateOnlyIP 属于"内网/保留"但允许在 ALLOW_PRIVATE_OUTBOUND=true 时放行的地址：
// 环回、RFC1918 私网、CGNAT(100.64/10)、TEST-NET、192.0.0/24、198.18/15。
func IsPrivateOnlyIP(ip net.IP) bool {
	if ip == nil || IsAlwaysBlockedIP(ip) {
		return false
	}
	if ip.IsLoopback() || ip.IsPrivate() {
		return true
	}
	v4 := ip.To4()
	if v4 == nil {
		return false
	}
	switch {
	case v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127: // 100.64.0.0/10 CGNAT
		return true
	case v4[0] == 192 && v4[1] == 0 && v4[2] == 0: // 192.0.0.0/24
		return true
	case v4[0] == 192 && v4[1] == 0 && v4[2] == 2: // TEST-NET-1
		return true
	case v4[0] == 198 && (v4[1] == 18 || v4[1] == 19): // 198.18.0.0/15
		return true
	case v4[0] == 198 && v4[1] == 51 && v4[2] == 100: // TEST-NET-2
		return true
	case v4[0] == 203 && v4[1] == 0 && v4[2] == 113: // TEST-NET-3
		return true
	}
	return false
}

// HostBlockKind 分类主机名：always=无条件拒绝；privateOnly=仅在未放开内网时拒绝。
func HostBlockKind(host string) (always bool, privateOnly bool) {
	h := strings.ToLower(strings.TrimSpace(host))
	if h == "" {
		return true, false
	}
	switch h {
	case "metadata", "metadata.google.internal", "instance-data":
		return true, false
	}
	for _, suffix := range []string{".internal", ".in-addr.arpa", ".home.arpa"} {
		if strings.HasSuffix(h, suffix) {
			return true, false
		}
	}
	if ip := net.ParseIP(h); ip != nil {
		if IsAlwaysBlockedIP(ip) {
			return true, false
		}
		if IsPrivateOnlyIP(ip) {
			return false, true
		}
		return false, false
	}
	switch h {
	case "localhost", "localhost.localdomain":
		return false, true
	}
	if strings.HasSuffix(h, ".localhost") || strings.HasSuffix(h, ".local") {
		return false, true
	}
	return false, false
}

// IsBlockedHost 对主机名字面量做判断（不解析 DNS），等价于"私网 + 无条件拒绝"的并集。
func IsBlockedHost(host string) bool {
	always, privateOnly := HostBlockKind(host)
	return always || privateOnly
}

// FormatProxyURI 输出下游消费方（opencode2 / clash / 环境变量）通用的代理 URI。
func FormatProxyURI(protocol, address string) string {
	p := strings.ToLower(strings.TrimSpace(protocol))
	if p != "http" && p != "socks5" {
		p = "http"
	}
	return fmt.Sprintf("%s://%s", p, address)
}

// Subnet24 返回 IPv4 的 /24 网段前缀（形如 "1.2.3."），用于"同 C 段限制"。
// 非 IPv4 或解析失败返回空串（调用方应忽略该样本）。
func Subnet24(ip string) string {
	v4 := net.ParseIP(strings.TrimSpace(ip)).To4()
	if v4 == nil {
		return ""
	}
	return fmt.Sprintf("%d.%d.%d.", v4[0], v4[1], v4[2])
}

// Subnet24Filter 把网段前缀转成 SQL LIKE 模式（"1.2.3." → "1.2.3.%"）。
func Subnet24Filter(prefix string) string {
	p := strings.TrimSpace(prefix)
	if p == "" {
		return ""
	}
	return strings.TrimSuffix(p, "%") + "%"
}
