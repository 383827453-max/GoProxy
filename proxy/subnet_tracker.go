package proxy

import (
	"sync"
	"time"

	"goproxy/netguard"
)

// subnetTracker 记录"最近用过的出口 /24"，用于出口去重与同 C 段限制。
//
// 背景（吸收 yunginnanet/prox5 的 SetDispenseMiddleware 与 slrp 的排序策略）：
// 免费代理池里同一机房往往成片出现（例如 52.128.241.107/108/109 同 /24），
// 不做限制时连续请求会一直从同一个 C 段出去，出口多样性形同虚设。
// 这里在"选代理"这一层做冷却：冷却窗口内命中过的 /24 优先避开；
// 若池内所有代理都落在冷却网段，则回退到不限制（可用性优先于多样性）。
type subnetTracker struct {
	mu       sync.Mutex
	cooldown time.Duration
	size     int
	recent   []subnetEntry
}

type subnetEntry struct {
	prefix string
	at     time.Time
}

func newSubnetTracker(cooldown time.Duration, size int) *subnetTracker {
	if size <= 0 {
		size = 16
	}
	return &subnetTracker{cooldown: cooldown, size: size}
}

// Active 返回仍在冷却窗口内的 /24 前缀列表（供 SQL 排除）。
func (t *subnetTracker) Active() []string {
	if t == nil || t.cooldown <= 0 {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	out := make([]string, 0, len(t.recent))
	kept := t.recent[:0]
	for _, e := range t.recent {
		if now.Sub(e.at) < t.cooldown {
			out = append(out, e.prefix)
			kept = append(kept, e)
		}
	}
	t.recent = kept
	return out
}

// Record 记录本次使用的代理出口 /24。
func (t *subnetTracker) Record(exitIP string) {
	if t == nil || t.cooldown <= 0 {
		return
	}
	prefix := netguard.Subnet24(exitIP)
	if prefix == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	// 去重后追加，超出容量时丢最旧的
	kept := t.recent[:0]
	for _, e := range t.recent {
		if e.prefix != prefix {
			kept = append(kept, e)
		}
	}
	kept = append(kept, subnetEntry{prefix: prefix, at: time.Now()})
	if len(kept) > t.size {
		kept = kept[len(kept)-t.size:]
	}
	t.recent = kept
}
