package identity_test

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"testing"

	"shortlink/internal/identity"
	"shortlink/internal/identity/fakegithub"
)

func newTestGitHubProvider(t *testing.T, fake *fakegithub.Server, redirectURL, clientSecret string) *identity.GitHubProvider {
	t.Helper()
	p, err := identity.NewGitHubProvider(identity.GitHubConfig{
		ClientID:     "gh-client",
		ClientSecret: clientSecret,
		RedirectURL:  redirectURL,
		AuthURL:      fake.AuthURL(),
		TokenURL:     fake.TokenURL(),
		APIBaseURL:   fake.APIBaseURL(),
		HTTPClient:   fake.Client(),
	})
	if err != nil {
		t.Fatalf("NewGitHubProvider: %v", err)
	}
	return p
}

func mustOAuthFlow(t *testing.T) identity.OAuthFlow {
	t.Helper()
	flow, err := identity.NewOAuthFlow()
	if err != nil {
		t.Fatalf("NewOAuthFlow: %v", err)
	}
	return flow
}

func TestNewGitHubProviderValidation(t *testing.T) {
	valid := identity.GitHubConfig{
		ClientID:     "gh-client",
		ClientSecret: "gh-secret",
		RedirectURL:  "http://127.0.0.1:8081/api/v1/auth/github/callback",
	}
	p, err := identity.NewGitHubProvider(valid)
	if err != nil {
		t.Fatalf("NewGitHubProvider valid defaults: %v", err)
	}
	if p.SecureCookies() {
		t.Error("SecureCookies() = true for http redirect, want false")
	}

	cases := []struct {
		name   string
		mutate func(*identity.GitHubConfig)
	}{
		{name: "missing client id", mutate: func(c *identity.GitHubConfig) { c.ClientID = "" }},
		{name: "missing client secret", mutate: func(c *identity.GitHubConfig) { c.ClientSecret = "" }},
		{name: "missing redirect url", mutate: func(c *identity.GitHubConfig) { c.RedirectURL = "" }},
		{name: "relative redirect url", mutate: func(c *identity.GitHubConfig) { c.RedirectURL = "/callback" }},
		{name: "invalid auth url", mutate: func(c *identity.GitHubConfig) { c.AuthURL = "ftp://github.com/auth" }},
		{name: "token url with query", mutate: func(c *identity.GitHubConfig) { c.TokenURL = "https://github.com/token?a=1" }},
		{name: "invalid api base url", mutate: func(c *identity.GitHubConfig) { c.APIBaseURL = "://bad" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := valid
			tc.mutate(&cfg)
			if _, err := identity.NewGitHubProvider(cfg); err == nil {
				t.Fatal("NewGitHubProvider = nil, want error")
			}
		})
	}
}

