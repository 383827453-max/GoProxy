package checker

import "testing"

func TestFailureOutcome(t *testing.T) {
	// 阈值 5：1..4 次降级保留，第 5 次才移除
	for fail := 1; fail < 5; fail++ {
		status, remove := failureOutcome(fail, 5)
		if remove || status != "degraded" {
			t.Fatalf("fail=%d 应降级保留，got status=%q remove=%v", fail, status, remove)
		}
	}
	status, remove := failureOutcome(5, 5)
	if !remove || status != "" {
		t.Fatalf("fail=5 应移除，got status=%q remove=%v", status, remove)
	}
	if _, remove := failureOutcome(9, 5); !remove {
		t.Fatal("超过阈值应移除")
	}

	// 阈值 <=0 时回退默认 5，而不是"一次失败就删"
	if _, remove := failureOutcome(1, 0); remove {
		t.Fatal("阈值非法时应回退默认值，不应移除")
	}
	if _, remove := failureOutcome(5, -3); !remove {
		t.Fatal("阈值非法回退为 5 后，第 5 次应移除")
	}
}
