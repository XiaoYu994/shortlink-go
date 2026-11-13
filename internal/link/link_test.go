package link

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestNewLink(t *testing.T) {
	tests := []struct {
		name    string
		rawURL  string
		wantURL string
		wantErr string // ValidationError.Reason 的子串；空表示应当成功
	}{
		{"http", "http://example.com", "http://example.com", ""},
		{"https with path", "https://example.com/a/b?q=1#frag", "https://example.com/a/b?q=1#frag", ""},
		{"trim spaces", "  https://example.com  ", "https://example.com", ""},
		{"uppercase scheme", "HTTPS://example.com", "HTTPS://example.com", ""},
		{"empty", "", "", "must not be empty"},
		{"spaces only", "   ", "", "must not be empty"},
		{"relative", "/path", "", "only http and https"},
		{"missing scheme", "example.com", "", "only http and https"},
		{"javascript scheme", "javascript:alert(1)", "", "only http and https"},
		{"file scheme", "file:///etc/passwd", "", "only http and https"},
		{"missing host", "https://", "", "missing host"},
		{"empty host with port", "http://:8080", "", "missing host"},
		{"bad escape", "https://example.com/%zz", "", "invalid URL"},
		{"space in host", "http://exa mple.com", "", "must not contain whitespace"},
		{"space in path", "https://example.com/a b", "", "must not contain whitespace"},
		{"space in query", "https://example.com/?q=a b", "", "must not contain whitespace"},
		{"tab in path", "https://example.com/a\tb", "", "must not contain whitespace"},
		{"too long", "https://example.com/" + strings.Repeat("a", MaxURLLength), "", "longer than"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NewLink(tt.rawURL, nil)
			if tt.wantErr != "" {
				var verr *ValidationError
				if !errors.As(err, &verr) {
					t.Fatalf("NewLink(%q) error = %v, want *ValidationError", tt.rawURL, err)
				}
				if verr.Field != "url" {
					t.Errorf("ValidationError.Field = %q, want %q", verr.Field, "url")
				}
				if !strings.Contains(verr.Reason, tt.wantErr) {
					t.Errorf("ValidationError.Reason = %q, want substring %q", verr.Reason, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("NewLink(%q) returned error: %v", tt.rawURL, err)
			}
			if got.URL != tt.wantURL {
				t.Errorf("NewLink(%q).URL = %q, want %q", tt.rawURL, got.URL, tt.wantURL)
			}
			if got.ExpiresAt != nil {
				t.Errorf("NewLink(%q).ExpiresAt = %v, want nil", tt.rawURL, got.ExpiresAt)
			}
		})
	}
}

func TestNewLinkExpiresAt(t *testing.T) {
	past := time.Now().Add(-time.Second)
	_, err := NewLink("https://example.com", &past)
	var verr *ValidationError
	if !errors.As(err, &verr) {
		t.Fatalf("NewLink past error = %v, want *ValidationError", err)
	}
	if verr.Field != "expires_at" {
		t.Errorf("ValidationError.Field = %q, want %q", verr.Field, "expires_at")
	}
	if !strings.Contains(verr.Reason, "must be in the future") {
		t.Errorf("ValidationError.Reason = %q, want substring %q", verr.Reason, "must be in the future")
	}

	future := time.Now().Add(time.Hour)
	got, err := NewLink("https://example.com", &future)
	if err != nil {
		t.Fatalf("NewLink future: %v", err)
	}
	if got.ExpiresAt == nil || !got.ExpiresAt.Equal(future.UTC()) {
		t.Fatalf("ExpiresAt = %v, want %v", got.ExpiresAt, future.UTC())
	}
	future = future.Add(time.Hour)
	if !got.ExpiresAt.Equal(future.Add(-time.Hour).UTC()) {
		t.Error("ExpiresAt follows the caller's time.Time")
	}
}

func TestLinkExpired(t *testing.T) {
	now := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)
	if (&Link{}).Expired(now) {
		t.Error("nil ExpiresAt is expired")
	}
	future := now.Add(time.Second)
	if (&Link{ExpiresAt: &future}).Expired(now) {
		t.Error("future ExpiresAt is expired")
	}
	if !(&Link{ExpiresAt: &now}).Expired(now) {
		t.Error("ExpiresAt equal to now is still valid")
	}
	past := now.Add(-time.Second)
	if !(&Link{ExpiresAt: &past}).Expired(now) {
		t.Error("past ExpiresAt is still valid")
	}
}
