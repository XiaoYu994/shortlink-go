package postgres

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"shortlink/internal/identity"
)

func TestMigration007APIKeyDefaultsAndConstraints(t *testing.T) {
	store, tenantID := newTestStore(t)
	ctx := testAuditCtx(context.Background())

	issued, err := store.IssueAPIKey(ctx, IssueAPIKeyParams{TenantID: tenantID, Scopes: identity.AllAPIKeyScopes()})
	if err != nil {
		t.Fatalf("IssueAPIKey: %v", err)
	}

	var (
		gotScopes string
		createdBy *int64
	)
	if err := store.db.QueryRowContext(ctx,
		`SELECT array_to_string(scopes, ','), created_by FROM api_keys WHERE id = $1`,
		issued.Key.ID,
	).Scan(&gotScopes, &createdBy); err != nil {
		t.Fatalf("select api_keys defaults: %v", err)
	}
	if want := "links:read,links:write,stats:read"; gotScopes != want {
		t.Errorf("default scopes = %q, want %q", gotScopes, want)
	}
	if createdBy != nil {
		t.Errorf("default created_by = %v, want nil", *createdBy)
	}

	tests := []struct {
		name      string
		scopesSQL string
	}{
		{name: "empty scopes", scopesSQL: "'{}'::text[]"},
		{name: "unknown scope", scopesSQL: "ARRAY['members:write']::text[]"},
		{name: "null scope element", scopesSQL: "ARRAY[NULL]::text[]"},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hash := bytes.Repeat([]byte{byte(i + 1)}, 32)
			_, err := store.db.ExecContext(ctx,
				`INSERT INTO api_keys (tenant_id, key_hash, scopes) VALUES ($1, $2, `+tt.scopesSQL+`)`,
				tenantID, hash)
			if err == nil {
				t.Fatalf("expected constraint violation for %s", tt.name)
			}
		})
	}
}

// 迁移 008 去掉 scopes 的 DEFAULT：漏写 scopes 的 INSERT 必须失败，而不是悄悄拿到全部权限。
func TestMigration008APIKeyScopesHasNoDefault(t *testing.T) {
	store, tenantID := newTestStore(t)
	hash := bytes.Repeat([]byte{0x7f}, 32)
	_, err := store.db.ExecContext(context.Background(),
		`INSERT INTO api_keys (tenant_id, key_hash) VALUES ($1, $2)`, tenantID, hash)
	if err == nil {
		t.Fatal("INSERT without scopes succeeded, want NOT NULL violation")
	}
}

func TestMigration007UsersAndIdentitiesConstraints(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := testAuditCtx(context.Background())

	var user1, user2 int64
	if err := store.db.QueryRowContext(ctx,
		`INSERT INTO users (email, name) VALUES ('same@example.com', 'Alice') RETURNING id`,
	).Scan(&user1); err != nil {
		t.Fatalf("insert user1: %v", err)
	}
	if err := store.db.QueryRowContext(ctx,
		`INSERT INTO users (email, name) VALUES ('same@example.com', 'Alice 2') RETURNING id`,
	).Scan(&user2); err != nil {
		t.Fatalf("insert user2 with same email: %v", err)
	}
	if user1 == user2 {
		t.Fatalf("expected distinct user ids, got %d", user1)
	}

	if _, err := store.db.ExecContext(ctx,
		`INSERT INTO user_identities (user_id, issuer, subject, email, email_verified)
		 VALUES ($1, 'https://dex.local', 'sub-1', 'same@example.com', true)`,
		user1,
	); err != nil {
		t.Fatalf("insert valid identity: %v", err)
	}

	tests := []struct {
		name    string
		query   string
		args    []any
		wantErr string
	}{
		{
			name:    "duplicate issuer and subject",
			query:   `INSERT INTO user_identities (user_id, issuer, subject) VALUES ($1, 'https://dex.local', 'sub-1')`,
			args:    []any{user2},
			wantErr: "uq_user_identities_issuer_subject",
		},
		{
			name:    "empty issuer",
			query:   `INSERT INTO user_identities (user_id, issuer, subject) VALUES ($1, '', 'sub-2')`,
			args:    []any{user2},
			wantErr: "ck_user_identities_issuer_length",
		},
		{
			name:    "empty subject",
			query:   `INSERT INTO user_identities (user_id, issuer, subject) VALUES ($1, 'https://dex.local', '')`,
			args:    []any{user2},
			wantErr: "ck_user_identities_subject_length",
		},
		{
			name:    "uppercase email",
			query:   `INSERT INTO user_identities (user_id, issuer, subject, email) VALUES ($1, 'https://dex.local', 'sub-3', 'Upper@example.com')`,
			args:    []any{user2},
			wantErr: "ck_user_identities_email",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := store.db.ExecContext(ctx, tt.query, tt.args...)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want constraint %q", err, tt.wantErr)
			}
		})
	}
}

