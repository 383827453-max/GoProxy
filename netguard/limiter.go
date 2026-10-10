package netguard

import (
	"context"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

var (
	limiterMu     sync.RWMutex
	exitIPLimiter *rate.Limiter
)

// InitExitIPLimiter 设置出口 IP / 地理位置查询的全局速率（rps；<=0 表示不限速）。
//
// 背景：免费额度类服务（ip-api.com 免费档 45 次/分钟）在验证并发 300 时会被瞬间打爆，
// 返回 503 Too many open connections，导致所有代理都因"拿不到出口信息"被拒。
// 因此验证链路必须共用同一个全局令牌桶。
func InitExitIPLimiter(rps float64, burst int) {
	limiterMu.Lock()
	defer limiterMu.Unlock()
	if rps <= 0 {
		exitIPLimiter = nil
		return
	}
	if burst <= 0 {
		burst = 1
	}
	exitIPLimiter = rate.NewLimiter(rate.Limit(rps), burst)
}

// WaitExitIP 在出口 IP 查询前取令牌。
// 返回 false 表示在 timeout 内没拿到令牌：调用方应放弃本次查询（快速失败），而不是无限等待。
func WaitExitIP(timeout time.Duration) bool {
	limiterMu.RLock()
	l := exitIPLimiter
	limiterMu.RUnlock()
	if l == nil {
		return true
	}
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return l.Wait(ctx) == nil
}
