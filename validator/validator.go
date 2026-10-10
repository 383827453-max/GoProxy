package validator

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"sync"
	"time"

	"golang.org/x/net/proxy"
	"goproxy/config"
	"goproxy/netguard"
	"goproxy/storage"
)

type Validator struct {
	concurrency   int
	timeout       time.Duration
	validateURL   string
	maxResponseMs int
	cfg           *config.Config
}

func concurrencyBuffer(total, concurrency int) int {
	if total < concurrency*10 {
		return total
	}
	return concurrency * 10
}

func New(concurrency, timeoutSec int, validateURL string) *Validator {
	cfg := config.Get()
	maxMs := 0
	if cfg != nil {
		maxMs = cfg.MaxResponseMs
	}
	return &Validator{
		concurrency:   concurrency,
		timeout:       time.Duration(timeoutSec) * time.Second,
		validateURL:   validateURL,
		maxResponseMs: maxMs,
		cfg:           cfg,
	}
}

type Result struct {
	Proxy        storage.Proxy
	Valid        bool
	Latency      time.Duration
	ExitIP       string
	ExitLocation string
}

// getExitIPInfo 通过代理获取出口 IP 和地理位置（多源降级 + 全局限速）。
//
// 限速原因：ip-api.com 免费档 45 次/分钟，而验证并发默认 300；不限速时会被瞬间打爆
// （实测返回 503 Too many open connections），结果所有代理都因"拿不到出口信息"被拒。
func getExitIPInfo(client *http.Client) (string, string) {
	if !netguard.WaitExitIP(3 * time.Second) {
		return "", ""
	}
	if ip, loc := tryIPAPI(client); ip != "" {
		return ip, loc
	}
	if ip, loc := tryIPAPICo(client); ip != "" {
		return ip, loc
	}
	if ip, loc := tryIPInfo(client); ip != "" {
		return ip, loc
	}
	return "", ""
}

// tryIPAPI 主源：ip-api.com（免费档仅 http，45 次/分钟）
func tryIPAPI(client *http.Client) (string, string) {
	resp, err := client.Get("http://ip-api.com/json/?fields=status,country,countryCode,city,query")
	if err != nil {
		return "", ""
	}
	defer resp.Body.Close()

	var result struct {
		Status      string `json:"status"`
		Query       string `json:"query"`
		Country     string `json:"country"`
		CountryCode string `json:"countryCode"`
		City        string `json:"city"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil || result.Status != "success" {
		return "", ""
	}
	location := result.CountryCode
	if result.City != "" {
		location = fmt.Sprintf("%s %s", result.CountryCode, result.City)
	}
	return result.Query, location
}

// tryIPAPICo 降级源：ipapi.co（HTTPS）
func tryIPAPICo(client *http.Client) (string, string) {
	resp, err := client.Get("https://ipapi.co/json/")
	if err != nil {
		return "", ""
	}
	defer resp.Body.Close()

	var result struct {
		IP          string `json:"ip"`
		City        string `json:"city"`
		CountryCode string `json:"country_code"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", ""
	}
	location := result.CountryCode
	if result.City != "" {
		location = fmt.Sprintf("%s %s", result.CountryCode, result.City)
	}
	return result.IP, location
}

