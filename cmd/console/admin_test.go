package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"shortlink/internal/cache"
	"shortlink/internal/identity"
	"shortlink/internal/link"
	"shortlink/internal/postgres"
	"shortlink/internal/tenant"
)

// failAdminOpen 给 link 命令的测试用：走到这里就说明打开错了存储。
func failAdminOpen(t *testing.T) func(context.Context) (adminStore, io.Closer, error) {
	t.Helper()
	return func(context.Context) (adminStore, io.Closer, error) {
		t.Fatal("admin store opened")
		return nil, nil, nil
	}
}

// failLinkOpen 给 key/tenant 命令的测试用：走到这里就说明读 Redis 了。
func failLinkOpen(t *testing.T) func(context.Context) (linkStore, io.Closer, error) {
	t.Helper()
	return func(context.Context) (linkStore, io.Closer, error) {
		t.Fatal("link store opened")
		return nil, nil, nil
	}
}

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
		{name: "member only", args: []string{"member"}, want: errUsage},
		{name: "bad member subcommand", args: []string{"member", "list"}, want: errUsage},
		{name: "invite missing tenant", args: []string{"member", "invite", "--email", "a@example.com", "--role", "member"}, want: errUsage},
		{name: "invite missing email", args: []string{"member", "invite", "--tenant", "1", "--role", "member"}, want: errUsage},
		{name: "invite missing role", args: []string{"member", "invite", "--tenant", "1", "--email", "a@example.com"}, want: errUsage},
		{name: "invite invalid role", args: []string{"member", "invite", "--tenant", "1", "--email", "a@example.com", "--role", "guest"}, want: errUsage},
		{name: "invite extra arg", args: []string{"member", "invite", "--tenant", "1", "--email", "a@example.com", "--role", "member", "extra"}, want: errUsage},
		{name: "link only", args: []string{"link"}, want: errUsage},
		{name: "bad link subcommand", args: []string{"link", "delete"}, want: errUsage},
		{name: "ban missing code", args: []string{"link", "ban", "--reason", "phishing"}, want: errUsage},
		{name: "ban missing reason", args: []string{"link", "ban", "--code", "abc12345"}, want: errUsage},
		{name: "ban invalid reason", args: []string{"link", "ban", "--code", "abc12345", "--reason", "copyright"}, want: errUsage},
		{name: "ban extra arg", args: []string{"link", "ban", "--code", "abc12345", "--reason", "phishing", "extra"}, want: errUsage},
		{name: "unban missing code", args: []string{"link", "unban"}, want: errUsage},
		{name: "unban extra arg", args: []string{"link", "unban", "--code", "abc12345", "extra"}, want: errUsage},
		{name: "help", args: []string{"help"}},
		{name: "issue help", args: []string{"key", "issue", "-h"}},
		{name: "member invite help", args: []string{"member", "invite", "-h"}},
		{name: "link help", args: []string{"link", "help"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			err := run(tt.args, &stdout, &stderr, func(context.Context) (adminStore, io.Closer, error) {
				t.Fatal("store opened")
				return nil, nil, nil
			}, func(context.Context) (linkStore, io.Closer, error) {
				t.Fatal("link store opened")
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
	}, failLinkOpen(t))
	if err == nil || !strings.Contains(err.Error(), "parse expires") {
		t.Fatalf("run error = %v, want parse expires", err)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
}

func TestConsoleRejectsBadScopeWithoutOpeningStore(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "empty value", args: []string{"key", "issue", "--name", "acme", "--scope", ""}},
		{name: "trailing comma", args: []string{"key", "issue", "--name", "acme", "--scope", "links:read,"}},
		{name: "unknown scope", args: []string{"key", "issue", "--name", "acme", "--scope", "links:delete"}},
		{name: "member scope", args: []string{"key", "issue", "--name", "acme", "--scope", "members:write"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			err := run(tt.args, &stdout, &stderr, func(context.Context) (adminStore, io.Closer, error) {
				t.Fatal("store opened")
				return nil, nil, nil
			}, failLinkOpen(t))
			if err == nil || !errors.Is(err, identity.ErrInvalidAPIKeyScope) || !strings.Contains(err.Error(), "parse scope") {
				t.Fatalf("run error = %v, want parse scope wrapping ErrInvalidAPIKeyScope", err)
			}
			if stdout.Len() != 0 {
				t.Fatalf("stdout = %q, want empty", stdout.String())
			}
		})
	}
}