func TestGitHubProviderHappyPathAndEmailRules(t *testing.T) {
	fake := fakegithub.Start(t, "gh-client", "gh-secret")
	httpProvider := newTestGitHubProvider(t, fake, "http://127.0.0.1:8081/api/v1/auth/github/callback", "gh-secret")
	httpsProvider := newTestGitHubProvider(t, fake, "https://console.example.com/api/v1/auth/github/callback", "gh-secret")
	if !httpsProvider.SecureCookies() {
		t.Error("SecureCookies() = false for https redirect, want true")
	}

	t.Run("primary verified email and numeric subject", func(t *testing.T) {
		fake.SetUser(fakegithub.User{
			ID:    424242,
			Login: "alice-gh",
			Name:  "  Alice GitHub  ",
			Emails: []fakegithub.Email{
				{Email: "secondary@example.com", Primary: false, Verified: true},
				{Email: "  Alice@Example.COM ", Primary: true, Verified: true},
			},
		})
		flow := mustOAuthFlow(t)
		authURL := httpProvider.AuthCodeURL(flow.State, flow.CodeVerifier)
		parsed, err := url.Parse(authURL)
		if err != nil {
			t.Fatalf("parse AuthCodeURL: %v", err)
		}
		if parsed.Query().Has("nonce") {
			t.Errorf("GitHub AuthCodeURL unexpectedly set nonce: %s", authURL)
		}

		code, gotState := authorizeAtFake(t, authURL)
		if gotState != flow.State {
			t.Fatalf("state = %q, want %q", gotState, flow.State)
		}
		req, ok := fake.LastAuthorize()
		if !ok || req.CodeChallenge == "" || req.CodeChallengeMethod != "S256" {
			t.Fatalf("LastAuthorize = %+v (ok=%v), want PKCE S256", req, ok)
		}

		got, err := httpProvider.ExchangeAndFetchUser(context.Background(), code, flow.CodeVerifier)
		if err != nil {
			t.Fatalf("ExchangeAndFetchUser: %v", err)
		}
		want := identity.ExternalIdentity{
			Issuer:        identity.GitHubIssuer,
			Subject:       "424242",
			Email:         "alice@example.com",
			EmailVerified: true,
			Name:          "Alice GitHub",
		}
		if got != want {
			t.Errorf("identity = %+v, want %+v", got, want)
		}
	})

	t.Run("empty name falls back to login", func(t *testing.T) {
		fake.SetUser(fakegithub.User{
			ID:     77,
			Login:  "no-name-user",
			Name:   "   ",
			Emails: []fakegithub.Email{{Email: "u@example.com", Primary: true, Verified: true}},
		})
		flow := mustOAuthFlow(t)
		code, _ := authorizeAtFake(t, httpProvider.AuthCodeURL(flow.State, flow.CodeVerifier))
		got, err := httpProvider.ExchangeAndFetchUser(context.Background(), code, flow.CodeVerifier)
		if err != nil {
			t.Fatalf("ExchangeAndFetchUser: %v", err)
		}
		if got.Name != "no-name-user" {
			t.Errorf("Name = %q, want login fallback no-name-user", got.Name)
		}
	})

	t.Run("unverified primary email does not promote verified secondary", func(t *testing.T) {
		fake.SetUser(fakegithub.User{
			ID:    88,
			Login: "unverified-primary",
			Emails: []fakegithub.Email{
				{Email: "verified-secondary@example.com", Primary: false, Verified: true},
				{Email: "unverified-primary@example.com", Primary: true, Verified: false},
			},
		})
		flow := mustOAuthFlow(t)
		code, _ := authorizeAtFake(t, httpProvider.AuthCodeURL(flow.State, flow.CodeVerifier))
		got, err := httpProvider.ExchangeAndFetchUser(context.Background(), code, flow.CodeVerifier)
		if err != nil {
			t.Fatalf("ExchangeAndFetchUser: %v", err)
		}
		if got.Email != "unverified-primary@example.com" || got.EmailVerified {
			t.Errorf("got = (%q, verified=%v), want unverified primary email", got.Email, got.EmailVerified)
		}
	})

	t.Run("no primary email leaves email empty and unverified", func(t *testing.T) {
		fake.SetUser(fakegithub.User{
			ID:    99,
			Login: "no-primary",
			Emails: []fakegithub.Email{
				{Email: "verified-secondary@example.com", Primary: false, Verified: true},
			},
		})
		flow := mustOAuthFlow(t)
		code, _ := authorizeAtFake(t, httpProvider.AuthCodeURL(flow.State, flow.CodeVerifier))
		got, err := httpProvider.ExchangeAndFetchUser(context.Background(), code, flow.CodeVerifier)
		if err != nil {
			t.Fatalf("ExchangeAndFetchUser: %v", err)
		}
		if got.Email != "" || got.EmailVerified {
			t.Errorf("got = (%q, verified=%v), want empty unverified email", got.Email, got.EmailVerified)
		}
	})
}

