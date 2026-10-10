package netguard

import (
	"testing"
	"time"
)

func TestGuardOutboundURL(t *testing.T) {
	reject := []string{
		"",
		"file:///etc/passwd",
		"gopher://127.0.0.1:6379/_INFO",
		"http://127.0.0.1:8080/config",
		"http://169.254.169.254/latest/meta-data/",
		"http://[::1]:8080/",
		"http://10.0.0.1/",
		"http://192.168.1.1/",
		"http://localhost/admin",
		"http://metadata.google.internal/computeMetadata/v1/",
		"http://user:pass@example.com/sub",
	}
	for _, u := range reject {
		if err := GuardOutboundURL(u, false); err == nil {
			t.Fatalf("%q 应被拒绝", u)
		}
	}

	// 公网字面量 IP 放行
	if err := GuardOutboundURL("https://1.1.1.1/sub?token=1", false); err != nil {
		t.Fatalf("公网 IP 应放行: %v", err)
	}
	// 显式放开内网时：环回/私网放行，但元数据与 file:// 仍必须拒绝
	if err := GuardOutboundURL("http://127.0.0.1:8080/sub", true); err != nil {
		t.Fatalf("allowPrivate 时应放行环回: %v", err)
	}
	if err := GuardOutboundURL("http://10.0.0.1/sub", true); err != nil {
		t.Fatalf("allowPrivate 时应放行私网: %v", err)
	}
	if err := GuardOutboundURL("http://169.254.169.254/latest/meta-data/", true); err == nil {
		t.Fatal("allowPrivate 也不应放行云元数据地址")
	}
	if err := GuardOutboundURL("file:///etc/passwd", true); err == nil {
		t.Fatal("allowPrivate 也不应放行 file://")
	}
}

func TestExitIPLimiter(t *testing.T) {
	// 未初始化：直接放行
	InitExitIPLimiter(0, 0)
	if !WaitExitIP(50 * time.Millisecond) {
		t.Fatal("未初始化限流器应直接放行")
	}

	// 1 rps + burst 1：第二次取令牌必须等超时（快速失败），验证限速确实生效
	InitExitIPLimiter(1, 1)
	if !WaitExitIP(200 * time.Millisecond) {
		t.Fatal("首次取令牌应成功")
	}
	if WaitExitIP(50 * time.Millisecond) {
		t.Fatal("burst 用尽后应取不到令牌（限速未生效）")
	}
	InitExitIPLimiter(0, 0)
}