func TestConsoleIssueListRevoke(t *testing.T) {
	expires := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
	plaintext := "slk_" + strings.Repeat("ab", 32)
	store := &fakeAdmin{issued: postgres.IssuedAPIKey{
		Tenant: tenant.Tenant{ID: 7, ExternalID: "platform-1"},
		Key: postgres.APIKey{
			ID:        9,
			TenantID:  7,
			Scopes:    identity.AllAPIKeyScopes(),
			CreatedAt: expires,
			ExpiresAt: &expires,
		},
		Plaintext: plaintext,
	}}
	var stdout, stderr bytes.Buffer
	err := run([]string{"key", "issue", "--name", "acme", "--external-id", "platform-1", "--expires", "2026-12-01T00:00:00Z"},
		&stdout, &stderr, func(context.Context) (adminStore, io.Closer, error) {
			return store, io.NopCloser(nil), nil
		}, failLinkOpen(t))
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if store.params.Name != "acme" || store.params.ExternalID != "platform-1" || store.params.TenantID != 0 {
		t.Fatalf("params = %+v", store.params)
	}
	if store.params.ExpiresAt == nil || !store.params.ExpiresAt.Equal(expires) {
		t.Fatalf("ExpiresAt = %v, want %v", store.params.ExpiresAt, expires)
	}
	if !slices.Equal(store.params.Scopes, identity.AllAPIKeyScopes()) {
		t.Fatalf("Scopes = %v, want all api key scopes", store.params.Scopes)
	}
	if !strings.Contains(stdout.String(), "tenant_id: 7\n") ||
		!strings.Contains(stdout.String(), "key_id: 9\n") ||
		!strings.Contains(stdout.String(), "scopes: links:read,links:write,stats:read\n") ||
		!strings.Contains(stdout.String(), "api_key: "+plaintext+"\n") {
		t.Fatalf("stdout = %q", stdout.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q", stderr.String())
	}

	stdout.Reset()
	store.keys = []postgres.APIKey{{
		ID:        9,
		TenantID:  7,
		Scopes:    []identity.Perm{identity.PermLinksRead, identity.PermStatsRead},
		CreatedAt: expires,
		ExpiresAt: &expires,
	}}
	if err := run([]string{"key", "list", "--tenant", "7"}, &stdout, &stderr, func(context.Context) (adminStore, io.Closer, error) {
		return store, io.NopCloser(nil), nil
	}, failLinkOpen(t)); err != nil {
		t.Fatalf("list: %v", err)
	}
	if store.listed != 7 {
		t.Fatalf("listed tenant = %d, want 7", store.listed)
	}
	if !strings.Contains(stdout.String(), "id\ttenant_id\tcreated_at\texpires_at\trevoked_at\tscopes\n") ||
		!strings.Contains(stdout.String(), "9\t7\t") ||
		!strings.Contains(stdout.String(), "\tlinks:read,stats:read\n") ||
		strings.Contains(stdout.String(), plaintext) {
		t.Fatalf("list stdout = %q", stdout.String())
	}

	stdout.Reset()
	if err := run([]string{"key", "revoke", "--id", "9"}, &stdout, &stderr, func(context.Context) (adminStore, io.Closer, error) {
		return store, io.NopCloser(nil), nil
	}, failLinkOpen(t)); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if store.revoked != 9 || stdout.String() != "revoked: 9\n" {
		t.Fatalf("revoked = %d, stdout = %q", store.revoked, stdout.String())
	}
}

func TestConsoleIssueWithScopes(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want []identity.Perm
	}{
		{
			name: "comma separated with spaces",
			args: []string{"key", "issue", "--tenant", "3", "--scope", "stats:read, links:read"},
			want: []identity.Perm{identity.PermLinksRead, identity.PermStatsRead},
		},
		{
			name: "repeated flags and duplicates",
			args: []string{"key", "issue", "--tenant", "3", "--scope", "links:write", "--scope", "links:read,links:write"},
			want: []identity.Perm{identity.PermLinksRead, identity.PermLinksWrite},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &fakeAdmin{issued: postgres.IssuedAPIKey{
				Tenant:    tenant.Tenant{ID: 3},
				Key:       postgres.APIKey{ID: 4, TenantID: 3, Scopes: tt.want, CreatedAt: time.Unix(0, 0).UTC()},
				Plaintext: "slk_" + strings.Repeat("ef", 32),
			}}
			var stdout, stderr bytes.Buffer
			err := run(tt.args, &stdout, &stderr, func(context.Context) (adminStore, io.Closer, error) {
				return store, io.NopCloser(nil), nil
			}, failLinkOpen(t))
			if err != nil {
				t.Fatalf("issue: %v", err)
			}
			if !slices.Equal(store.params.Scopes, tt.want) {
				t.Fatalf("params.Scopes = %v, want %v", store.params.Scopes, tt.want)
			}
			if !strings.Contains(stdout.String(), "scopes: "+formatScopes(tt.want)+"\n") {
				t.Fatalf("stdout = %q, want scopes line", stdout.String())
			}
		})
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
	}, failLinkOpen(t))
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
	limits := postgres.TenantLimits{ID: 7, Name: "acme", LinkUsage: 3, LinkQuota: 1000, RateLimit: 10}
	store := &fakeAdmin{limits: limits}
	open := func(context.Context) (adminStore, io.Closer, error) {
		return store, io.NopCloser(nil), nil
	}

	var stdout, stderr bytes.Buffer
	openLink := failLinkOpen(t)
	if err := run([]string{"tenant", "show", "--tenant", "7"}, &stdout, &stderr, open, openLink); err != nil {
		t.Fatalf("show: %v", err)
	}
	if store.shown != 7 {
		t.Fatalf("shown tenant = %d, want 7", store.shown)
	}
	want := "id\tname\tlink_usage\tlink_quota\trate_limit_per_second\n7\tacme\t3\t1000\t10\n"
	if stdout.String() != want {
		t.Fatalf("show stdout = %q, want %q", stdout.String(), want)
	}

	stdout.Reset()
	if err := run([]string{"tenant", "set", "--tenant", "7", "--link-quota", "5"}, &stdout, &stderr, open, openLink); err != nil {
		t.Fatalf("set link quota: %v", err)
	}
	if store.setTenant != 7 || store.setParams.LinkQuota == nil || *store.setParams.LinkQuota != 5 {
		t.Fatalf("set tenant = %d, params = %+v", store.setTenant, store.setParams)
	}
	if store.setParams.RateLimit != nil {
		t.Fatalf("RateLimit = %v, want nil", *store.setParams.RateLimit)
	}

	stdout.Reset()
	if err := run([]string{"tenant", "set", "--tenant", "7", "--rate-limit", "0"}, &stdout, &stderr, open, openLink); err != nil {
		t.Fatalf("set rate limit: %v", err)
	}
	if store.setParams.LinkQuota != nil || store.setParams.RateLimit == nil || *store.setParams.RateLimit != 0 {
		t.Fatalf("params = %+v, want rate limit 0 and no quota", store.setParams)
	}
}

