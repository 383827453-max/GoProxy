package fetcher

import (
	"bufio"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"goproxy/netguard"
	"goproxy/storage"
)

// 代理来源定义
type Source struct {
	URL      string
	Protocol string // http 或 socks5
}

// 快速更新源（5-30分钟更新）- 用于紧急和补充模式
var fastUpdateSources = []Source{
	// ProxyScraper - 每30分钟更新
	{"https://raw.githubusercontent.com/ProxyScraper/ProxyScraper/main/http.txt", "http"},
	{"https://raw.githubusercontent.com/ProxyScraper/ProxyScraper/main/socks4.txt", "socks5"},
	{"https://raw.githubusercontent.com/ProxyScraper/ProxyScraper/main/socks5.txt", "socks5"},
	// monosans - 每小时更新
	{"https://raw.githubusercontent.com/monosans/proxy-list/main/proxies/http.txt", "http"},
	// prxchk - 频繁更新
	{"https://raw.githubusercontent.com/prxchk/proxy-list/main/http.txt", "http"},
	{"https://raw.githubusercontent.com/prxchk/proxy-list/main/socks5.txt", "socks5"},
	{"https://raw.githubusercontent.com/prxchk/proxy-list/main/socks4.txt", "socks5"},
	// sunny9577 - 自动抓取更新
	{"https://cdn.jsdelivr.net/gh/sunny9577/proxy-scraper/generated/http_proxies.txt", "http"},
	{"https://cdn.jsdelivr.net/gh/sunny9577/proxy-scraper/generated/socks5_proxies.txt", "socks5"},
	{"https://cdn.jsdelivr.net/gh/sunny9577/proxy-scraper/generated/socks4_proxies.txt", "socks5"},
}

// 慢速更新源（每天更新）- 用于优化轮换模式
var slowUpdateSources = []Source{
	// TheSpeedX - 每天更新，量大
	{"https://raw.githubusercontent.com/TheSpeedX/SOCKS-List/master/http.txt", "http"},
	{"https://raw.githubusercontent.com/TheSpeedX/SOCKS-List/master/socks4.txt", "socks5"},
	{"https://raw.githubusercontent.com/TheSpeedX/SOCKS-List/master/socks5.txt", "socks5"},
	// monosans SOCKS
	{"https://raw.githubusercontent.com/monosans/proxy-list/main/proxies/socks4.txt", "socks5"},
	{"https://raw.githubusercontent.com/monosans/proxy-list/main/proxies/socks5.txt", "socks5"},
	// databay-labs - 备用源
	{"https://cdn.jsdelivr.net/gh/databay-labs/free-proxy-list/http.txt", "http"},
	{"https://cdn.jsdelivr.net/gh/databay-labs/free-proxy-list/socks5.txt", "socks5"},
	// Anonym0usWork1221 - 量大质量尚可
	{"https://cdn.jsdelivr.net/gh/Anonym0usWork1221/Free-Proxies/proxy_files/http_proxies.txt", "http"},
	{"https://cdn.jsdelivr.net/gh/Anonym0usWork1221/Free-Proxies/proxy_files/socks5_proxies.txt", "socks5"},
	{"https://cdn.jsdelivr.net/gh/Anonym0usWork1221/Free-Proxies/proxy_files/socks4_proxies.txt", "socks5"},
	// ALIILAPRO
	{"https://cdn.jsdelivr.net/gh/ALIILAPRO/Proxy/http.txt", "http"},
	{"https://cdn.jsdelivr.net/gh/ALIILAPRO/Proxy/socks4.txt", "socks5"},
	// vakhov/fresh-proxy-list
	{"https://cdn.jsdelivr.net/gh/vakhov/fresh-proxy-list/http.txt", "http"},
	{"https://cdn.jsdelivr.net/gh/vakhov/fresh-proxy-list/socks5.txt", "socks5"},
	{"https://cdn.jsdelivr.net/gh/vakhov/fresh-proxy-list/socks4.txt", "socks5"},
	// Zaeem20
	{"https://cdn.jsdelivr.net/gh/Zaeem20/FREE_PROXIES_LIST/http.txt", "http"},
	{"https://cdn.jsdelivr.net/gh/Zaeem20/FREE_PROXIES_LIST/socks4.txt", "socks5"},
	// hookzof - socks5 专项
	{"https://cdn.jsdelivr.net/gh/hookzof/socks5_list/proxy.txt", "socks5"},
	// proxy4parsing
	{"https://cdn.jsdelivr.net/gh/proxy4parsing/proxy-list/http.txt", "http"},
	{"https://cdn.jsdelivr.net/gh/proxy4parsing/proxy-list/socks5.txt", "socks5"},
}

