package idgen

import (
	"errors"
	"math"
	"testing"
)

func TestEncodeBase62(t *testing.T) {
	tests := []struct {
		name string
		n    int64
		want string
	}{
		{"zero", 0, "0"},
		{"digit end", 9, "9"},
		{"uppercase start", 10, "A"},
		{"uppercase end", 35, "Z"},
		{"lowercase start", 36, "a"},
		{"last character", 61, "z"},
		{"carry", 62, "10"},
		{"two digits max", 3843, "zz"},
		{"three digits min", 3844, "100"},
		{"max int64", math.MaxInt64, "AzL8n0Y58m7"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := EncodeBase62(tt.n); got != tt.want {
				t.Errorf("EncodeBase62(%d) = %q, want %q", tt.n, got, tt.want)
			}
		})
	}
}

func TestEncodeBase62NegativePanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("EncodeBase62(-1) did not panic")
		}
	}()
	EncodeBase62(-1)
}

func TestDecodeBase62(t *testing.T) {
	tests := []struct {
		name string
		code string
		want int64
	}{
		{"zero", "0", 0},
		{"leading zero", "00", 0},
		{"last character", "z", 61},
		{"carry", "10", 62},
		{"two digits max", "zz", 3843},
		{"three digits min", "100", 3844},
		{"max int64", "AzL8n0Y58m7", math.MaxInt64},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := DecodeBase62(tt.code)
			if err != nil {
				t.Fatalf("DecodeBase62(%q) returned error: %v", tt.code, err)
			}
			if got != tt.want {
				t.Errorf("DecodeBase62(%q) = %d, want %d", tt.code, got, tt.want)
			}
		})
	}
}

func TestDecodeBase62Errors(t *testing.T) {
	tests := []struct {
		name string
		code string
		want error
	}{
		{"empty", "", ErrInvalidCode},
		{"invalid character", "!", ErrInvalidCode},
		{"space", "10 0", ErrInvalidCode},
		{"negative sign", "-1", ErrInvalidCode},
		{"non ascii", "é", ErrInvalidCode},
		{"max int64 plus one", "AzL8n0Y58m8", ErrOverflow},
		{"eleven z", "zzzzzzzzzzz", ErrOverflow},
		{"twelve digits", "100000000000", ErrOverflow},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := DecodeBase62(tt.code)
			if !errors.Is(err, tt.want) {
				t.Errorf("DecodeBase62(%q) error = %v, want %v", tt.code, err, tt.want)
			}
		})
	}
}

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
			if decodeTable[code[j]] < 0 {
				t.Fatalf("code %q contains invalid character %q", code, code[j])
			}
		}
		if _, ok := seen[code]; ok {
			t.Fatalf("duplicate code %q", code)
		}
		seen[code] = struct{}{}
	}
}

func TestBase62RoundTrip(t *testing.T) {
	values := []int64{0, 1, 9, 10, 35, 36, 61, 62, 383, 3843, 3844, 238327, 238328, math.MaxInt64}
	for _, n := range values {
		code := EncodeBase62(n)
		got, err := DecodeBase62(code)
		if err != nil {
			t.Fatalf("DecodeBase62(EncodeBase62(%d)) returned error: %v", n, err)
		}
		if got != n {
			t.Errorf("round trip %d -> %q -> %d", n, code, got)
		}
	}
}
