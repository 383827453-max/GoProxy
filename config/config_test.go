package config

import "testing"

func TestJoinBind(t *testing.T) {
	cases := []struct {
		bind, port, want string
	}{
		{"127.0.0.1", ":7777", "127.0.0.1:7777"},
		{"0.0.0.0", ":7777", ":7777"},
		{"", ":7777", "127.0.0.1:7777"},
		{"10.0.0.5", ":7777", "10.0.0.5:7777"},
		{"127.0.0.1", "7777", "127.0.0.1:7777"},
	}
	for _, c := range cases {
		if got := joinBind(c.bind, c.port, "127.0.0.1"); got != c.want {
			t.Fatalf("joinBind(%q,%q) = %q, want %q", c.bind, c.port, got, c.want)
		}
	}
}

func TestProxyAddrRespectsExplicitHost(t *testing.T) {
	cfg := &Config{ProxyBind: "127.0.0.1"}
	if got := cfg.ProxyAddr(":7777"); got != "127.0.0.1:7777" {
		t.Fatalf("unexpected: %s", got)
	}
	// 端口里已写死 host 时保持原样
	cfg2 := &Config{ProxyBind: ""}
	if got := cfg2.ProxyAddr("192.0.2.9:7777"); got != "192.0.2.9:7777" {
		t.Fatalf("unexpected: %s", got)
	}
}

func TestCalculateSlots(t *testing.T) {
	cfg := &Config{PoolMaxSize: 100, PoolHTTPRatio: 0.3, PoolMinPerProtocol: 10}
	h, s := cfg.CalculateSlots()
	if h != 30 || s != 70 {
		t.Fatalf("slots = (%d,%d), want (30,70)", h, s)
	}
	// 比例极端时仍保证每协议最小值
	cfg2 := &Config{PoolMaxSize: 20, PoolHTTPRatio: 0.95, PoolMinPerProtocol: 10}
	h2, s2 := cfg2.CalculateSlots()
	if h2 < 10 || s2 < 10 {
		t.Fatalf("每协议最小值未生效: (%d,%d)", h2, s2)
	}
}

func TestDefaultSecurityPosture(t *testing.T) {
	// 默认必须只绑本机、强制要求出口信息、不允许内网出站
	cfg := DefaultConfig()
	if cfg.ProxyBind != "127.0.0.1" || cfg.WebUIBind != "127.0.0.1" {
		t.Fatalf("默认绑定地址不安全: proxy=%q webui=%q", cfg.ProxyBind, cfg.WebUIBind)
	}
	if !cfg.RequireExitInfo {
		t.Fatal("默认应要求出口信息")
	}
	if cfg.AllowPrivateOutbound {
		t.Fatal("默认不应允许内网出站")
	}
	if cfg.ExitIPRateLimit > 1 {
		t.Fatalf("默认出口 IP 查询速率过高: %v rps", cfg.ExitIPRateLimit)
	}
}
