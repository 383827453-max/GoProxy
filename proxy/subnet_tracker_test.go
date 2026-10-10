package proxy

import (
	"testing"
	"time"
)

func TestSubnetTrackerRecordAndActive(t *testing.T) {
	tr := newSubnetTracker(time.Minute, 3)

	tr.Record("52.128.241.107")
	tr.Record("52.128.241.109") // 同 /24，去重后只保留一个
	tr.Record("8.8.8.8")
	active := tr.Active()
	if len(active) != 2 {
		t.Fatalf("同 /24 应去重，got %v", active)
	}
	if active[0] != "52.128.241." || active[1] != "8.8.8." {
		t.Fatalf("前缀异常: %v", active)
	}

	// 容量上限：超出后丢最旧的
	tr.Record("1.1.1.1")
	tr.Record("2.2.2.2")
	active = tr.Active()
	if len(active) != 3 {
		t.Fatalf("容量应为 3，got %v", active)
	}
	for _, p := range active {
		if p == "52.128.241." {
			t.Fatalf("最旧网段应被淘汰: %v", active)
		}
	}
}

func TestSubnetTrackerExpiry(t *testing.T) {
	tr := newSubnetTracker(50*time.Millisecond, 4)
	tr.Record("9.9.9.9")
	if len(tr.Active()) != 1 {
		t.Fatal("刚记录应处于冷却中")
	}
	time.Sleep(80 * time.Millisecond)
	if len(tr.Active()) != 0 {
		t.Fatalf("超过冷却窗口应过期，got %v", tr.Active())
	}
}

func TestSubnetTrackerDisabled(t *testing.T) {
	// cooldown=0 表示关闭该特性：不记录、不排除
	tr := newSubnetTracker(0, 4)
	tr.Record("1.2.3.4")
	if len(tr.Active()) != 0 {
		t.Fatal("cooldown=0 时不应产生冷却网段")
	}
	// 非法/空 IP 不应产生记录
	tr2 := newSubnetTracker(time.Minute, 4)
	tr2.Record("")
	tr2.Record("not-an-ip")
	if len(tr2.Active()) != 0 {
		t.Fatalf("非法 IP 不应记录: %v", tr2.Active())
	}
}
