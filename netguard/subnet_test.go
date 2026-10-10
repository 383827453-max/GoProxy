package netguard

import "testing"

func TestSubnet24(t *testing.T) {
	cases := []struct {
		ip   string
		want string
	}{
		{"52.128.241.107", "52.128.241."},
		{" 8.8.8.8 ", "8.8.8."},
		{"2606:4700:4700::1111", ""}, // IPv6 不支持 /24
		{"", ""},
		{"not-an-ip", ""},
	}
	for _, c := range cases {
		if got := Subnet24(c.ip); got != c.want {
			t.Fatalf("Subnet24(%q) = %q, want %q", c.ip, got, c.want)
		}
	}
}

func TestSubnet24Filter(t *testing.T) {
	cases := []struct{ in, want string }{
		{"52.128.241.", "52.128.241.%"},
		{"52.128.241.%", "52.128.241.%"},
		{"", ""},
	}
	for _, c := range cases {
		if got := Subnet24Filter(c.in); got != c.want {
			t.Fatalf("Subnet24Filter(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
