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
		{name: "tenant only", args: []string{"tenant"}, want: errUsage},
		{name: "bad tenant subcommand", args: []string{"tenant", "delete"}, want: errUsage},
		{name: "tenant show missing tenant", args: []string{"tenant", "show"}, want: errUsage},
		{name: "tenant set missing tenant", args: []string{"tenant", "set"}, want: errUsage},
		{name: "tenant set no options", args: []string{"tenant", "set", "--tenant", "1"}, want: errUsage},
		{name: "tenant set negative", args: []string{"tenant", "set", "--tenant", "1", "--link-quota", "-1"}, want: errUsage},
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

func TestConsoleTenantShowAndSet(t *testing.T) {
	limits := postgres.TenantLimits{ID: 7, Name: "acme", LinkCount: 3, LinkQuota: 1000, RateLimit: 10}
	store := &fakeAdmin{limits: limits}
	open := func(context.Context) (adminStore, io.Closer, error) {
		return store, io.NopCloser(nil), nil
	}

	var stdout, stderr bytes.Buffer
	if err := run([]string{"tenant", "show", "--tenant", "7"}, &stdout, &stderr, open); err != nil {
		t.Fatalf("show: %v", err)
	}
	if store.shown != 7 {
		t.Fatalf("shown tenant = %d, want 7", store.shown)
	}
	want := "id\tname\tlink_count\tlink_quota\trate_limit_per_second\n7\tacme\t3\t1000\t10\n"
	if stdout.String() != want {
		t.Fatalf("show stdout = %q, want %q", stdout.String(), want)
	}

	stdout.Reset()
	if err := run([]string{"tenant", "set", "--tenant", "7", "--link-quota", "5"}, &stdout, &stderr, open); err != nil {
		t.Fatalf("set link quota: %v", err)
	}
	if store.setTenant != 7 || store.setParams.LinkQuota == nil || *store.setParams.LinkQuota != 5 {
		t.Fatalf("set tenant = %d, params = %+v", store.setTenant, store.setParams)
	}
	if store.setParams.RateLimit != nil {
		t.Fatalf("RateLimit = %v, want nil", *store.setParams.RateLimit)
	}

	stdout.Reset()
	if err := run([]string{"tenant", "set", "--tenant", "7", "--rate-limit", "0"}, &stdout, &stderr, open); err != nil {
		t.Fatalf("set rate limit: %v", err)
	}
	if store.setParams.LinkQuota != nil || store.setParams.RateLimit == nil || *store.setParams.RateLimit != 0 {
		t.Fatalf("params = %+v, want rate limit 0 and no quota", store.setParams)
	}
}

type fakeAdmin struct {
	params    postgres.IssueAPIKeyParams
	issued    postgres.IssuedAPIKey
	keys      []postgres.APIKey
	listed    int64
	revoked   int64
	limits    postgres.TenantLimits
	shown     int64
	setTenant int64
	setParams postgres.SetTenantLimitsParams
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

func (f *fakeAdmin) GetTenantLimits(_ context.Context, tenantID int64) (postgres.TenantLimits, error) {
	f.shown = tenantID
	return f.limits, nil
}

func (f *fakeAdmin) SetTenantLimits(_ context.Context, tenantID int64, p postgres.SetTenantLimitsParams) (postgres.TenantLimits, error) {
	f.setTenant = tenantID
	f.setParams = p
	return f.limits, nil
}
