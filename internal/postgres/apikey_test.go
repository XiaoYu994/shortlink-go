package postgres

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

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
	_, err := store.IssueAPIKey(ctx, IssueAPIKeyParams{Name: "acme", ExpiresAt: &future})
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
	if got.ID != issued.Tenant.ID || got.ExternalID != "platform-1" {
		t.Errorf("Authenticate = %+v, want tenant %d external platform-1", got, issued.Tenant.ID)
	}

	second, err := store.IssueAPIKey(ctx, IssueAPIKeyParams{TenantID: issued.Tenant.ID})
	if err != nil {
		t.Fatalf("IssueAPIKey existing tenant: %v", err)
	}
	if second.Key.ExpiresAt != nil {
		t.Errorf("second ExpiresAt = %v, want nil", second.Key.ExpiresAt)
	}
	if _, err := store.Authenticate(ctx, second.Plaintext); err != nil {
		t.Fatalf("Authenticate second: %v", err)
	}

	keys, err := store.ListAPIKeys(ctx, issued.Tenant.ID)
	if err != nil {
		t.Fatalf("ListAPIKeys: %v", err)
	}
	if len(keys) != 2 || keys[0].ID != issued.Key.ID || keys[1].ID != second.Key.ID {
		t.Fatalf("ListAPIKeys = %+v, want both keys in id order", keys)
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
	if _, err := store.IssueAPIKey(ctx, IssueAPIKeyParams{TenantID: issued.Tenant.ID + 999_999}); !errors.Is(err, ErrTenantNotFound) {
		t.Fatalf("issue missing tenant error = %v, want ErrTenantNotFound", err)
	}

	_, err = store.IssueAPIKey(ctx, IssueAPIKeyParams{Name: "other", ExternalID: "platform-1"})
	if !isUniqueViolation(err) {
		t.Fatalf("duplicate external id error = %v, want unique violation", err)
	}
	if strings.Contains(err.Error(), issued.Plaintext) {
		t.Fatal("error contains plaintext")
	}
}
