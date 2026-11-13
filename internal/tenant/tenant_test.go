package tenant

import (
	"bytes"
	"strings"
	"testing"
)

func TestValidToken(t *testing.T) {
	valid := "slk_" + strings.Repeat("ab", 32)
	tests := []struct {
		name  string
		token string
		want  bool
	}{
		{"ok", valid, true},
		{"empty", "", false},
		{"missing prefix", strings.Repeat("ab", 32), false},
		{"uppercase", "slk_" + strings.Repeat("AB", 32), false},
		{"short", "slk_abcd", false},
		{"bearer left on", "Bearer " + valid, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ValidToken(tt.token); got != tt.want {
				t.Errorf("ValidToken(%q) = %v, want %v", tt.token, got, tt.want)
			}
		})
	}
}

func TestGenerateAPIKey(t *testing.T) {
	plaintext, hash, err := GenerateAPIKey()
	if err != nil {
		t.Fatalf("GenerateAPIKey: %v", err)
	}
	if !ValidToken(plaintext) {
		t.Fatalf("plaintext %q is not a valid token", plaintext)
	}
	if !bytes.Equal(hash, HashAPIKey(plaintext)) {
		t.Fatal("hash does not match HashAPIKey(plaintext)")
	}
	again, _, err := GenerateAPIKey()
	if err != nil {
		t.Fatalf("GenerateAPIKey again: %v", err)
	}
	if again == plaintext {
		t.Fatal("two generated keys are identical")
	}
}
