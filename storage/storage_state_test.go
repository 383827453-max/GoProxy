package storage

import (
	"path/filepath"
	"testing"
)

// newTestStorage 建一个临时库（CGO sqlite3），用于验证状态语义与计数口径。
func newTestStorage(t *testing.T) *Storage {
	t.Helper()
	dir := t.TempDir()
	s, err := New(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("初始化存储失败: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestCountSemanticsExcludePendingAndDisabled(t *testing.T) {
	s := newTestStorage(t)

	// 4 条免费代理：2 active、1 degraded、1 pending；1 条 disabled；1 条订阅 active
	mustAdd := func(addr, proto, source, status string) {
		t.Helper()
		if err := s.AddProxyWithSource(addr, proto, source); err != nil {
			t.Fatalf("加代理失败 %s: %v", addr, err)
		}
		if err := s.SetStatus(addr, status); err != nil {
			t.Fatalf("置状态失败 %s: %v", addr, err)
		}
	}
	mustAdd("1.1.1.1:1080", "socks5", "free", "active")
	mustAdd("2.2.2.2:1080", "socks5", "free", "active")
	mustAdd("3.3.3.3:8080", "http", "free", "degraded")
	mustAdd("4.4.4.4:8080", "http", "free", "pending")
	mustAdd("5.5.5.5:1080", "socks5", "free", "disabled")
	mustAdd("6.6.6.6:1080", "socks5", "custom", "active")

	// 槽位口径：只数免费 + active/degraded
	total, err := s.Count()
	if err != nil {
		t.Fatal(err)
	}
	if total != 3 {
		t.Fatalf("Count() 应为 3（pending/disabled/custom 不计入），实际 %d", total)
	}
	httpCount, _ := s.CountByProtocol("http")
	if httpCount != 1 {
		t.Fatalf("http 计数应为 1（degraded 计入、pending 不计入），实际 %d", httpCount)
	}
	socks5Count, _ := s.CountByProtocol("socks5")
	if socks5Count != 2 {
		t.Fatalf("socks5 计数应为 2（disabled 不计入），实际 %d", socks5Count)
	}

	// pending 单独可数（入池节流要用它）
	pending, err := s.CountPending()
	if err != nil {
		t.Fatal(err)
	}
	if pending != 1 {
		t.Fatalf("CountPending() 应为 1，实际 %d", pending)
	}

	// 状态分布应覆盖全部 6 条
	dist, err := s.CountByStatus()
	if err != nil {
		t.Fatal(err)
	}
	if dist["active"] != 3 || dist["degraded"] != 1 || dist["pending"] != 1 || dist["disabled"] != 1 {
		t.Fatalf("状态分布异常: %v", dist)
	}
}

func TestPendingNotSelectable(t *testing.T) {
	s := newTestStorage(t)
	if err := s.AddProxyWithSource("9.9.9.9:1080", "socks5", "free"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetStatus("9.9.9.9:1080", "pending"); err != nil {
		t.Fatal(err)
	}
	// pending 不应被任何选择路径取到（否则未验证代理会被转发）
	if p, err := s.GetRandomByProtocolExcludeSubnets("socks5", nil, nil, ""); err == nil && p != nil {
		t.Fatalf("pending 代理不应可被选择: %+v", p)
	}
	// 转 active 后可被取到
	if err := s.SetStatus("9.9.9.9:1080", "active"); err != nil {
		t.Fatal(err)
	}
	p, err := s.GetRandomByProtocolExcludeSubnets("socks5", nil, nil, "")
	if err != nil || p == nil || p.Address != "9.9.9.9:1080" {
		t.Fatalf("active 代理应可被选择: p=%+v err=%v", p, err)
	}
}

func TestSubnetExclusionInSelection(t *testing.T) {
	s := newTestStorage(t)
	// 三条同 /24 + 一条不同 /24，全部 active 且带出口 IP
	for _, row := range []struct{ addr, exit string }{
		{"1.2.3.10:1080", "52.128.241.10"},
		{"1.2.3.11:1080", "52.128.241.11"},
		{"1.2.3.12:1080", "52.128.241.12"},
		{"1.2.3.13:1080", "8.8.8.8"},
	} {
		if err := s.AddProxyWithSource(row.addr, "socks5", "free"); err != nil {
			t.Fatal(err)
		}
		if err := s.UpdateExitInfo(row.addr, row.exit, "US Test", 100); err != nil {
			t.Fatal(err)
		}
		if err := s.SetStatus(row.addr, "active"); err != nil {
			t.Fatal(err)
		}
	}

	// 排除 52.128.241. 后只能取到 8.8.8.8 那条
	for i := 0; i < 5; i++ {
		p, err := s.GetRandomByProtocolExcludeSubnets("socks5", nil, []string{"52.128.241."}, "")
		if err != nil {
			t.Fatalf("选择失败: %v", err)
		}
		if p.Address != "1.2.3.13:1080" {
			t.Fatalf("应排除同 C 段，实际取到 %s", p.Address)
		}
	}

	// 不排除时 4 条都可能在结果里（至少能取到）
	if _, err := s.GetRandomByProtocolExcludeSubnets("socks5", nil, nil, ""); err != nil {
		t.Fatalf("不排除时应能取到: %v", err)
	}
}

func TestHealthCheckNotBlindSpotWhenAllGradeS(t *testing.T) {
	s := newTestStorage(t)
	// 造 3 条 S 级（低延迟）active 代理 —— 这正是线上实测出现的"整池都是 S 级"场景
	for _, a := range []string{"1.1.1.1:1080", "2.2.2.2:1080", "3.3.3.3:1080"} {
		if err := s.AddProxyWithSource(a, "socks5", "free"); err != nil {
			t.Fatal(err)
		}
		if err := s.UpdateExitInfo(a, a[:len(a)-5], "US Test", 100); err != nil {
			t.Fatal(err)
		}
		if err := s.SetStatus(a, "active"); err != nil {
			t.Fatal(err)
		}
	}

	// 全部为 S 级时，"跳过 S 级"的候选数为 0 → 调用方据此判断不应跳过
	nonS, err := s.CountCheckable(true, 5)
	if err != nil {
		t.Fatal(err)
	}
	if nonS != 0 {
		t.Fatalf("全部 S 级时非 S 待检数应为 0，实际 %d", nonS)
	}
	// 不跳过时必须能取到批次，否则健康检查会整轮空转
	batch, err := s.GetBatchForHealthCheck(20, false, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(batch) != 3 {
		t.Fatalf("不跳过 S 级时应取到 3 条，实际 %d", len(batch))
	}
}

func TestHealthCheckCoversRowsAboveSelectionThreshold(t *testing.T) {
	s := newTestStorage(t)
	addr := "9.9.9.9:1080"
	if err := s.AddProxyWithSource(addr, "socks5", "free"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetStatus(addr, "degraded"); err != nil {
		t.Fatal(err)
	}
	// fail_count = 4：已被"选择"排除（<3），但还没到失败阈值 5
	for i := 0; i < 4; i++ {
		if err := s.IncrementFailCount(addr); err != nil {
			t.Fatal(err)
		}
	}
	batch, err := s.GetBatchForHealthCheck(20, false, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(batch) != 1 || batch[0].Address != addr {
		t.Fatalf("fail_count=4 的行必须仍被健康检查覆盖（否则成为僵尸行），实际 %+v", batch)
	}

	// 达到阈值后被兜底清扫删除
	if err := s.IncrementFailCount(addr); err != nil {
		t.Fatal(err)
	}
	n, err := s.DeleteInvalid(5)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("达阈值应被清扫，实际清理 %d", n)
	}
	if total, _ := s.CountAll(); total != 0 {
		t.Fatalf("清扫后不应残留，实际 %d", total)
	}
}

func TestDeleteStalePending(t *testing.T) {
	s := newTestStorage(t)
	if err := s.AddProxyWithSource("7.7.7.7:1080", "socks5", "free"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetStatus("7.7.7.7:1080", "pending"); err != nil {
		t.Fatal(err)
	}
	// created_at 是 NOW，用 0 分钟阈值（回退为 60）不会删；直接用极大阈值模拟"很久没验证"
	deleted, err := s.DeleteStalePending(60)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 0 {
		t.Fatalf("刚创建的 pending 不应被清理，deleted=%d", deleted)
	}
	// 手工把创建时间推到 2 小时前
	if _, err := s.db.Exec(`UPDATE proxies SET created_at = datetime('now', '-120 minutes') WHERE address = '7.7.7.7:1080'`); err != nil {
		t.Fatal(err)
	}
	deleted, err = s.DeleteStalePending(60)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 1 {
		t.Fatalf("超时 pending 应被清理，deleted=%d", deleted)
	}
}
