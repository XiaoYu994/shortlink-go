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
		name       string
		args       []string
		wantOK     bool
		wantUsage  bool
		wantConfig string
	}{
		{
			name:   "no flags",
			args:   nil,
			wantOK: true,
		},
		{
			name:       "with config flag",
			args:       []string{"--config", "/etc/shortlink.yaml"},
			wantOK:     true,
			wantConfig: "/etc/shortlink.yaml",
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
			opts, ok, err := parseWorkerArgs(tt.args, &stderr)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v (err=%v)", ok, tt.wantOK, err)
			}
			if tt.wantUsage && !errors.Is(err, errUsage) {
				t.Fatalf("err = %v, want errUsage", err)
			}
			if !tt.wantUsage && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if opts.configPath != tt.wantConfig {
				t.Fatalf("configPath = %q, want %q", opts.configPath, tt.wantConfig)
			}
		})
	}
}

func TestStripLeadingConfigFlagAndParseConfigCommand(t *testing.T) {
	cfg, rest, err := stripLeadingConfigFlag([]string{"--config", "/tmp/a.yaml", "key", "issue"})
	if err != nil || cfg != "/tmp/a.yaml" || len(rest) != 2 || rest[0] != "key" {
		t.Fatalf("stripLeadingConfigFlag = (%q, %v, %v)", cfg, rest, err)
	}
	cfg, rest, err = stripLeadingConfigFlag([]string{"--config=/tmp/b.yaml", "serve"})
	if err != nil || cfg != "/tmp/b.yaml" || len(rest) != 1 || rest[0] != "serve" {
		t.Fatalf("stripLeadingConfigFlag(=) = (%q, %v, %v)", cfg, rest, err)
	}
	if _, _, err := stripLeadingConfigFlag([]string{"--config"}); !errors.Is(err, errUsage) {
		t.Fatalf("missing config value err = %v, want errUsage", err)
	}

	var stderr bytes.Buffer
	dumpOpts, ok, err := parseConfigCommand([]string{"dump", "--config", "/tmp/c.yaml", "--format", "text"}, &stderr)
	if !ok || err != nil || dumpOpts.configPath != "/tmp/c.yaml" || dumpOpts.format != "text" {
		t.Fatalf("parseConfigCommand = (%+v, %v, %v)", dumpOpts, ok, err)
	}
	if _, ok, err := parseConfigCommand([]string{"unknown"}, &stderr); ok || !errors.Is(err, errUsage) {
		t.Fatalf("parseConfigCommand(unknown) = (%v, %v), want errUsage", ok, err)
	}
}

func TestParseAllowTestHostArgs(t *testing.T) {
	tests := []struct {
		name      string
		args      []string
		wantOK    bool
		wantErr   bool
		wantHosts []string
	}{
		{
			name:      "valid loopback test host",
			args:      []string{"--allow-test-host", "127.0.0.1:18089"},
			wantOK:    true,
			wantHosts: []string{"127.0.0.1:18089"},
		},
		{
			name:      "valid localhost test host",
			args:      []string{"--allow-test-host", "localhost:18089"},
			wantOK:    true,
			wantHosts: []string{"localhost:18089"},
		},
		{
			name:    "rejects public ip",
			args:    []string{"--allow-test-host", "8.8.8.8:80"},
			wantOK:  false,
			wantErr: true,
		},
		{
			name:    "rejects missing port",
			args:    []string{"--allow-test-host", "127.0.0.1"},
			wantOK:  false,
			wantErr: true,
		},
		{
			name:    "rejects non-numeric port",
			args:    []string{"--allow-test-host", "127.0.0.1:abc"},
			wantOK:  false,
			wantErr: true,
		},
	}

	parsers := []struct {
		name string
		fn   func([]string) ([]string, bool, error)
	}{
		{
			name: "worker",
			fn: func(args []string) ([]string, bool, error) {
				var stderr bytes.Buffer
				opts, ok, err := parseWorkerArgs(args, &stderr)
				return opts.allowTestHosts, ok, err
			},
		},
		{
			name: "serve",
			fn: func(args []string) ([]string, bool, error) {
				var stderr bytes.Buffer
				opts, ok, err := parseServeArgs(args, &stderr)
				return opts.allowTestHosts, ok, err
			},
		},
	}

	for _, p := range parsers {
		for _, tt := range tests {
			t.Run(p.name+"/"+tt.name, func(t *testing.T) {
				hosts, ok, err := p.fn(tt.args)
				if ok != tt.wantOK {
					t.Fatalf("ok = %v, want %v (err=%v)", ok, tt.wantOK, err)
				}
				if tt.wantErr {
					if err == nil {
						t.Fatal("expected error, got nil")
					}
					return
				}
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if len(hosts) != len(tt.wantHosts) {
					t.Fatalf("allowTestHosts = %v, want %v", hosts, tt.wantHosts)
				}
				for i := range tt.wantHosts {
					if hosts[i] != tt.wantHosts[i] {
						t.Errorf("allowTestHosts[%d] = %q, want %q", i, hosts[i], tt.wantHosts[i])
					}
				}
			})
		}
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
