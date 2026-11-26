package identity

import (
	"errors"
	"testing"
	"time"
)

func TestNormalizePhone(t *testing.T) {
	valid := []struct {
		in   string
		want string
	}{
		{"+8613800138000", "+8613800138000"},
		{"  +14155551234\t", "+14155551234"},
		{"+1234567", "+1234567"},
		{"+123456789012345", "+123456789012345"},
	}
	for _, tc := range valid {
		got, err := NormalizePhone(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("NormalizePhone(%q) = (%q, %v), want (%q, nil)", tc.in, got, err, tc.want)
		}
	}

	invalid := []string{
		"",
		"   ",
		"13800138000",
		"+01234567",
		"+123456",
		"+1234567890123456",
		"+86-13800138000",
		"+86 13800138000",
		"+861380013800a",
		"++8613800138000",
	}
	for _, raw := range invalid {
		if got, err := NormalizePhone(raw); !errors.Is(err, ErrInvalidPhone) || got != "" {
			t.Errorf("NormalizePhone(%q) = (%q, %v), want ErrInvalidPhone", raw, got, err)
		}
	}
}

func TestMaskPhone(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"", "****"},
		{"+1234", "****"},
		{"+1234567", "+1****67"},
		{"+8613800138000", "+86****8000"},
		{"+14155551234", "+14****1234"},
	}
	for _, tc := range tests {
		if got := MaskPhone(tc.in); got != tc.want {
			t.Errorf("MaskPhone(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestOTPCodeGenerationAndHashVerification(t *testing.T) {
	code, err := NewOTPCode()
	if err != nil {
		t.Fatalf("NewOTPCode: %v", err)
	}
	if !ValidOTPCode(code) {
		t.Fatalf("generated code %q is not valid 6-digit OTP", code)
	}
	for _, bad := range []string{"", "12345", "1234567", "12345a", " 12345"} {
		if ValidOTPCode(bad) {
			t.Errorf("ValidOTPCode(%q) = true, want false", bad)
		}
	}

	salt, err := NewOTPSalt()
	if err != nil {
		t.Fatalf("NewOTPSalt: %v", err)
	}
	phone := "+8613800138000"
	hash := HashOTPCode(salt, phone, code)
	if !VerifyOTPCodeHash(salt, hash, phone, code) {
		t.Fatal("VerifyOTPCodeHash rejected matching phone and code")
	}
	if VerifyOTPCodeHash(salt, hash, "+8613800138001", code) {
		t.Fatal("VerifyOTPCodeHash accepted mismatched phone")
	}
	otherCode := "000000"
	if code == otherCode {
		otherCode = "999999"
	}
	if VerifyOTPCodeHash(salt, hash, phone, otherCode) {
		t.Fatal("VerifyOTPCodeHash accepted mismatched code")
	}
	if VerifyOTPCodeHash(salt[:8], hash, phone, code) {
		t.Fatal("VerifyOTPCodeHash accepted short salt")
	}
}

func TestOTPRetryAfter(t *testing.T) {
	err := OTPRetryAfterError{Err: ErrOTPCooldown, RetryAfter: 42 * time.Second}
	if !errors.Is(err, ErrOTPCooldown) {
		t.Fatalf("errors.Is(err, ErrOTPCooldown) = false")
	}
	d, ok := OTPRetryAfter(err)
	if !ok || d != 42*time.Second {
		t.Fatalf("OTPRetryAfter = (%v, %v), want (42s, true)", d, ok)
	}
	shortErr := OTPRetryAfterError{Err: ErrOTPRateLimited, RetryAfter: 100 * time.Millisecond}
	if d, ok := OTPRetryAfter(shortErr); !ok || d != time.Second {
		t.Fatalf("OTPRetryAfter(short) = (%v, %v), want (1s, true)", d, ok)
	}
	if _, ok := OTPRetryAfter(ErrOTPInvalid); ok {
		t.Fatal("OTPRetryAfter(ErrOTPInvalid) = true, want false")
	}
}
