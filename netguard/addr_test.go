package netguard

import "testing"

func TestNormalizeProxyAddress(t *testing.T) {
	cases := []struct {
		name    string
		line    string
		def     string
		wantAdr string
		wantPro string
		wantOK  bool
	}{
		{"plain ipv4 http", "1.2.3.4:8080", "http", "1.2.3.4:8080", "http", true},
		{"plain ipv4 socks", "1.2.3.4:1080", "socks5", "1.2.3.4:1080", "socks5", true},
		{"http scheme", "http://1.2.3.4:3128", "socks5", "1.2.3.4:3128", "http", true},
		{"https scheme normalizes to http", "https://1.2.3.4:443", "http", "1.2.3.4:443", "http", true},
		{"socks4 scheme normalizes to socks5", "socks4://1.2.3.4:1080", "http", "1.2.3.4:1080", "socks5", true},
		{"socks5h scheme", "socks5h://1.2.3.4:1080", "http", "1.2.3.4:1080", "socks5", true},
		{"unknown scheme dropped", "vmess://1.2.3.4:443", "http", "", "", false},
		{"trojan scheme dropped", "trojan://1.2.3.4:443", "http", "", "", false},
		{"blank line", "   ", "http", "", "", false},
		{"comment line", "# note", "http", "", "", false},
		{"missing port", "1.2.3.4", "http", "", "", false},
		{"bad port", "1.2.3.4:70000", "http", "", "", false},
		{"zero port", "1.2.3.4:0", "http", "", "", false},
		{"hostname rejected", "proxy.example.com:8080", "http", "", "", false},
		{"loopback rejected", "127.0.0.1:8080", "http", "", "", false},
		{"private rejected", "10.0.0.5:8080", "http", "", "", false},
		{"metadata rejected", "169.254.169.254:80", "http", "", "", false},
		{"cgnat rejected", "100.64.1.1:8080", "http", "", "", false},
		{"ipv6 public kept", "[2606:4700:4700::1111]:443", "http", "[2606:4700:4700::1111]:443", "http", true},
		{"trailing path stripped", "1.2.3.4:8080/path", "http", "1.2.3.4:8080", "http", true},
		{"credentials stripped", "user:pass@1.2.3.4:8080", "http", "1.2.3.4:8080", "http", true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			addr, proto, ok := NormalizeProxyAddress(c.line, c.def)
			if ok != c.wantOK {
				t.Fatalf("ok = %v, want %v (addr=%q proto=%q)", ok, c.wantOK, addr, proto)
			}
			if !ok {
				return
			}
			if addr != c.wantAdr || proto != c.wantPro {
				t.Fatalf("got (%q,%q), want (%q,%q)", addr, proto, c.wantAdr, c.wantPro)
			}
		})
	}
}

func TestIsBlockedIP(t *testing.T) {
	blocked := []string{"127.0.0.1", "::1", "10.1.2.3", "192.168.1.1", "172.16.0.1", "169.254.169.254", "100.64.0.1", "0.0.0.0", "fe80::1", "fc00::1"}
	for _, s := range blocked {
		if !IsBlockedHost(s) {
			t.Fatalf("%s 应被判定为禁止地址", s)
		}
	}
	allowed := []string{"1.1.1.1", "8.8.8.8", "104.16.0.1", "2606:4700:4700::1111"}
	for _, s := range allowed {
		if IsBlockedHost(s) {
			t.Fatalf("%s 不应被判定为禁止地址", s)
		}
	}
}

func TestFormatProxyURI(t *testing.T) {
	if got := FormatProxyURI("socks5", "1.2.3.4:1080"); got != "socks5://1.2.3.4:1080" {
		t.Fatalf("unexpected uri: %s", got)
	}
	if got := FormatProxyURI("weird", "1.2.3.4:1080"); got != "http://1.2.3.4:1080" {
		t.Fatalf("unknown protocol should fall back to http, got %s", got)
	}
}
