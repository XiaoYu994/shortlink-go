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
	if len(actions) != 14 {
		t.Fatalf("len(AllActions()) = %d, want 14", len(actions))
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

	for _, tt := range []TargetType{TargetOrg, TargetLink, TargetMember, TargetInvitation, TargetAPIKey} {
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

func TestNormalizeMeta(t *testing.T) {
	pos := int64(9)
	zero := int64(0)
	neg := int64(-3)
	longReq := strings.Repeat("a", 200)
	longUTF8 := strings.Repeat("世", 50) // 150 字节：按 rune 边界截断应为 42*3=126 字节。

	tests := []struct {
		name     string
		in       Meta
		wantKind ActorKind
		wantID   *int64
		wantReq  string
	}{
		{
			name:     "valid user",
			in:       UserMeta(pos, "req-user"),
			wantKind: ActorUser,
			wantID:   &pos,
			wantReq:  "req-user",
		},
		{
			name:     "valid api key",
			in:       APIKeyMeta(pos, "req-key"),
			wantKind: ActorAPIKey,
			wantID:   &pos,
			wantReq:  "req-key",
		},
		{
			name:     "user without id falls back to system",
			in:       Meta{ActorKind: ActorUser, RequestID: "req-1"},
			wantKind: ActorSystem,
			wantID:   nil,
			wantReq:  "req-1",
		},
		{
			name:     "api key with zero id falls back to system",
			in:       Meta{ActorKind: ActorAPIKey, ActorID: &zero, RequestID: "req-2"},
			wantKind: ActorSystem,
			wantID:   nil,
			wantReq:  "req-2",
		},
		{
			name:     "user with negative id falls back to system",
			in:       Meta{ActorKind: ActorUser, ActorID: &neg, RequestID: "req-3"},
			wantKind: ActorSystem,
			wantID:   nil,
			wantReq:  "req-3",
		},
		{
			name:     "operator drops stray actor id",
			in:       Meta{ActorKind: ActorOperator, ActorID: &pos, RequestID: ""},
			wantKind: ActorOperator,
			wantID:   nil,
			wantReq:  "",
		},
		{
			name:     "system drops stray actor id",
			in:       Meta{ActorKind: ActorSystem, ActorID: &pos, RequestID: "job:12"},
			wantKind: ActorSystem,
			wantID:   nil,
			wantReq:  "job:12",
		},
		{
			name:     "unknown kind falls back to system and truncates request id",
			in:       Meta{ActorKind: ActorKind("bot"), ActorID: &pos, RequestID: longReq},
			wantKind: ActorSystem,
			wantID:   nil,
			wantReq:  longReq[:128],
		},
		{
			name:     "multi-byte utf8 request id truncates cleanly on rune boundary",
			in:       Meta{ActorKind: ActorSystem, RequestID: longUTF8},
			wantKind: ActorSystem,
			wantID:   nil,
			wantReq:  strings.Repeat("世", 42),
		},
		{
			name:     "invalid utf8 sequence in request id is sanitized",
			in:       Meta{ActorKind: ActorSystem, RequestID: "req-\xffok"},
			wantKind: ActorSystem,
			wantID:   nil,
			wantReq:  "req-ok",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := NormalizeMeta(tt.in)
			if !utf8.ValidString(got.RequestID) || len(got.RequestID) > maxRequestIDLen {
				t.Fatalf("RequestID %q is invalid UTF-8 or over %d bytes", got.RequestID, maxRequestIDLen)
			}
			if got.ActorKind != tt.wantKind || got.RequestID != tt.wantReq {
				t.Fatalf("NormalizeMeta = %+v, want kind=%q req=%q", got, tt.wantKind, tt.wantReq)
			}
			if (got.ActorID == nil) != (tt.wantID == nil) {
				t.Fatalf("ActorID nil mismatch: got %v, want %v", got.ActorID, tt.wantID)
			}
			if got.ActorID != nil && *got.ActorID != *tt.wantID {
				t.Fatalf("ActorID = %d, want %d", *got.ActorID, *tt.wantID)
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