func TestGitHubProviderClassifiesErrors(t *testing.T) {
	t.Run("http 200 auth and outage errors", func(t *testing.T) {
		fake := fakegithub.Start(t, "gh-client", "gh-secret")
		provider := newTestGitHubProvider(t, fake, "http://127.0.0.1:8081/api/v1/auth/github/callback", "gh-secret")

		zeroID := int64(0)
		tests := []struct {
			name    string
			tamper  fakegithub.Tamper
			wantErr error
		}{
			{name: "expired code", tamper: fakegithub.Tamper{CodeExpired: true}, wantErr: identity.ErrAuthFailed},
			{name: "bad verification code", tamper: fakegithub.Tamper{TokenError: "bad_verification_code"}, wantErr: identity.ErrAuthFailed},
			{name: "unsupported grant type", tamper: fakegithub.Tamper{TokenError: "unsupported_grant_type"}, wantErr: identity.ErrProviderUnavailable},
			{name: "incorrect client credentials", tamper: fakegithub.Tamper{TokenError: "incorrect_client_credentials"}, wantErr: identity.ErrProviderUnavailable},
			{name: "redirect uri mismatch", tamper: fakegithub.Tamper{TokenError: "redirect_uri_mismatch"}, wantErr: identity.ErrProviderUnavailable},
			{name: "token endpoint 500", tamper: fakegithub.Tamper{TokenError: "server_error", TokenStatus: http.StatusInternalServerError}, wantErr: identity.ErrProviderUnavailable},
			{name: "empty access token", tamper: fakegithub.Tamper{EmptyAccessToken: true}, wantErr: identity.ErrProviderUnavailable},
			{name: "user api 401", tamper: fakegithub.Tamper{UserStatus: http.StatusUnauthorized}, wantErr: identity.ErrAuthFailed},
			{name: "user api invalid id", tamper: fakegithub.Tamper{UserIDOverride: &zeroID}, wantErr: identity.ErrAuthFailed},
			{name: "user api 500", tamper: fakegithub.Tamper{UserStatus: http.StatusInternalServerError}, wantErr: identity.ErrProviderUnavailable},
			{name: "emails api 403", tamper: fakegithub.Tamper{EmailsStatus: http.StatusForbidden}, wantErr: identity.ErrProviderUnavailable},
			{name: "emails api 500", tamper: fakegithub.Tamper{EmailsStatus: http.StatusInternalServerError}, wantErr: identity.ErrProviderUnavailable},
		}

		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				fake.SetTamper(tc.tamper)
				t.Cleanup(func() { fake.SetTamper(fakegithub.Tamper{}) })

				flow := mustOAuthFlow(t)
				code, _ := authorizeAtFake(t, provider.AuthCodeURL(flow.State, flow.CodeVerifier))
				_, err := provider.ExchangeAndFetchUser(context.Background(), code, flow.CodeVerifier)
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
			})
		}
	})

	t.Run("wrong client secret returns ErrProviderUnavailable", func(t *testing.T) {
		fake := fakegithub.Start(t, "gh-client", "real-secret")
		provider := newTestGitHubProvider(t, fake, "http://127.0.0.1:8081/api/v1/auth/github/callback", "wrong-secret")

		flow := mustOAuthFlow(t)
		code, _ := authorizeAtFake(t, provider.AuthCodeURL(flow.State, flow.CodeVerifier))
		_, err := provider.ExchangeAndFetchUser(context.Background(), code, flow.CodeVerifier)
		if !errors.Is(err, identity.ErrProviderUnavailable) {
			t.Fatalf("err = %v, want ErrProviderUnavailable", err)
		}
	})

	t.Run("wrong PKCE code_verifier returns ErrAuthFailed", func(t *testing.T) {
		fake := fakegithub.Start(t, "gh-client", "gh-secret")
		provider := newTestGitHubProvider(t, fake, "http://127.0.0.1:8081/api/v1/auth/github/callback", "gh-secret")

		flow := mustOAuthFlow(t)
		code, _ := authorizeAtFake(t, provider.AuthCodeURL(flow.State, flow.CodeVerifier))
		_, err := provider.ExchangeAndFetchUser(context.Background(), code, "wrong-verifier-value-that-does-not-match")
		if !errors.Is(err, identity.ErrAuthFailed) {
			t.Fatalf("err = %v, want ErrAuthFailed", err)
		}
	})

	t.Run("empty params and canceled context", func(t *testing.T) {
		fake := fakegithub.Start(t, "gh-client", "gh-secret")
		provider := newTestGitHubProvider(t, fake, "http://127.0.0.1:8081/api/v1/auth/github/callback", "gh-secret")

		if _, err := provider.ExchangeAndFetchUser(context.Background(), "", "v"); !errors.Is(err, identity.ErrAuthFailed) {
			t.Fatalf("empty code err = %v, want ErrAuthFailed", err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := provider.ExchangeAndFetchUser(ctx, "c", "v"); !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled ctx err = %v, want context.Canceled", err)
		}
	})
}
