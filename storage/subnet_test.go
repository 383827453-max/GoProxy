package storage

import (
	"strings"
	"testing"
)

func TestBuildSubnetFilter(t *testing.T) {
	// 空/仅空白 → 不生成任何片段
	sql, args := buildSubnetFilter(nil)
	if sql != "" || len(args) != 0 {
		t.Fatalf("nil 输入应返回空，got sql=%q args=%v", sql, args)
	}
	sql, args = buildSubnetFilter([]string{"  ", ""})
	if sql != "" || len(args) != 0 {
		t.Fatalf("空白项应被忽略，got sql=%q args=%v", sql, args)
	}

	// 正常项：每个网段一个 NOT LIKE，参数带 %
	sql, args = buildSubnetFilter([]string{"52.128.241.", "8.8.8."})
	if !strings.Contains(sql, "NOT LIKE ?") {
		t.Fatalf("应包含 NOT LIKE，got %q", sql)
	}
	if strings.Count(sql, "NOT LIKE ?") != 2 {
		t.Fatalf("应有 2 个 NOT LIKE，got %q", sql)
	}
	if len(args) != 2 || args[0] != "52.128.241.%" || args[1] != "8.8.8.%" {
		t.Fatalf("参数不正确: %v", args)
	}

	// 已带 % 的前缀不重复追加
	_, args = buildSubnetFilter([]string{"1.2.3.%"})
	if args[0] != "1.2.3.%" {
		t.Fatalf("重复百分号: %v", args[0])
	}
}

func TestPlaceholders(t *testing.T) {
	if placeholders(0) != "" || placeholders(1) != "?" || placeholders(3) != "?,?,?" {
		t.Fatalf("placeholders 生成异常")
	}
}