// 所有源
var allSources = append(fastUpdateSources, slowUpdateSources...)

type Fetcher struct {
	sources       []Source
	client        *http.Client
	sourceManager *SourceManager
}

func New(httpURL, socks5URL string, sourceManager *SourceManager) *Fetcher {
	return &Fetcher{
		sources:       allSources,
		sourceManager: sourceManager,
		client: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// FetchSmart 智能抓取：根据模式和协议需求选择源
func (f *Fetcher) FetchSmart(mode string, preferredProtocol string) ([]storage.Proxy, error) {
	var sources []Source

	switch mode {
	case "emergency":
		// 紧急模式：忽略断路器，强制使用所有源（包括被禁用的）
		sources = f.filterAvailableSources(allSources, preferredProtocol, true)
		log.Printf("[fetch] 🚨 紧急模式: 使用 %d 个源（忽略断路器）", len(sources))

	case "refill":
		// 补充模式：使用快更新源
		sources = f.filterAvailableSources(fastUpdateSources, preferredProtocol, false)
		log.Printf("[fetch] 🔄 补充模式: 使用 %d 个快更新源", len(sources))

	case "optimize":
		// 优化模式：随机选择2-3个慢更新源
		sources = f.selectRandomSources(slowUpdateSources, 3, preferredProtocol)
		log.Printf("[fetch] ⚡ 优化模式: 使用 %d 个源", len(sources))

	default:
		sources = f.filterAvailableSources(fastUpdateSources, preferredProtocol, false)
	}

	if len(sources) == 0 {
		return nil, fmt.Errorf("no available sources")
	}

	return f.fetchFromSources(sources)
}

// filterAvailableSources 过滤可用的源（通过断路器）
// ignoreCircuitBreaker: 是否忽略断路器（Emergency 模式下使用）
func (f *Fetcher) filterAvailableSources(sources []Source, preferredProtocol string, ignoreCircuitBreaker bool) []Source {
	var available []Source
	for _, src := range sources {
		// 检查断路器（紧急模式下忽略）
		if !ignoreCircuitBreaker && f.sourceManager != nil && !f.sourceManager.CanUseSource(src.URL) {
			continue
		}
		// 如果指定了协议偏好，优先该协议的源
		if preferredProtocol != "" && src.Protocol != "" && src.Protocol != preferredProtocol {
			continue
		}
		available = append(available, src)
	}
	return available
}

// selectRandomSources 随机选择N个源
func (f *Fetcher) selectRandomSources(sources []Source, count int, preferredProtocol string) []Source {
	available := f.filterAvailableSources(sources, preferredProtocol, false)
	if len(available) <= count {
		return available
	}

	// 随机打乱
	shuffled := make([]Source, len(available))
	copy(shuffled, available)
	for i := range shuffled {
		j := i + int(time.Now().UnixNano())%(len(shuffled)-i)
		shuffled[i], shuffled[j] = shuffled[j], shuffled[i]
	}

	return shuffled[:count]
}

// fetchFromSources 从指定源列表抓取
func (f *Fetcher) fetchFromSources(sources []Source) ([]storage.Proxy, error) {
	type result struct {
		proxies []storage.Proxy
		source  Source
		err     error
	}

	ch := make(chan result, len(sources))
	for _, src := range sources {
		go func(s Source) {
			proxies, err := f.fetchFromURL(s.URL, s.Protocol)
			ch <- result{proxies: proxies, source: s, err: err}
		}(src)
	}

	var all []storage.Proxy
	seen := make(map[string]bool)
	for range sources {
		r := <-ch
		if r.err != nil {
			log.Printf("[fetch] ❌ %s error: %v", r.source.URL, r.err)
			if f.sourceManager != nil {
				f.sourceManager.RecordFail(r.source.URL, 3, 5, 30)
			}
			continue
		}

		// 记录成功
		if f.sourceManager != nil {
			f.sourceManager.RecordSuccess(r.source.URL)
		}

		// 去重
		var deduped []storage.Proxy
		for _, p := range r.proxies {
			if !seen[p.Address] {
				seen[p.Address] = true
				deduped = append(deduped, p)
			}
		}
		log.Printf("[fetch] ✅ %d 个 %s 代理 from %s", len(deduped), r.source.Protocol, r.source.URL)
		all = append(all, deduped...)
	}

	if len(all) == 0 {
		return nil, fmt.Errorf("no proxies fetched")
	}
	log.Printf("[fetch] 总共抓取: %d 个代理（去重后）", len(all))
	return all, nil
}

// Fetch 从所有来源并发抓取代理
func (f *Fetcher) Fetch() ([]storage.Proxy, error) {
	type result struct {
		proxies []storage.Proxy
		source  Source
		err     error
	}

	ch := make(chan result, len(f.sources))
	for _, src := range f.sources {
		go func(s Source) {
			proxies, err := f.fetchFromURL(s.URL, s.Protocol)
			ch <- result{proxies: proxies, source: s, err: err}
		}(src)
	}

	var all []storage.Proxy
	seen := make(map[string]bool)
	for range f.sources {
		r := <-ch
		if r.err != nil {
			log.Printf("fetch %s error: %v", r.source.URL, r.err)
			continue
		}
		// 去重
		var deduped []storage.Proxy
		for _, p := range r.proxies {
			if !seen[p.Address] {
				seen[p.Address] = true
				deduped = append(deduped, p)
			}
		}
		log.Printf("fetched %d %s proxies from %s", len(deduped), r.source.Protocol, r.source.URL)
		all = append(all, deduped...)
	}

	if len(all) == 0 {
		return nil, fmt.Errorf("no proxies fetched")
	}
	log.Printf("total fetched: %d proxies (deduped)", len(all))
	return all, nil
}

func (f *Fetcher) fetchFromURL(url, protocol string) ([]storage.Proxy, error) {
	resp, err := f.client.Get(url)
	if err != nil {
		return nil, fmt.Errorf("get %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %d from %s", resp.StatusCode, url)
	}

	return parseProxyList(resp.Body, protocol)
}

// parseProxyList 解析公开源返回的代理列表。
//
// 归一化规则（旧实现只做 socks4→socks5，导致 "https://x.x.x.x:port" 被当成协议 https
// 入库、日志刷屏并浪费验证预算）：
//   - 带 scheme 的行：http/https → http，socks4/socks4a/socks5/socks5h → socks5，其余整行丢弃；
//   - 不带 scheme 的行：用源声明的默认协议；
//   - 仅接受字面量 IP:port，端口 1-65535；
//   - 环回 / 私网 / 链路本地 / CGNAT / 云元数据地址丢弃。
func parseProxyList(r io.Reader, protocol string) ([]storage.Proxy, error) {
	var proxies []storage.Proxy
	total, dropped := 0, 0
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		total++
		addr, proto, ok := netguard.NormalizeProxyAddress(scanner.Text(), protocol)
		if !ok {
			dropped++
			continue
		}
		proxies = append(proxies, storage.Proxy{
			Address:  addr,
			Protocol: proto,
		})
	}
	if dropped > 0 {
		log.Printf("[fetch] 解析 %d 行，丢弃 %d 行（未知 scheme / 非 IP / 保留地址），入库候选 %d", total, dropped, len(proxies))
	}
	return proxies, scanner.Err()
}

// SourceURLs 返回内置源清单，供运维/测试核对（不再需要读源码）。
func SourceURLs() []string {
	out := make([]string, 0, len(allSources))
	for _, s := range allSources {
		out = append(out, s.URL)
	}
	return out
}

var _ = strings.TrimSpace