type fakeLink struct {
	link *link.Link
	err  error

	banCalls   int
	bannedCode string
	banReason  link.BanReason
	banSource  link.BanSource
	unbanCalls int
	unbanCode  string
}

func (f *fakeLink) Ban(_ context.Context, code string, reason link.BanReason, source link.BanSource) (*link.Link, error) {
	f.banCalls++
	f.bannedCode, f.banReason, f.banSource = code, reason, source
	return f.link, f.err
}

func (f *fakeLink) Unban(_ context.Context, code string) (*link.Link, error) {
	f.unbanCalls++
	f.unbanCode = code
	return f.link, f.err
}

func TestConsoleLinkBanAndUnban(t *testing.T) {
	bannedAt := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
	store := &fakeLink{link: &link.Link{
		Code: "abc12345",
		Ban:  &link.Ban{At: bannedAt, Reason: link.BanReasonPhishing, Source: link.BanSourceOperator},
	}}
	openLink := func(context.Context) (linkStore, io.Closer, error) {
		return store, io.NopCloser(nil), nil
	}

	var stdout, stderr bytes.Buffer
	if err := run([]string{"link", "ban", "--code", "abc12345", "--reason", "phishing"},
		&stdout, &stderr, failAdminOpen(t), openLink); err != nil {
		t.Fatalf("ban: %v", err)
	}
	if store.bannedCode != "abc12345" || store.banReason != link.BanReasonPhishing || store.banSource != link.BanSourceOperator {
		t.Fatalf("Ban(%q, %q, %q), want (abc12345, phishing, operator)",
			store.bannedCode, store.banReason, store.banSource)
	}
	want := "code\tstatus\tbanned_at\tban_reason\tban_source\n" +
		"abc12345\tbanned\t2026-11-18T10:00:00Z\tphishing\toperator\n"
	if stdout.String() != want {
		t.Fatalf("ban stdout = %q, want %q", stdout.String(), want)
	}

	// 解封后链接仍然停用：输出当前状态 disabled，封禁字段清空。
	disabledAt := bannedAt.Add(-time.Hour)
	store.link = &link.Link{Code: "abc12345", DisabledAt: &disabledAt}
	stdout.Reset()
	if err := run([]string{"link", "unban", "--code", "abc12345"},
		&stdout, &stderr, failAdminOpen(t), openLink); err != nil {
		t.Fatalf("unban: %v", err)
	}
	if store.unbanCode != "abc12345" {
		t.Fatalf("Unban(%q), want abc12345", store.unbanCode)
	}
	want = "code\tstatus\tbanned_at\tban_reason\tban_source\n" +
		"abc12345\tdisabled\t-\t-\t-\n"
	if stdout.String() != want {
		t.Fatalf("unban stdout = %q, want %q", stdout.String(), want)
	}
}

