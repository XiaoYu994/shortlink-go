package identity_test

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"testing"

	"shortlink/internal/identity"
	"shortlink/internal/identity/fakeoidc"
)

func authorizeAtFake(t *testing.T, authURL string) (code, state string) {
	t.Helper()
	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Get(authURL)
	if err != nil {
		t.Fatalf("GET authorize: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("authorize status = %d, want 302", resp.StatusCode)
	}
	loc, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		t.Fatalf("parse Location: %v", err)
	}
	return loc.Query().Get("code"), loc.Query().Get("state")
}

func TestOIDCProviderHappyPathAndSecureCookies(t *testing.T) {
	fake := fakeoidc.Start(t, "shortlink-console", "secret+value")
	fake.SetUser(fakeoidc.User{
		Subject:       "sub-alice",
		Email:         " Alice@Example.COM ",
		EmailVerified: true,
		Name:          " Alice ",
	})

	httpProvider, err := identity.NewOIDCProvider(context.Background(), identity.OIDCConfig{
		Issuer:       fake.URL(),
		ClientID:     "shortlink-console",
		ClientSecret: "secret+value",
		RedirectURL:  "http://127.0.0.1:8081/api/v1/auth/callback",
		HTTPClient:   fake.Client(),
	})
	if err != nil {
		t.Fatalf("NewOIDCProvider http: %v", err)
	}
	if httpProvider.SecureCookies() {
		t.Error("SecureCookies() = true for http redirect url, want false")
	}

	httpsProvider, err := identity.NewOIDCProvider(context.Background(), identity.OIDCConfig{
		Issuer:       fake.URL(),
		ClientID:     "shortlink-console",
		ClientSecret: "secret+value",
		RedirectURL:  "https://console.example.com/api/v1/auth/callback",
		HTTPClient:   fake.Client(),
	})
	if err != nil {
		t.Fatalf("NewOIDCProvider https: %v", err)
	}
	if !httpsProvider.SecureCookies() {
		t.Error("SecureCookies() = false for https redirect url, want true")
	}

	flow, err := identity.NewOIDCFlow()
	if err != nil {
		t.Fatalf("NewOIDCFlow: %v", err)
	}
	authURL := httpProvider.AuthCodeURL(flow.State, flow.CodeVerifier, flow.Nonce)
	code, gotState := authorizeAtFake(t, authURL)
	if gotState != flow.State {
		t.Fatalf("state = %q, want %q", gotState, flow.State)
	}

	got, err := httpProvider.ExchangeAndVerify(context.Background(), code, flow.CodeVerifier, flow.Nonce)
	if err != nil {
		t.Fatalf("ExchangeAndVerify: %v", err)
	}
	want := identity.ExternalIdentity{
		Issuer:        fake.URL(),
		Subject:       "sub-alice",
		Email:         "alice@example.com",
		EmailVerified: true,
		Name:          "Alice",
	}
	if got != want {
		t.Errorf("identity = %+v, want %+v", got, want)
	}
}

