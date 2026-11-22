package httpx

import (
	"net/netip"
	"strings"
	"testing"
)

/*
FuzzRightmostForwardedIP 锁住访客 IP 的取值：只用最右一段非空字段。
这段不是合法 IP 时必须拒绝，不能改用它左边的地址。
go test 只跑种子。持续生成输入用 go test -fuzz=FuzzRightmostForwardedIP。
*/
func FuzzRightmostForwardedIP(f *testing.F) {
	f.Add("203.0.113.9, 10.0.0.1")
	f.Add("203.0.113.9")
	f.Add("203.0.113.9, not-an-ip")
	f.Add("203.0.113.9,")
	f.Add("")
	f.Add("::1")
	f.Add("::ffff:192.0.2.1")
	f.Fuzz(func(t *testing.T, header string) {
		got, ok := rightmostForwardedIP(header)
		field, has := rightmostNonEmpty(header)
		if !has {
			if ok || got != "" {
				t.Fatalf("empty header accepted %q", got)
			}
			return
		}
		ip, err := netip.ParseAddr(field)
		if err != nil {
			if ok || got != "" {
				t.Fatalf("invalid rightmost %q accepted %q from %q", field, got, header)
			}
			return
		}
		want := ip.Unmap().String()
		if !ok || got != want {
			t.Fatalf("header %q: got %q ok=%v, want %q", header, got, ok, want)
		}
	})
}

func BenchmarkRightmostForwardedIP(b *testing.B) {
	const header = "203.0.113.9, 10.0.0.1"
	for range b.N {
		if _, ok := rightmostForwardedIP(header); !ok {
			b.Fatal("want ok")
		}
	}
}

// rightmostNonEmpty 只找最右一段非空字段，不判断它是不是 IP。
// fuzz 用它当独立的预期，避免把实现抄一遍再和自己比。
func rightmostNonEmpty(header string) (string, bool) {
	parts := strings.Split(header, ",")
	for i := len(parts) - 1; i >= 0; i-- {
		field := strings.TrimSpace(parts[i])
		if field != "" {
			return field, true
		}
	}
	return "", false
}
