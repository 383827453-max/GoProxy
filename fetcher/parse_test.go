package fetcher

import (
	"strings"
	"testing"
)

func TestParseProxyListNormalizesAndFilters(t *testing.T) {
	in := strings.Join([]string{
		"# comment",
		"",
		"1.2.3.4:8080",
		"5.6.7.8:1080",
		"http://9.9.9.9:3128",
		"https://8.8.8.8:443",    // 旧实现会生成 protocol=https 并刷屏
		"socks4://7.7.7.7:1080",  // 应归一化为 socks5
		"vmess://6.6.6.6:443",    // 未知 scheme 丢弃
		"127.0.0.1:8080",         // 环回丢弃
		"169.254.169.254:80",     // 元数据地址丢弃
		"proxy.example.com:8080", // 非字面量 IP 丢弃
		"1.2.3.4",                // 缺端口丢弃
		"1.2.3.4:99999",          // 端口越界丢弃
		"  10.0.0.1:3128  ",      // 私网丢弃
	}, "\n")

	got, err := parseProxyList(strings.NewReader(in), "socks5")
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	// 有效行：1.2.3.4:8080 / 5.6.7.8:1080 / 9.9.9.9:3128 / 8.8.8.8:443 / 7.7.7.7:1080
	if len(got) != 5 {
		t.Fatalf("期望 5 条有效代理，实际 %d: %+v", len(got), got)
	}

	// 无 scheme 的行沿用源默认协议
	if got[0].Address != "1.2.3.4:8080" || got[0].Protocol != "socks5" {
		t.Fatalf("第 1 条异常: %+v", got[0])
	}
	// 不应再出现任何 https / socks4 协议条目
	for _, p := range got {
		if p.Protocol != "http" && p.Protocol != "socks5" {
			t.Fatalf("出现非法协议 %q（%s）", p.Protocol, p.Address)
		}
	}
	if got[2].Address != "9.9.9.9:3128" || got[2].Protocol != "http" {
		t.Fatalf("http:// 行归一化失败: %+v", got[2])
	}
	if got[3].Address != "8.8.8.8:443" || got[3].Protocol != "http" {
		t.Fatalf("https:// 行应归一化为 http: %+v", got[3])
	}
	if got[4].Address != "7.7.7.7:1080" || got[4].Protocol != "socks5" {
		t.Fatalf("socks4:// 行归一化失败: %+v", got[4])
	}
}

func TestBuiltinSourcesNotEmpty(t *testing.T) {
	urls := SourceURLs()
	if len(urls) < 10 {
		t.Fatalf("内置源数量异常: %d", len(urls))
	}
	for _, u := range urls {
		if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
			t.Fatalf("源 URL 非法: %s", u)
		}
	}
}