// tryIPInfo 降级源：ipinfo.io（HTTPS）
func tryIPInfo(client *http.Client) (string, string) {
	resp, err := client.Get("https://ipinfo.io/json")
	if err != nil {
		return "", ""
	}
	defer resp.Body.Close()

	var result struct {
		IP      string `json:"ip"`
		City    string `json:"city"`
		Country string `json:"country"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", ""
	}
	location := result.Country
	if result.City != "" {
		location = fmt.Sprintf("%s %s", result.Country, result.City)
	}
	return result.IP, location
}

// HTTPS 测试目标列表，随机选一个验证代理的 CONNECT 隧道能力
var httpsTestTargets = []string{
	"https://www.google.com",
	"https://www.openai.com",
	"https://www.github.com",
	"https://www.cloudflare.com",
	"https://httpbin.org/ip",
}

// checkHTTPSConnect 通过 HTTP 代理实际访问一个随机 HTTPS 网站，验证 CONNECT 隧道是否可用
// 首次失败会换一个目标重试一次，避免目标网站偶尔抽风导致误杀
func checkHTTPSConnect(proxyAddr string, timeout time.Duration) bool {
	proxyURL, err := url.Parse(fmt.Sprintf("http://%s", proxyAddr))
	if err != nil {
		return false
	}

	client := &http.Client{
		Transport: &http.Transport{
			Proxy:               http.ProxyURL(proxyURL),
			TLSHandshakeTimeout: timeout,
		},
		Timeout: timeout,
	}

	// 随机起始索引
	start := int(time.Now().UnixNano() % int64(len(httpsTestTargets)))

	for attempt := 0; attempt < 2; attempt++ {
		idx := (start + attempt) % len(httpsTestTargets)
		resp, err := client.Get(httpsTestTargets[idx])
		if err != nil {
			continue
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()

		// 2xx 或 3xx 都算成功（部分网站会重定向）
		if resp.StatusCode >= 200 && resp.StatusCode < 400 {
			return true
		}
	}

	return false
}

// ValidateAll 并发验证所有代理，返回验证结果
func (v *Validator) ValidateAll(proxies []storage.Proxy) []Result {
	var results []Result
	for r := range v.ValidateStream(proxies) {
		results = append(results, r)
	}
	return results
}

// ValidateStream 并发验证，边验证边通过 channel 返回结果
func (v *Validator) ValidateStream(proxies []storage.Proxy) <-chan Result {
	ch := make(chan Result, concurrencyBuffer(len(proxies), v.concurrency))
	sem := make(chan struct{}, v.concurrency)
	var wg sync.WaitGroup

	go func() {
		for _, p := range proxies {
			wg.Add(1)
			sem <- struct{}{}
			go func(px storage.Proxy) {
				defer wg.Done()
				defer func() { <-sem }()
				valid, latency, exitIP, exitLocation := v.ValidateOne(px)
				ch <- Result{Proxy: px, Valid: valid, Latency: latency, ExitIP: exitIP, ExitLocation: exitLocation}
			}(p)
		}
		wg.Wait()
		close(ch)
	}()

	return ch
}

// ValidateOne 验证单个代理是否可用，返回是否有效、延迟、出口IP和地理位置
func (v *Validator) ValidateOne(p storage.Proxy) (bool, time.Duration, string, string) {
	var client *http.Client
	var err error

	switch p.Protocol {
	case "http":
		client, err = newHTTPClient(p.Address, v.timeout)
	case "socks5":
		client, err = newSOCKS5Client(p.Address, v.timeout)
	default:
		log.Printf("unknown protocol %s for %s", p.Protocol, p.Address)
		return false, 0, "", ""
	}

	if err != nil {
		return false, 0, "", ""
	}

	start := time.Now()
	resp, err := client.Get(v.validateURL)
	latency := time.Since(start)
	if err != nil {
		return false, 0, "", ""
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	// 验证状态码（200 或 204 都接受）
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return false, latency, "", ""
	}

	// 响应时间过滤
	if v.maxResponseMs > 0 && latency > time.Duration(v.maxResponseMs)*time.Millisecond {
		return false, latency, "", ""
	}

	// 获取出口 IP 和地理位置（仅在验证通过时）
	exitIP, exitLocation := getExitIPInfo(client)

	// 是否强制要求出口信息：默认要求；在出口查询被限流/目标站不可达时可关闭，
	// 让代理以 UNKNOWN 地理位置入池（地理过滤随之跳过），避免池子长期为空。
	requireExit := true
	if v.cfg != nil {
		requireExit = v.cfg.RequireExitInfo
	}
	if requireExit && (exitIP == "" || exitLocation == "") {
		return false, latency, exitIP, exitLocation
	}

	// 地理过滤：白名单优先，否则走黑名单（仅在拿到位置信息时生效）
	if v.cfg != nil && len(exitLocation) >= 2 {
		countryCode := exitLocation[:2]
		if len(v.cfg.AllowedCountries) > 0 {
			// 白名单模式：不在白名单中则拒绝
			allowed := false
			for _, a := range v.cfg.AllowedCountries {
				if countryCode == a {
					allowed = true
					break
				}
			}
			if !allowed {
				return false, latency, exitIP, exitLocation
			}
		} else if len(v.cfg.BlockedCountries) > 0 {
			// 黑名单模式
			for _, blocked := range v.cfg.BlockedCountries {
				if countryCode == blocked {
					return false, latency, exitIP, exitLocation
				}
			}
		}
	}

	// HTTP 代理额外检测：必须支持 HTTPS CONNECT 隧道
	if p.Protocol == "http" {
		if !checkHTTPSConnect(p.Address, v.timeout) {
			return false, latency, exitIP, exitLocation
		}
	}

	return true, latency, exitIP, exitLocation
}

// LookupExitInfo 只为"已入池/候选入池"的代理查询出口信息（受全局限速保护）。
//
// 两段式验证的第二步：第一段（ValidateOne）只做连通性 + 延迟 + CONNECT 隧道，
// 高并发跑完全部候选；第二段只对少量幸存者查出口 IP/位置，
// 避免把 45 次/分钟的免费额度浪费在几万个死代理上。
func (v *Validator) LookupExitInfo(p storage.Proxy) (string, string, error) {
	var client *http.Client
	var err error
	switch p.Protocol {
	case "http":
		client, err = newHTTPClient(p.Address, v.timeout)
	case "socks5":
		client, err = newSOCKS5Client(p.Address, v.timeout)
	default:
		return "", "", fmt.Errorf("unknown protocol %s", p.Protocol)
	}
	if err != nil {
		return "", "", err
	}
	ip, loc := getExitIPInfo(client)
	if ip == "" {
		return "", "", fmt.Errorf("未取到出口信息")
	}
	return ip, loc, nil
}

// IsGeoBlocked 判断出口位置是否命中地理过滤（白名单优先，其次黑名单）。
func (v *Validator) IsGeoBlocked(exitLocation string) bool {
	if v.cfg == nil || len(exitLocation) < 2 {
		return false
	}
	cc := exitLocation[:2]
	if len(v.cfg.AllowedCountries) > 0 {
		for _, a := range v.cfg.AllowedCountries {
			if cc == a {
				return false
			}
		}
		return true
	}
	for _, b := range v.cfg.BlockedCountries {
		if cc == b {
			return true
		}
	}
	return false
}

func newHTTPClient(address string, timeout time.Duration) (*http.Client, error) {
	proxyURL, err := url.Parse(fmt.Sprintf("http://%s", address))
	if err != nil {
		return nil, err
	}
	return &http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyURL(proxyURL),
		},
		Timeout: timeout,
	}, nil
}

func newSOCKS5Client(address string, timeout time.Duration) (*http.Client, error) {
	dialer, err := proxy.SOCKS5("tcp", address, nil, proxy.Direct)
	if err != nil {
		return nil, err
	}
	return &http.Client{
		Transport: &http.Transport{
			Dial: dialer.Dial,
		},
		Timeout: timeout,
	}, nil
}