func TestMigration007MembershipsAndSessionsConstraints(t *testing.T) {
	store, tenantID := newTestStore(t)
	ctx := testAuditCtx(context.Background())

	var userID int64
	if err := store.db.QueryRowContext(ctx,
		`INSERT INTO users (email, name) VALUES ('member@example.com', 'Bob') RETURNING id`,
	).Scan(&userID); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	if _, err := store.db.ExecContext(ctx,
		`INSERT INTO memberships (user_id, tenant_id, role) VALUES ($1, $2, 'member')`,
		userID, tenantID,
	); err != nil {
		t.Fatalf("insert membership: %v", err)
	}

	validHash := bytes.Repeat([]byte{0xab}, 32)
	now := time.Now().UTC().Truncate(time.Microsecond)
	if _, err := store.db.ExecContext(ctx,
		`INSERT INTO sessions (user_id, token_hash, created_at, expires_at) VALUES ($1, $2, $3, $4)`,
		userID, validHash, now, now.Add(time.Hour),
	); err != nil {
		t.Fatalf("insert session: %v", err)
	}

	tests := []struct {
		name    string
		query   string
		args    []any
		wantErr string
	}{
		{
			name:    "duplicate membership",
			query:   `INSERT INTO memberships (user_id, tenant_id, role) VALUES ($1, $2, 'admin')`,
			args:    []any{userID, tenantID},
			wantErr: "memberships_pkey",
		},
		{
			name:    "invalid role",
			query:   `INSERT INTO memberships (user_id, tenant_id, role) VALUES ($1, $2, 'superadmin')`,
			args:    []any{userID + 1, tenantID},
			wantErr: "ck_memberships_role",
		},
		{
			name:    "invalid session hash length",
			query:   `INSERT INTO sessions (user_id, token_hash, created_at, expires_at) VALUES ($1, $2, $3, $4)`,
			args:    []any{userID, []byte("short"), now, now.Add(time.Hour)},
			wantErr: "ck_sessions_token_hash_length",
		},
		{
			name:    "duplicate session hash",
			query:   `INSERT INTO sessions (user_id, token_hash, created_at, expires_at) VALUES ($1, $2, $3, $4)`,
			args:    []any{userID, validHash, now, now.Add(time.Hour)},
			wantErr: "sessions_token_hash_key",
		},
		{
			name:    "session expires not after created",
			query:   `INSERT INTO sessions (user_id, token_hash, created_at, expires_at) VALUES ($1, $2, $3, $4)`,
			args:    []any{userID, bytes.Repeat([]byte{0xcd}, 32), now, now},
			wantErr: "ck_sessions_expires_after_created",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := store.db.ExecContext(ctx, tt.query, tt.args...)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want constraint %q", err, tt.wantErr)
			}
		})
	}
}

