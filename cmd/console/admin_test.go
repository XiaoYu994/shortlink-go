package main

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"shortlink/internal/postgres"
	"shortlink/internal/tenant"
)

func TestConsoleUsageDoesNotOpenStore(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want error
	}{
		{name: "no args", want: errUsage},
		{name: "unknown", args: []string{"serve"}, want: errUsage},
		{name: "key only", args: []string{"key"}, want: errUsage},
		{name: "bad subcommand", args: []string{"key", "delete"}, want: errUsage},
		{name: "issue extra", args: []string{"key", "issue", "--name", "acme", "extra"}, want: errUsage},
		{name: "list missing tenant", args: []string{"key", "list"}, want: errUsage},
		{name: "revoke missing id", args: []string{"key", "revoke"}, want: errUsage},
		{name: "help", args: []string{"help"}},
		{name: "issue help", args: []string{"key", "issue", "-h"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			err := run(tt.args, &stdout, &stderr, func(context.Context) (adminStore, io.Closer, error) {
				t.Fatal("store opened")
				return nil, nil, nil
			})
			if tt.want == nil {
				if err != nil {
					t.Fatalf("run error = %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tt.want.Error()) {
				t.Fatalf("run error = %v, want containing %v", err, tt.want)
			}
			if stdout.Len() != 0 {
				t.Fatalf("stdout = %q, want empty", stdout.String())
			}
			if !strings.Contains(stderr.String(), "console key issue") {
				t.Fatalf("stderr = %q, want usage", stderr.String())
			}
		})
	}
}

func TestConsoleRejectsBadExpiryWithoutOpeningStore(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := run([]string{"key", "issue", "--name", "acme", "--expires", "tomorrow"}, &stdout, &stderr, func(context.Context) (adminStore, io.Closer, error) {
		t.Fatal("store opened")
		return nil, nil, nil
	})
	if err == nil || !strings.Contains(err.Error(), "parse expires") {
		t.Fatalf("run error = %v, want parse expires", err)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
}

func TestConsoleIssueListRevoke(t *testing.T) {
	expires := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
	plaintext := "slk_" + strings.Repeat("ab", 32)
	store := &fakeAdmin{issued: postgres.IssuedAPIKey{
		Tenant:    tenant.Tenant{ID: 7, ExternalID: "platform-1"},
		Key:       postgres.APIKey{ID: 9, TenantID: 7, CreatedAt: expires, ExpiresAt: &expires},
		Plaintext: plaintext,
	}}
	var stdout, stderr bytes.Buffer
	err := run([]string{"key", "issue", "--name", "acme", "--external-id", "platform-1", "--expires", "2026-12-01T00:00:00Z"},
		&stdout, &stderr, func(context.Context) (adminStore, io.Closer, error) {
			return store, io.NopCloser(nil), nil
		})
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if store.params.Name != "acme" || store.params.ExternalID != "platform-1" || store.params.TenantID != 0 {
		t.Fatalf("params = %+v", store.params)
	}
	if store.params.ExpiresAt == nil || !store.params.ExpiresAt.Equal(expires) {
		t.Fatalf("ExpiresAt = %v, want %v", store.params.ExpiresAt, expires)
	}
	if !strings.Contains(stdout.String(), "tenant_id: 7\n") ||
		!strings.Contains(stdout.String(), "key_id: 9\n") ||
		!strings.Contains(stdout.String(), "api_key: "+plaintext+"\n") {
		t.Fatalf("stdout = %q", stdout.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q", stderr.String())
	}

	stdout.Reset()
	store.keys = []postgres.APIKey{{
		ID: 9, TenantID: 7, CreatedAt: expires, ExpiresAt: &expires,
	}}
	if err := run([]string{"key", "list", "--tenant", "7"}, &stdout, &stderr, func(context.Context) (adminStore, io.Closer, error) {
		return store, io.NopCloser(nil), nil
	}); err != nil {
		t.Fatalf("list: %v", err)
	}
	if store.listed != 7 {
		t.Fatalf("listed tenant = %d, want 7", store.listed)
	}
	if !strings.Contains(stdout.String(), "9\t7\t") || strings.Contains(stdout.String(), plaintext) {
		t.Fatalf("list stdout = %q", stdout.String())
	}

	stdout.Reset()
	if err := run([]string{"key", "revoke", "--id", "9"}, &stdout, &stderr, func(context.Context) (adminStore, io.Closer, error) {
		return store, io.NopCloser(nil), nil
	}); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if store.revoked != 9 || stdout.String() != "revoked: 9\n" {
		t.Fatalf("revoked = %d, stdout = %q", store.revoked, stdout.String())
	}
}

func TestConsoleIssueForExistingTenant(t *testing.T) {
	store := &fakeAdmin{issued: postgres.IssuedAPIKey{
		Tenant:    tenant.Tenant{ID: 3},
		Key:       postgres.APIKey{ID: 4, TenantID: 3, CreatedAt: time.Unix(0, 0).UTC()},
		Plaintext: "slk_" + strings.Repeat("cd", 32),
	}}
	var stdout, stderr bytes.Buffer
	err := run([]string{"key", "issue", "--tenant", "3"}, &stdout, &stderr, func(context.Context) (adminStore, io.Closer, error) {
		return store, io.NopCloser(nil), nil
	})
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if store.params.TenantID != 3 || store.params.Name != "" || store.params.ExpiresAt != nil {
		t.Fatalf("params = %+v", store.params)
	}
	if strings.Contains(stdout.String(), "expires_at:") {
		t.Fatalf("stdout = %q, want no expires_at", stdout.String())
	}
}

type fakeAdmin struct {
	params  postgres.IssueAPIKeyParams
	issued  postgres.IssuedAPIKey
	keys    []postgres.APIKey
	listed  int64
	revoked int64
}

func (f *fakeAdmin) IssueAPIKey(_ context.Context, p postgres.IssueAPIKeyParams) (postgres.IssuedAPIKey, error) {
	f.params = p
	return f.issued, nil
}

func (f *fakeAdmin) ListAPIKeys(_ context.Context, tenantID int64) ([]postgres.APIKey, error) {
	f.listed = tenantID
	return f.keys, nil
}

func (f *fakeAdmin) RevokeAPIKey(_ context.Context, keyID int64) error {
	f.revoked = keyID
	return nil
}
