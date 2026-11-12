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
