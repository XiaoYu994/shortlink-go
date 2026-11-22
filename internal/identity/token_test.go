package identity

import (
	"bytes"
	"strings"
	"testing"
)

func TestTokenFormatValid(t *testing.T) {
	hex64 := strings.Repeat("ab", 32)
	tests := []struct {
		name   string
		format TokenFormat
		token  string
		want   bool
	}{
		{name: "api key ok", format: APIKeyToken, token: "slk_" + hex64, want: true},
		{name: "api key empty", format: APIKeyToken, token: "", want: false},
		{name: "api key session prefix", format: APIKeyToken, token: "sls_" + hex64, want: false},
		{name: "api key missing prefix", format: APIKeyToken, token: hex64, want: false},
		{name: "api key uppercase", format: APIKeyToken, token: "slk_" + strings.Repeat("AB", 32), want: false},
		{name: "api key short", format: APIKeyToken, token: "slk_abcd", want: false},
		{name: "api key bearer left on", format: APIKeyToken, token: "Bearer slk_" + hex64, want: false},
		{name: "session ok", format: SessionToken, token: "sls_" + hex64, want: true},
		{name: "session empty", format: SessionToken, token: "", want: false},
		{name: "session api key prefix", format: SessionToken, token: "slk_" + hex64, want: false},
		{name: "session missing prefix", format: SessionToken, token: hex64, want: false},
		{name: "session uppercase", format: SessionToken, token: "sls_" + strings.Repeat("AB", 32), want: false},
		{name: "session short", format: SessionToken, token: "sls_abcd", want: false},
		{name: "zero format rejects bare hex", format: TokenFormat{}, token: hex64, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.format.Valid(tt.token); got != tt.want {
				t.Errorf("Valid(%q) = %v, want %v", tt.token, got, tt.want)
			}
		})
	}
}

func TestTokenFormatGenerate(t *testing.T) {
	tests := []struct {
		name   string
		format TokenFormat
		other  TokenFormat
	}{
		{name: "api key", format: APIKeyToken, other: SessionToken},
		{name: "session", format: SessionToken, other: APIKeyToken},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plaintext, hash, err := tt.format.Generate()
			if err != nil {
				t.Fatalf("Generate: %v", err)
			}
			if !tt.format.Valid(plaintext) {
				t.Fatalf("plaintext %q is not valid for format", plaintext)
			}
			if tt.other.Valid(plaintext) {
				t.Fatalf("plaintext %q unexpectedly matched other format", plaintext)
			}
			if len(hash) != 32 || !bytes.Equal(hash, HashToken(plaintext)) {
				t.Fatalf("hash length = %d, does not match HashToken(plaintext)", len(hash))
			}
			again, _, err := tt.format.Generate()
			if err != nil {
				t.Fatalf("Generate again: %v", err)
			}
			if again == plaintext {
				t.Fatal("two generated tokens are identical")
			}
		})
	}

	if _, _, err := (TokenFormat{}).Generate(); err == nil {
		t.Fatal("zero TokenFormat{}.Generate() = nil, want error")
	}
}
