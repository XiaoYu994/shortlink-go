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