func TestConsoleLinkNotFound(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "ban", args: []string{"link", "ban", "--code", "missing1", "--reason", "spam"}},
		{name: "unban", args: []string{"link", "unban", "--code", "missing1"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &fakeLink{err: link.ErrNotFound}
			var stdout, stderr bytes.Buffer
			err := run(tt.args, &stdout, &stderr, failAdminOpen(t), func(context.Context) (linkStore, io.Closer, error) {
				return store, io.NopCloser(nil), nil
			})
			if err == nil || errors.Is(err, errUsage) || !errors.Is(err, link.ErrNotFound) {
				t.Fatalf("run error = %v, want link.ErrNotFound and not usage", err)
			}
			if stdout.Len() != 0 {
				t.Fatalf("stdout = %q, want empty", stdout.String())
			}
		})
	}
}

func TestConsoleLinkCacheNotUpdated(t *testing.T) {
	store := &fakeLink{
		link: &link.Link{
			Code: "abc12345",
			Ban:  &link.Ban{At: time.Now(), Reason: link.BanReasonSpam, Source: link.BanSourceOperator},
		},
		err: fmt.Errorf("ban link: %w", cache.ErrNotUpdated),
	}
	var stdout, stderr bytes.Buffer
	err := run([]string{"link", "ban", "--code", "abc12345", "--reason", "spam"},
		&stdout, &stderr, failAdminOpen(t), func(context.Context) (linkStore, io.Closer, error) {
			return store, io.NopCloser(nil), nil
		})
	if err == nil || errors.Is(err, errUsage) {
		t.Fatalf("run error = %v, want non-usage error", err)
	}
	if !errors.Is(err, cache.ErrNotUpdated) {
		t.Fatalf("run error = %v, want cache.ErrNotUpdated", err)
	}
	for _, want := range []string{"已经写入数据库", "缓存没有更新", "1 小时", "重跑"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want containing %q", err.Error(), want)
		}
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
}

// TestConsoleKeyAndTenantDoNotOpenLinkStore 锁住 ADR 0009：只有 link 命令需要 Redis。
func TestConsoleKeyAndTenantDoNotOpenLinkStore(t *testing.T) {
	t.Setenv("REDIS_URL", "")
	store := &fakeAdmin{
		limits:     postgres.TenantLimits{ID: 1, Name: "acme"},
		invitation: identity.Invitation{ID: 1, TenantID: 1, Email: "alice@example.com", Role: identity.RoleOwner, CreatedAt: time.Unix(0, 0).UTC()},
	}
	open := func(context.Context) (adminStore, io.Closer, error) {
		return store, io.NopCloser(nil), nil
	}
	openLink := failLinkOpen(t)

	var stdout, stderr bytes.Buffer
	if err := run([]string{"tenant", "show", "--tenant", "1"}, &stdout, &stderr, open, openLink); err != nil {
		t.Fatalf("tenant show: %v", err)
	}
	stdout.Reset()
	if err := run([]string{"key", "revoke", "--id", "9"}, &stdout, &stderr, open, openLink); err != nil {
		t.Fatalf("key revoke: %v", err)
	}
	stdout.Reset()
	if err := run([]string{"member", "invite", "--tenant", "1", "--email", "alice@example.com", "--role", "owner"}, &stdout, &stderr, open, openLink); err != nil {
		t.Fatalf("member invite: %v", err)
	}
}

