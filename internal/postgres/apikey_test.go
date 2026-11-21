package postgres

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"shortlink/internal/identity"
	"shortlink/internal/tenant"
)

func TestIssueAPIKeyRejectsBadParams(t *testing.T) {
	store := &Store{}
	future := time.Now().Add(time.Hour)
	past := time.Now().Add(-time.Minute)
	tests := []struct {
		name    string
		params  IssueAPIKeyParams
		wantErr error
	}{
		{name: "neither", wantErr: ErrIssueTarget},
		{name: "both", params: IssueAPIKeyParams{TenantID: 1, Name: "acme"}, wantErr: ErrIssueTarget},
		{name: "blank name", params: IssueAPIKeyParams{Name: "  "}, wantErr: ErrIssueTarget},
		{name: "negative id", params: IssueAPIKeyParams{TenantID: -1}, wantErr: ErrIssueTarget},
		{
			name:    "external id on existing",
			params:  IssueAPIKeyParams{TenantID: 1, ExternalID: "ext"},
			wantErr: ErrExternalIDOnExisting,
		},
		{
			name:    "past expiry",
			params:  IssueAPIKeyParams{Name: "acme", ExpiresAt: &past},
			wantErr: ErrExpiresNotFuture,
		},
		{
			name:    "nil scopes",
			params:  IssueAPIKeyParams{Name: "acme"},
			wantErr: identity.ErrInvalidAPIKeyScope,
		},
		{
			name:    "empty scopes slice",
			params:  IssueAPIKeyParams{Name: "acme", Scopes: []identity.Perm{}},
			wantErr: identity.ErrInvalidAPIKeyScope,
		},
		{
			name:    "unknown scope",
			params:  IssueAPIKeyParams{Name: "acme", Scopes: []identity.Perm{"links:delete"}},
			wantErr: identity.ErrInvalidAPIKeyScope,
		},
		{
			name:    "member scope not allowed on api key",
			params:  IssueAPIKeyParams{Name: "acme", Scopes: []identity.Perm{identity.PermLinksRead, identity.PermMembersWrite}},
			wantErr: identity.ErrInvalidAPIKeyScope,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := store.IssueAPIKey(context.Background(), tt.params)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("IssueAPIKey error = %v, want %v", err, tt.wantErr)
			}
		})
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := store.IssueAPIKey(ctx, IssueAPIKeyParams{Name: "acme", ExpiresAt: &future, Scopes: identity.AllAPIKeyScopes()})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled IssueAPIKey error = %v, want context.Canceled", err)
	}
	if _, err := store.ListAPIKeys(ctx, 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled ListAPIKeys error = %v, want context.Canceled", err)
	}
	if err := store.RevokeAPIKey(ctx, 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled RevokeAPIKey error = %v, want context.Canceled", err)
	}
}

