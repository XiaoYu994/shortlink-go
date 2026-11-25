package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"shortlink/internal/identity"
	"shortlink/internal/job"
	"shortlink/internal/mailer"
)

func TestParseWorkerArgs(t *testing.T) {
	tests := []struct {
		name      string
		args      []string
		wantOK    bool
		wantUsage bool
	}{
		{
			name:   "no flags",
			args:   nil,
			wantOK: true,
		},
		{
			name:   "help flag",
			args:   []string{"-h"},
			wantOK: false,
		},
		{
			name:      "rejects extra positional args",
			args:      []string{"extra"},
			wantOK:    false,
			wantUsage: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stderr bytes.Buffer
			_, ok, err := parseWorkerArgs(tt.args, &stderr)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v (err=%v)", ok, tt.wantOK, err)
			}
			if tt.wantUsage && !errors.Is(err, errUsage) {
				t.Fatalf("err = %v, want errUsage", err)
			}
			if !tt.wantUsage && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestDrainCleanupBatches(t *testing.T) {
	var calls int
	counts := []int64{5, 5, 2}
	total, err := drainCleanupBatches(context.Background(), 5, 10, func(_ context.Context, limit int) (int64, error) {
		if limit != 5 {
			t.Fatalf("limit = %d, want 5", limit)
		}
		n := counts[calls]
		calls++
		return n, nil
	})
	if err != nil {
		t.Fatalf("drainCleanupBatches error = %v", err)
	}
	if total != 12 || calls != 3 {
		t.Errorf("total=%d calls=%d, want (12, 3)", total, calls)
	}
}

type fakeInvEmailStore struct {
	target mailer.InvitationEmailTarget
	err    error
}

func (f fakeInvEmailStore) GetInvitationEmailTarget(context.Context, int64) (mailer.InvitationEmailTarget, error) {
	return f.target, f.err
}

type fakeMailer struct {
	sent []mailer.Message
	err  error
}

func (f *fakeMailer) Send(_ context.Context, msg mailer.Message) error {
	if f.err != nil {
		return f.err
	}
	f.sent = append(f.sent, msg)
	return nil
}

func TestInvitationEmailHandler(t *testing.T) {
	t.Run("skips when smtp sender is nil", func(t *testing.T) {
		h := newInvitationEmailHandler(fakeInvEmailStore{}, nil, "", nil)
		err := h(context.Background(), job.Job{Payload: []byte(`{"invitation_id":1}`)})
		if !job.IsSkipped(err) {
			t.Fatalf("err = %v, want job.Skip", err)
		}
	})

	t.Run("skips missing or non-pending invitation", func(t *testing.T) {
		sender := &fakeMailer{}
		hMissing := newInvitationEmailHandler(fakeInvEmailStore{err: identity.ErrInvitationNotFound}, sender, "", nil)
		if err := hMissing(context.Background(), job.Job{Payload: []byte(`{"invitation_id":9}`)}); !job.IsSkipped(err) {
			t.Fatalf("missing err = %v, want job.Skip", err)
		}

		now := time.Now().UTC()
		hRevoked := newInvitationEmailHandler(fakeInvEmailStore{target: mailer.InvitationEmailTarget{
			InvitationID: 2,
			Email:        "a@example.com",
			RevokedAt:    &now,
		}}, sender, "", nil)
		if err := hRevoked(context.Background(), job.Job{Payload: []byte(`{"invitation_id":2}`)}); !job.IsSkipped(err) {
			t.Fatalf("revoked err = %v, want job.Skip", err)
		}
	})

	t.Run("sends message and maps permanent smtp error", func(t *testing.T) {
		sender := &fakeMailer{}
		future := time.Now().UTC().Add(time.Hour)
		target := mailer.InvitationEmailTarget{
			InvitationID: 5,
			TenantID:     7,
			TenantName:   "Acme",
			Email:        "bob@example.com",
			Role:         "admin",
			ExpiresAt:    &future,
		}
		h := newInvitationEmailHandler(fakeInvEmailStore{target: target}, sender, "https://console.example.com", nil)
		if err := h(context.Background(), job.Job{Payload: []byte(`{"invitation_id":5}`)}); err != nil {
			t.Fatalf("send err = %v", err)
		}
		if len(sender.sent) != 1 || sender.sent[0].To != "bob@example.com" || !strings.Contains(sender.sent[0].Body, "https://console.example.com/login") {
			t.Fatalf("sent = %+v", sender.sent)
		}

		sender.err = mailer.MarkPermanent(errors.New("550 unknown mailbox"))
		if err := h(context.Background(), job.Job{Payload: []byte(`{"invitation_id":5}`)}); !job.IsPermanent(err) {
			t.Fatalf("5xx err = %v, want job.Permanent", err)
		}
	})
}
