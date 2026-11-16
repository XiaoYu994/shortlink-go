package idgen

import (
	"strings"
	"testing"
)

func TestGenerateCode(t *testing.T) {
	const n = 1000
	seen := make(map[string]struct{}, n)
	for range n {
		code, err := GenerateCode()
		if err != nil {
			t.Fatalf("GenerateCode: %v", err)
		}
		if len(code) != CodeLength {
			t.Fatalf("len(code) = %d, want %d", len(code), CodeLength)
		}
		for j := range code {
			if strings.IndexByte(alphabet, code[j]) < 0 {
				t.Fatalf("code %q contains invalid character %q", code, code[j])
			}
		}
		if _, ok := seen[code]; ok {
			t.Fatalf("duplicate code %q", code)
		}
		seen[code] = struct{}{}
	}
}

func TestValidCode(t *testing.T) {
	tests := []struct {
		name string
		code string
		want bool
	}{
		{"valid", "AbC123xy", true},
		{"digits only", "12345678", true},
		{"too short", "AbC123x", false},
		{"too long", "AbC123xyZ", false},
		{"invalid character", "AbC123-y", false},
		{"empty", "", false},
		{"space", "AbC 23xy", false},
		{"non ascii", "AbC123é", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ValidCode(tt.code); got != tt.want {
				t.Errorf("ValidCode(%q) = %v, want %v", tt.code, got, tt.want)
			}
		})
	}
}

/*
FuzzValidCode 锁住短码格式：长度必须是 CodeLength，并且每个字节都在字母表里。
go test 只跑种子。持续生成输入用 go test -fuzz=FuzzValidCode。
*/
func FuzzValidCode(f *testing.F) {
	f.Add("AbC123xy")
	f.Add("12345678")
	f.Add("")
	f.Add("AbC123x")
	f.Add("AbC123xyZ")
	f.Add("AbC123-y")
	f.Add("AbC 23xy")
	f.Add("AbC123é")
	f.Fuzz(func(t *testing.T, code string) {
		got := ValidCode(code)
		want := len(code) == CodeLength
		if want {
			for i := range len(code) {
				if strings.IndexByte(alphabet, code[i]) < 0 {
					want = false
					break
				}
			}
		}
		if got != want {
			t.Fatalf("ValidCode(%q) = %v, want %v", code, got, want)
		}
	})
}

func BenchmarkValidCode(b *testing.B) {
	const code = "AbC123xy"
	for range b.N {
		ValidCode(code)
	}
}

func BenchmarkGenerateCode(b *testing.B) {
	for range b.N {
		code, err := GenerateCode()
		if err != nil {
			b.Fatal(err)
		}
		if len(code) != CodeLength {
			b.Fatalf("len = %d", len(code))
		}
	}
}