func TestConsoleMemberInviteRejectsBadEmailAndExpiryWithoutOpeningStore(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{
			name:    "invalid email",
			args:    []string{"member", "invite", "--tenant", "1", "--email", "not-an-email", "--role", "member"},
			wantErr: "parse email",
		},
		{
			name:    "invalid expires",
			args:    []string{"member", "invite", "--tenant", "1", "--email", "alice@example.com", "--role", "member", "--expires", "tomorrow"},
			wantErr: "parse expires",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			err := run(tt.args, &stdout, &stderr, func(context.Context) (adminStore, io.Closer, error) {
				t.Fatal("admin store opened")
				return nil, nil, nil
			}, failLinkOpen(t))
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("run error = %v, want containing %q", err, tt.wantErr)
			}
			if stdout.Len() != 0 {
				t.Fatalf("stdout = %q, want empty", stdout.String())
			}
		})
	}
}

func TestConsoleMemberInvite(t *testing.T) {
	created := time.Date(2026, 11, 22, 4, 0, 0, 0, time.UTC)
	expires := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)

	t.Run("without expires", func(t *testing.T) {
		store := &fakeAdmin{
			invitation: identity.Invitation{
				ID:        12,
				TenantID:  7,
				Email:     "alice@example.com",
				Role:      identity.RoleOwner,
				CreatedAt: created,
			},
		}
		var stdout, stderr bytes.Buffer
		err := run([]string{"member", "invite", "--tenant", "7", "--email", "  Alice@Example.COM ", "--role", "owner"},
			&stdout, &stderr, func(context.Context) (adminStore, io.Closer, error) {
				return store, io.NopCloser(nil), nil
			}, failLinkOpen(t))
		if err != nil {
			t.Fatalf("member invite: %v", err)
		}
		if store.inviteParams.TenantID != 7 || store.inviteParams.Email != "alice@example.com" || store.inviteParams.Role != identity.RoleOwner || store.inviteParams.ExpiresAt != nil {
			t.Fatalf("inviteParams = %+v", store.inviteParams)
		}
		want := "id\ttenant_id\temail\trole\tcreated_at\texpires_at\n" +
			"12\t7\talice@example.com\towner\t2026-11-22T04:00:00Z\t-\n"
		if stdout.String() != want {
			t.Fatalf("stdout = %q, want %q", stdout.String(), want)
		}
		if stderr.Len() != 0 {
			t.Fatalf("stderr = %q, want empty", stderr.String())
		}
	})

	t.Run("with expires", func(t *testing.T) {
		store := &fakeAdmin{
			invitation: identity.Invitation{
				ID:        13,
				TenantID:  7,
				Email:     "bob@example.com",
				Role:      identity.RoleViewer,
				CreatedAt: created,
				ExpiresAt: &expires,
			},
		}
		var stdout, stderr bytes.Buffer
		err := run([]string{"member", "invite", "--tenant", "7", "--email", "bob@example.com", "--role", "viewer", "--expires", "2026-12-01T00:00:00Z"},
			&stdout, &stderr, func(context.Context) (adminStore, io.Closer, error) {
				return store, io.NopCloser(nil), nil
			}, failLinkOpen(t))
		if err != nil {
			t.Fatalf("member invite with expires: %v", err)
		}
		if store.inviteParams.ExpiresAt == nil || !store.inviteParams.ExpiresAt.Equal(expires) {
			t.Fatalf("ExpiresAt = %v, want %v", store.inviteParams.ExpiresAt, expires)
		}
		want := "id\ttenant_id\temail\trole\tcreated_at\texpires_at\n" +
			"13\t7\tbob@example.com\tviewer\t2026-11-22T04:00:00Z\t2026-12-01T00:00:00Z\n"
		if stdout.String() != want {
			t.Fatalf("stdout = %q, want %q", stdout.String(), want)
		}
	})
}

type fakeAdmin struct {
	params       postgres.IssueAPIKeyParams
	issued       postgres.IssuedAPIKey
	keys         []postgres.APIKey
	listed       int64
	revoked      int64
	limits       postgres.TenantLimits
	shown        int64
	setTenant    int64
	setParams    postgres.SetTenantLimitsParams
	inviteParams postgres.CreateInvitationParams
	invitation   identity.Invitation
	inviteErr    error
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

func (f *fakeAdmin) CreateInvitation(_ context.Context, p postgres.CreateInvitationParams) (identity.Invitation, error) {
	f.inviteParams = p
	return f.invitation, f.inviteErr
}
