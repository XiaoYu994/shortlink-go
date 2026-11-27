package audit

import (
	"context"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestEnumsValidAndAllActions(t *testing.T) {
	for _, kind := range []ActorKind{ActorUser, ActorAPIKey, ActorOperator, ActorSystem} {
		if !kind.Valid() {
			t.Errorf("%q.Valid() = false, want true", kind)
		}
	}
	if ActorKind("unknown").Valid() {
		t.Error(`ActorKind("unknown").Valid() = true, want false`)
	}

	actions := AllActions()
	if len(actions) != 20 {
		t.Fatalf("len(AllActions()) = %d, want 20", len(actions))
	}
	for _, act := range actions {
		if !act.Valid() {
			t.Errorf("Action %q.Valid() = false, want true", act)
		}
	}
	if Action("link.unknown").Valid() {
		t.Error(`Action("link.unknown").Valid() = true, want false`)
	}
	// 修改返回切片不能污染后续调用。
	actions[0] = "tampered"
	if got := AllActions()[0]; got == "tampered" {
		t.Fatal("AllActions() returned shared backing array")
	}

	for _, tt := range []TargetType{TargetOrg, TargetLink, TargetMember, TargetInvitation, TargetAPIKey, TargetWebhook} {
		if !tt.Valid() {
			t.Errorf("%q.Valid() = false, want true", tt)
		}
	}
	if TargetType("session").Valid() {
		t.Error(`TargetType("session").Valid() = true, want false`)
	}
}

func TestWithMetaAndPointerIsolation(t *testing.T) {
	if _, ok := MetaFrom(context.TODO()); ok {
		t.Fatal("MetaFrom(empty) = true, want false")
	}

	orig := UserMeta(42, "req-1")
	ctx := WithMeta(context.Background(), orig)
	*orig.ActorID = 999

	got, ok := MetaFrom(ctx)
	if !ok {
		t.Fatal("MetaFrom = false, want true")
	}
	if got.ActorKind != ActorUser || got.ActorID == nil || *got.ActorID != 42 || got.RequestID != "req-1" {
		t.Fatalf("got = %+v, want user 42 req-1", got)
	}

	*got.ActorID = 777
	gotAgain, _ := MetaFrom(ctx)
	if *gotAgain.ActorID != 42 {
		t.Fatalf("ActorID mutated across MetaFrom calls: got %d, want 42", *gotAgain.ActorID)
	}
}

func TestSanitizeRequestID(t *testing.T) {
	longReq := strings.Repeat("a", 200)
	longUTF8 := strings.Repeat("世", 50) // 150 字节：按 rune 边界截断应为 42*3=126 字节。

	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "short ascii", in: "req-1", want: "req-1"},
		{name: "truncates over 128 bytes", in: longReq, want: longReq[:128]},
		{name: "truncates multi-byte utf8 on rune boundary", in: longUTF8, want: strings.Repeat("世", 42)},
		{name: "strips invalid utf8", in: "req-\xffok", want: "req-ok"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SanitizeRequestID(tt.in)
			if !utf8.ValidString(got) || len(got) > maxRequestIDLen {
				t.Fatalf("SanitizeRequestID(%q) = %q is invalid UTF-8 or over %d bytes", tt.in, got, maxRequestIDLen)
			}
			if got != tt.want {
				t.Fatalf("SanitizeRequestID(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestValidateMeta(t *testing.T) {
	pos := int64(5)
	zero := int64(0)
	invalid := []Meta{
		{},
		{ActorKind: ActorKind("unknown")},
		{ActorKind: ActorUser},
		{ActorKind: ActorUser, ActorID: &zero},
		{ActorKind: ActorAPIKey},
		{ActorKind: ActorOperator, ActorID: &pos},
		{ActorKind: ActorSystem, ActorID: &pos},
	}
	for _, m := range invalid {
		if err := ValidateMeta(m); !errors.Is(err, ErrMissingMeta) {
			t.Errorf("ValidateMeta(%+v) = %v, want ErrMissingMeta", m, err)
		}
	}
	for _, m := range []Meta{UserMeta(5, "r"), APIKeyMeta(6, "r"), OperatorMeta("cli"), SystemMeta("job:1")} {
		if err := ValidateMeta(m); err != nil {
			t.Errorf("ValidateMeta(%+v) = %v, want nil", m, err)
		}
	}
}
