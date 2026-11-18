package link

import (
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"
	"unicode"
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
	now := time.Now()
	future := time.Now().Add(time.Hour)
	tests := []struct {
		name    string
		at      time.Time
		wantErr string
	}{
		{"past", past, "must be in the future"},
		{"now", now, "must be in the future"},
		{"future", future, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			at := tt.at
			got, err := NewLink("https://example.com", &at)
			if tt.wantErr != "" {
				var verr *ValidationError
				if !errors.As(err, &verr) {
					t.Fatalf("NewLink error = %v, want *ValidationError", err)
				}
				if verr.Field != "expires_at" {
					t.Errorf("Field = %q, want %q", verr.Field, "expires_at")
				}
				if !strings.Contains(verr.Reason, tt.wantErr) {
					t.Errorf("Reason = %q, want substring %q", verr.Reason, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("NewLink: %v", err)
			}
			if got.ExpiresAt == nil || !got.ExpiresAt.Equal(at.UTC()) {
				t.Fatalf("ExpiresAt = %v, want %v", got.ExpiresAt, at.UTC())
			}
			// 调用方改自己的变量，存进去的副本应保持原值。
			at = at.Add(time.Hour)
			if !got.ExpiresAt.Equal(at.Add(-time.Hour).UTC()) {
				t.Error("stored ExpiresAt changed with the caller's time.Time")
			}
		})
	}
}

func TestLinkExpired(t *testing.T) {
	now := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)
	future := now.Add(time.Second)
	past := now.Add(-time.Second)
	tests := []struct {
		name string
		at   *time.Time
		want bool
	}{
		{"nil", nil, false},
		{"future", &future, false},
		{"exact", &now, true},
		{"past", &past, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := (&Link{ExpiresAt: tt.at}).Expired(now); got != tt.want {
				t.Errorf("Expired = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestLinkStatus(t *testing.T) {
	now := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)
	past := now.Add(-time.Second)
	ban := &Ban{At: now, Reason: BanReasonPhishing, Source: BanSourceOperator}
	tests := []struct {
		name string
		link Link
		want Status
	}{
		{"active", Link{}, StatusActive},
		{"future expiry is active", Link{ExpiresAt: ptrTime(now.Add(time.Second))}, StatusActive},
		{"expiry at now is expired", Link{ExpiresAt: &now}, StatusExpired},
		{"past expiry is expired", Link{ExpiresAt: &past}, StatusExpired},
		{"disabled", Link{DisabledAt: &past}, StatusDisabled},
		{"disabled and expired", Link{ExpiresAt: &past, DisabledAt: &past}, StatusDisabled},
		{"banned", Link{Ban: ban}, StatusBanned},
		{"banned and expired", Link{ExpiresAt: &past, Ban: ban}, StatusBanned},
		{"banned and disabled", Link{DisabledAt: &past, Ban: ban}, StatusBanned},
		{"banned, disabled and expired", Link{ExpiresAt: &past, DisabledAt: &past, Ban: ban}, StatusBanned},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.link.Status(now); got != tt.want {
				t.Errorf("Status = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestLinkServable(t *testing.T) {
	now := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)
	past := now.Add(-time.Second)
	ban := &Ban{At: now, Reason: BanReasonSpam, Source: BanSourceRisk}
	tests := []struct {
		name string
		link Link
		want bool
	}{
		{"active", Link{}, true},
		{"expired", Link{ExpiresAt: &past}, false},
		{"disabled", Link{DisabledAt: &past}, false},
		{"banned", Link{Ban: ban}, false},
		{"expired, disabled and banned", Link{ExpiresAt: &past, DisabledAt: &past, Ban: ban}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.link.Servable(now); got != tt.want {
				t.Errorf("Servable = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestUpdateEmpty(t *testing.T) {
	disabled := true
	url := "https://example.com"
	tests := []struct {
		name string
		u    Update
		want bool
	}{
		{"zero", Update{}, true},
		{"url", Update{URL: &url}, false},
		{"disabled", Update{Disabled: &disabled}, false},
		{"clear expires", Update{ClearExpiresAt: true}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.u.Empty(); got != tt.want {
				t.Errorf("Empty = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestBanValidation(t *testing.T) {
	reasons := []BanReason{
		BanReasonPhishing, BanReasonMalware, BanReasonAdult, BanReasonGambling,
		BanReasonDrugs, BanReasonSpam, BanReasonOther,
	}
	for _, reason := range reasons {
		if !reason.Valid() {
			t.Errorf("BanReason(%q).Valid() = false, want true", reason)
		}
	}
	for _, reason := range []BanReason{"", "phising", "PHISHING"} {
		if reason.Valid() {
			t.Errorf("BanReason(%q).Valid() = true, want false", reason)
		}
	}
	if !BanSourceOperator.Valid() || !BanSourceRisk.Valid() {
		t.Error("operator and risk must be valid ban sources")
	}
	for _, source := range []BanSource{"", "admin", "OPERATOR"} {
		if source.Valid() {
			t.Errorf("BanSource(%q).Valid() = true, want false", source)
		}
	}
}

func ptrTime(t time.Time) *time.Time { return &t }

func TestDayStartAndKey(t *testing.T) {
	// 本地已经是 11 月 16 日，UTC 还是 11 月 15 日；日聚合按 UTC 算。
	at := time.Date(2026, 11, 16, 3, 0, 0, 0, time.FixedZone("UTC+8", 8*60*60))
	want := time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)
	if got := DayStart(at); !got.Equal(want) {
		t.Errorf("DayStart = %v, want %v", got, want)
	}
	if got, want := DayKey(at), "2026-11-15"; got != want {
		t.Errorf("DayKey = %q, want %q", got, want)
	}
}

/*
FuzzValidateURL 锁住接受下来的 URL：等于去掉首尾空白后的输入，能再校验一次，
并且是带 host 的 http/https，中间没有空白。
go test 只跑种子。持续生成输入用 go test -fuzz=FuzzValidateURL。
*/
func FuzzValidateURL(f *testing.F) {
	f.Add("https://example.com/a/b?q=1#frag")
	f.Add("  https://example.com  ")
	f.Add("HTTPS://example.com")
	f.Add("")
	f.Add("javascript:alert(1)")
	f.Add("https://example.com/a b")
	f.Add("https://example.com/%zz")
	f.Add("http://:8080")
	f.Fuzz(func(t *testing.T, raw string) {
		got, err := ValidateURL(raw)
		if err != nil {
			var verr *ValidationError
			if !errors.As(err, &verr) {
				t.Fatalf("ValidateURL(%q) error = %v, want *ValidationError", raw, err)
			}
			return
		}
		if strings.TrimSpace(raw) != got {
			t.Fatalf("ValidateURL(%q) = %q, want trimmed input", raw, got)
		}
		again, err := ValidateURL(got)
		if err != nil || again != got {
			t.Fatalf("round trip %q -> %q, %v", got, again, err)
		}
		parsed, err := url.Parse(got)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" {
			t.Fatalf("accepted %q", got)
		}
		if strings.ContainsFunc(got, unicode.IsSpace) {
			t.Fatalf("accepted whitespace in %q", got)
		}
	})
}

func BenchmarkValidateURL(b *testing.B) {
	const raw = "https://example.com/a/b?q=1"
	for range b.N {
		if _, err := ValidateURL(raw); err != nil {
			b.Fatal(err)
		}
	}
}