func TestIssueListRevokeAPIKey(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()
	future := time.Now().Add(time.Hour).UTC().Truncate(time.Microsecond)

	issued, err := store.IssueAPIKey(ctx, IssueAPIKeyParams{
		Name:       "  acme  ",
		ExternalID: " platform-1 ",
		ExpiresAt:  &future,
		Scopes:     identity.AllAPIKeyScopes(),
	})
	if err != nil {
		t.Fatalf("IssueAPIKey: %v", err)
	}
	if !tenant.ValidToken(issued.Plaintext) {
		t.Fatalf("plaintext %q is not a valid token", issued.Plaintext)
	}
	if issued.Tenant.ExternalID != "platform-1" {
		t.Errorf("ExternalID = %q, want platform-1", issued.Tenant.ExternalID)
	}
	if issued.Key.ExpiresAt == nil || !issued.Key.ExpiresAt.Equal(future) {
		t.Fatalf("ExpiresAt = %v, want %v", issued.Key.ExpiresAt, future)
	}
	if !slices.Equal(issued.Key.Scopes, identity.AllAPIKeyScopes()) {
		t.Fatalf("issued Scopes = %v, want %v", issued.Key.Scopes, identity.AllAPIKeyScopes())
	}

	var name string
	if err := store.db.QueryRowContext(ctx, `SELECT name FROM tenants WHERE id = $1`, issued.Tenant.ID).Scan(&name); err != nil {
		t.Fatalf("select tenant: %v", err)
	}
	if name != "acme" {
		t.Errorf("tenant name = %q, want acme", name)
	}

	got, err := store.Authenticate(ctx, issued.Plaintext)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if got.Tenant.ID != issued.Tenant.ID || got.Tenant.ExternalID != "platform-1" ||
		got.ActorKind != identity.ActorAPIKey || got.ActorID != issued.Key.ID ||
		!slices.Equal(got.Scopes, identity.AllAPIKeyScopes()) {
		t.Errorf("Authenticate = %+v, want tenant %d external platform-1 key %d with all scopes", got, issued.Tenant.ID, issued.Key.ID)
	}

	second, err := store.IssueAPIKey(ctx, IssueAPIKeyParams{
		TenantID: issued.Tenant.ID,
		Scopes:   []identity.Perm{identity.PermStatsRead, identity.PermLinksRead, identity.PermStatsRead},
	})
	if err != nil {
		t.Fatalf("IssueAPIKey existing tenant: %v", err)
	}
	if second.Key.ExpiresAt != nil {
		t.Errorf("second ExpiresAt = %v, want nil", second.Key.ExpiresAt)
	}
	wantSecondScopes := []identity.Perm{identity.PermLinksRead, identity.PermStatsRead}
	if !slices.Equal(second.Key.Scopes, wantSecondScopes) {
		t.Fatalf("second Scopes = %v, want %v", second.Key.Scopes, wantSecondScopes)
	}
	gotSecond, err := store.Authenticate(ctx, second.Plaintext)
	if err != nil {
		t.Fatalf("Authenticate second: %v", err)
	}
	if gotSecond.ActorID != second.Key.ID || !slices.Equal(gotSecond.Scopes, wantSecondScopes) {
		t.Errorf("Authenticate second = %+v, want actor %d scopes %v", gotSecond, second.Key.ID, wantSecondScopes)
	}

	keys, err := store.ListAPIKeys(ctx, issued.Tenant.ID)
	if err != nil {
		t.Fatalf("ListAPIKeys: %v", err)
	}
	if len(keys) != 2 || keys[0].ID != issued.Key.ID || keys[1].ID != second.Key.ID {
		t.Fatalf("ListAPIKeys = %+v, want both keys in id order", keys)
	}
	if !slices.Equal(keys[0].Scopes, identity.AllAPIKeyScopes()) {
		t.Errorf("first key scopes = %v, want %v", keys[0].Scopes, identity.AllAPIKeyScopes())
	}
	if !slices.Equal(keys[1].Scopes, wantSecondScopes) {
		t.Errorf("second key scopes = %v, want %v", keys[1].Scopes, wantSecondScopes)
	}
	if keys[1].ExpiresAt != nil || keys[1].RevokedAt != nil {
		t.Errorf("second key state = %+v, want no expiry and not revoked", keys[1])
	}

	if _, err := store.db.ExecContext(ctx,
		`UPDATE api_keys SET expires_at = now() - interval '1 second' WHERE id = $1`, issued.Key.ID); err != nil {
		t.Fatalf("expire key: %v", err)
	}
	if _, err := store.Authenticate(ctx, issued.Plaintext); !errors.Is(err, tenant.ErrUnauthenticated) {
		t.Fatalf("expired Authenticate error = %v, want ErrUnauthenticated", err)
	}

	if err := store.RevokeAPIKey(ctx, second.Key.ID); err != nil {
		t.Fatalf("RevokeAPIKey: %v", err)
	}
	if _, err := store.Authenticate(ctx, second.Plaintext); !errors.Is(err, tenant.ErrUnauthenticated) {
		t.Fatalf("revoked Authenticate error = %v, want ErrUnauthenticated", err)
	}
	if err := store.RevokeAPIKey(ctx, second.Key.ID); !errors.Is(err, ErrAPIKeyRevoked) {
		t.Fatalf("second revoke error = %v, want ErrAPIKeyRevoked", err)
	}
	if err := store.RevokeAPIKey(ctx, second.Key.ID+999_999); !errors.Is(err, ErrAPIKeyNotFound) {
		t.Fatalf("missing revoke error = %v, want ErrAPIKeyNotFound", err)
	}

	keys, err = store.ListAPIKeys(ctx, issued.Tenant.ID)
	if err != nil {
		t.Fatalf("ListAPIKeys after revoke: %v", err)
	}
	if keys[1].RevokedAt == nil {
		t.Fatal("listed revoked key has nil RevokedAt")
	}

	if _, err := store.ListAPIKeys(ctx, issued.Tenant.ID+999_999); !errors.Is(err, ErrTenantNotFound) {
		t.Fatalf("list missing tenant error = %v, want ErrTenantNotFound", err)
	}
	if _, err := store.IssueAPIKey(ctx, IssueAPIKeyParams{
		TenantID: issued.Tenant.ID + 999_999,
		Scopes:   identity.AllAPIKeyScopes(),
	}); !errors.Is(err, ErrTenantNotFound) {
		t.Fatalf("issue missing tenant error = %v, want ErrTenantNotFound", err)
	}

	_, err = store.IssueAPIKey(ctx, IssueAPIKeyParams{
		Name:       "other",
		ExternalID: "platform-1",
		Scopes:     identity.AllAPIKeyScopes(),
	})
	if !isUniqueViolation(err) {
		t.Fatalf("duplicate external id error = %v, want unique violation", err)
	}
	if strings.Contains(err.Error(), issued.Plaintext) {
		t.Fatal("error contains plaintext")
	}
}