func TestMigration007InvitationsConstraints(t *testing.T) {
	store, tenantID := newTestStore(t)
	ctx := testAuditCtx(context.Background())

	now := time.Now().UTC().Truncate(time.Microsecond)
	tests := []struct {
		name    string
		query   string
		args    []any
		wantErr string
	}{
		{
			name:    "uppercase email",
			query:   `INSERT INTO invitations (tenant_id, email, role) VALUES ($1, 'Invite@example.com', 'member')`,
			args:    []any{tenantID},
			wantErr: "ck_invitations_email",
		},
		{
			name:    "invalid role",
			query:   `INSERT INTO invitations (tenant_id, email, role) VALUES ($1, 'badrole@example.com', 'guest')`,
			args:    []any{tenantID},
			wantErr: "ck_invitations_role",
		},
		{
			name:    "accepted_at without accepted_by",
			query:   `INSERT INTO invitations (tenant_id, email, role, created_at, accepted_at) VALUES ($1, 'half@example.com', 'member', $2, $2)`,
			args:    []any{tenantID, now},
			wantErr: "ck_invitations_accepted_fields",
		},
		{
			name:    "accepted and revoked together",
			query:   `INSERT INTO invitations (tenant_id, email, role, created_at, accepted_at, accepted_by, revoked_at) VALUES ($1, 'both@example.com', 'member', $2, $2, 1, $2)`,
			args:    []any{tenantID, now},
			wantErr: "ck_invitations_state",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := store.db.ExecContext(ctx, tt.query, tt.args...)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want constraint %q", err, tt.wantErr)
			}
		})
	}

	var firstID int64
	if err := store.db.QueryRowContext(ctx,
		`INSERT INTO invitations (tenant_id, email, role) VALUES ($1, 'invitee@example.com', 'member') RETURNING id`,
		tenantID,
	).Scan(&firstID); err != nil {
		t.Fatalf("insert pending invitation: %v", err)
	}

	_, err := store.db.ExecContext(ctx,
		`INSERT INTO invitations (tenant_id, email, role) VALUES ($1, 'invitee@example.com', 'admin')`,
		tenantID,
	)
	if err == nil || !strings.Contains(err.Error(), "uq_invitations_pending_tenant_email") {
		t.Fatalf("duplicate pending invitation err = %v, want uq_invitations_pending_tenant_email", err)
	}

	// 撤销上一条待兑现邀请后，同一租户可以再次邀请该邮箱。
	if _, err := store.db.ExecContext(ctx,
		`UPDATE invitations SET revoked_at = now() WHERE id = $1`, firstID,
	); err != nil {
		t.Fatalf("revoke invitation: %v", err)
	}
	var secondID int64
	if err := store.db.QueryRowContext(ctx,
		`INSERT INTO invitations (tenant_id, email, role) VALUES ($1, 'invitee@example.com', 'admin') RETURNING id`,
		tenantID,
	).Scan(&secondID); err != nil {
		t.Fatalf("re-invite after revoke: %v", err)
	}

	// 已兑现的邀请也允许之后再次邀请。
	if _, err := store.db.ExecContext(ctx,
		`UPDATE invitations SET accepted_at = now(), accepted_by = 42 WHERE id = $1`, secondID,
	); err != nil {
		t.Fatalf("accept invitation: %v", err)
	}
	if _, err := store.db.ExecContext(ctx,
		`INSERT INTO invitations (tenant_id, email, role) VALUES ($1, 'invitee@example.com', 'viewer')`,
		tenantID,
	); err != nil {
		t.Fatalf("re-invite after accept: %v", err)
	}
}

func TestMigration010IndexesAndConstraints(t *testing.T) {
	store, tenantID := newTestStore(t)
	ctx := testAuditCtx(context.Background())

	for _, idx := range []string{
		"idx_sessions_revoked_at",
		"idx_jobs_pending_tenant_run",
		"idx_jobs_pending_kind_run",
		"idx_memberships_tenant_owners",
	} {
		var exists bool
		if err := store.db.QueryRowContext(ctx,
			`SELECT EXISTS (SELECT 1 FROM pg_indexes WHERE indexname = $1)`, idx,
		).Scan(&exists); err != nil {
			t.Fatalf("check index %s: %v", idx, err)
		}
		if !exists {
			t.Errorf("missing expected migration 010 index %s", idx)
		}
	}

	now := time.Now().UTC().Truncate(time.Microsecond)
	tests := []struct {
		name    string
		query   string
		args    []any
		wantErr string
	}{
		{
			name:    "api key hash length",
			query:   `INSERT INTO api_keys (tenant_id, key_hash, scopes) VALUES ($1, $2, ARRAY['links:read']::text[])`,
			args:    []any{tenantID, []byte("too-short")},
			wantErr: "ck_api_keys_hash_length",
		},
		{
			name:    "blank tenant name",
			query:   `INSERT INTO tenants (name) VALUES ('   ')`,
			wantErr: "ck_tenants_name_length",
		},
		{
			name:    "overlong tenant name",
			query:   `INSERT INTO tenants (name) VALUES ($1)`,
			args:    []any{strings.Repeat("a", 101)},
			wantErr: "ck_tenants_name_length",
		},
		{
			name:    "invitation revoked and superseded together",
			query:   `INSERT INTO invitations (tenant_id, email, role, created_at, revoked_at, superseded_at) VALUES ($1, 'dupstate@example.com', 'member', $2, $2, $2)`,
			args:    []any{tenantID, now},
			wantErr: "ck_invitations_state",
		},
		{
			name:    "negative job lock_version",
			query:   `INSERT INTO jobs (kind, lock_version) VALUES ('link.risk_scan', -1)`,
			wantErr: "ck_jobs_lock_version_nonnegative",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := store.db.ExecContext(ctx, tt.query, tt.args...)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want constraint %q", err, tt.wantErr)
			}
		})
	}
}