func TestOIDCProviderRejectsTamperedResponses(t *testing.T) {
	fake := fakeoidc.Start(t, "shortlink-console", "shortlink-console-secret")
	provider, err := identity.NewOIDCProvider(context.Background(), identity.OIDCConfig{
		Issuer:       fake.URL(),
		ClientID:     "shortlink-console",
		ClientSecret: "shortlink-console-secret",
		RedirectURL:  "http://127.0.0.1:8081/api/v1/auth/callback",
		HTTPClient:   fake.Client(),
	})
	if err != nil {
		t.Fatalf("NewOIDCProvider: %v", err)
	}

	tests := []struct {
		name    string
		tamper  fakeoidc.Tamper
		wantErr error
	}{
		{name: "expired code", tamper: fakeoidc.Tamper{CodeExpired: true}, wantErr: identity.ErrAuthFailed},
		{name: "wrong signature", tamper: fakeoidc.Tamper{WrongSignature: true}, wantErr: identity.ErrAuthFailed},
		{name: "wrong issuer", tamper: fakeoidc.Tamper{Issuer: "https://evil.example.com"}, wantErr: identity.ErrAuthFailed},
		{name: "wrong audience", tamper: fakeoidc.Tamper{Audience: "other-client"}, wantErr: identity.ErrAuthFailed},
		{name: "expired id token", tamper: fakeoidc.Tamper{ExpiredIDToken: true}, wantErr: identity.ErrAuthFailed},
		{name: "wrong nonce", tamper: fakeoidc.Tamper{Nonce: "different-nonce"}, wantErr: identity.ErrNonceMismatch},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake.SetTamper(tt.tamper)
			t.Cleanup(func() { fake.SetTamper(fakeoidc.Tamper{}) })

			flow, err := identity.NewOIDCFlow()
			if err != nil {
				t.Fatalf("NewOIDCFlow: %v", err)
			}
			code, _ := authorizeAtFake(t, provider.AuthCodeURL(flow.State, flow.CodeVerifier, flow.Nonce))
			_, err = provider.ExchangeAndVerify(context.Background(), code, flow.CodeVerifier, flow.Nonce)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if !errors.Is(err, identity.ErrAuthFailed) {
				t.Errorf("err = %v, want also wrapping ErrAuthFailed", err)
			}
		})
	}
}

func TestOIDCProviderWrongClientSecretReturnsUnavailable(t *testing.T) {
	fake := fakeoidc.Start(t, "shortlink-console", "real-secret")
	provider, err := identity.NewOIDCProvider(context.Background(), identity.OIDCConfig{
		Issuer:       fake.URL(),
		ClientID:     "shortlink-console",
		ClientSecret: "wrong-secret",
		RedirectURL:  "http://127.0.0.1:8081/api/v1/auth/callback",
		HTTPClient:   fake.Client(),
	})
	if err != nil {
		t.Fatalf("NewOIDCProvider: %v", err)
	}
	flow, err := identity.NewOIDCFlow()
	if err != nil {
		t.Fatalf("NewOIDCFlow: %v", err)
	}
	code, _ := authorizeAtFake(t, provider.AuthCodeURL(flow.State, flow.CodeVerifier, flow.Nonce))
	_, err = provider.ExchangeAndVerify(context.Background(), code, flow.CodeVerifier, flow.Nonce)
	if !errors.Is(err, identity.ErrProviderUnavailable) {
		t.Fatalf("err = %v, want ErrProviderUnavailable", err)
	}
}

func TestOIDCProviderClassifiesProviderOutages(t *testing.T) {
	tests := []struct {
		name    string
		tamper  fakeoidc.Tamper
		wantErr error
	}{
		{name: "token temporarily unavailable", tamper: fakeoidc.Tamper{TokenError: "temporarily_unavailable"}, wantErr: identity.ErrProviderUnavailable},
		{name: "token server error", tamper: fakeoidc.Tamper{TokenError: "server_error"}, wantErr: identity.ErrProviderUnavailable},
		{name: "jwks unavailable", tamper: fakeoidc.Tamper{JWKSFailure: true}, wantErr: identity.ErrProviderUnavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := fakeoidc.Start(t, "shortlink-console", "shortlink-console-secret")
			provider, err := identity.NewOIDCProvider(context.Background(), identity.OIDCConfig{
				Issuer:       fake.URL(),
				ClientID:     "shortlink-console",
				ClientSecret: "shortlink-console-secret",
				RedirectURL:  "http://127.0.0.1:8081/api/v1/auth/callback",
				HTTPClient:   fake.Client(),
			})
			if err != nil {
				t.Fatalf("NewOIDCProvider: %v", err)
			}
			fake.SetTamper(tt.tamper)
			flow, err := identity.NewOIDCFlow()
			if err != nil {
				t.Fatalf("NewOIDCFlow: %v", err)
			}
			code, _ := authorizeAtFake(t, provider.AuthCodeURL(flow.State, flow.CodeVerifier, flow.Nonce))
			_, err = provider.ExchangeAndVerify(context.Background(), code, flow.CodeVerifier, flow.Nonce)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if errors.Is(err, identity.ErrAuthFailed) {
				t.Errorf("outage was classified as authentication failure: %v", err)
			}
		})
	}
}
