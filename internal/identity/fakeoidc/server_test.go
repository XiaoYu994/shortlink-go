package fakeoidc

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

const (
	testClientID     = "shortlink-console"
	testClientSecret = "shortlink+secret/with=special"
	testRedirectURL  = "http://127.0.0.1:8081/api/v1/auth/callback"
)

type profileClaims struct {
	Email         string `json:"email"`
	EmailVerified bool   `json:"email_verified"`
	Name          string `json:"name"`
}

/*
authorizeThroughFake 发送一次不跟随重定向的 GET /authorize，
返回回调 Location 里的 code 和 state。
*/
func authorizeThroughFake(t *testing.T, authURL string) (code, state string, status int) {
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
		return "", "", resp.StatusCode
	}
	loc, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		t.Fatalf("parse redirect location: %v", err)
	}
	return loc.Query().Get("code"), loc.Query().Get("state"), resp.StatusCode
}

/*
TestEndToEndWithGoOIDC 用真实的 golang.org/x/oauth2 和 coreos/go-oidc/v3
走完发现文档、带 PKCE/state/nonce 的授权码获取、令牌交换和 ID Token 验签与声明提取。
同时覆盖含特殊字符的 client_secret 在 RFC 6749 Basic Auth 下的解码。
*/
func TestEndToEndWithGoOIDC(t *testing.T) {
	srv := Start(t, testClientID, testClientSecret)
	ctx := context.Background()

	provider, err := oidc.NewProvider(ctx, srv.URL())
	if err != nil {
		t.Fatalf("oidc.NewProvider: %v", err)
	}
	oauthCfg := oauth2.Config{
		ClientID:     testClientID,
		ClientSecret: testClientSecret,
		Endpoint:     provider.Endpoint(),
		RedirectURL:  testRedirectURL,
		Scopes:       []string{oidc.ScopeOpenID, "email", "profile"},
	}

	verifier := oauth2.GenerateVerifier()
	wantState := "state-123"
	wantNonce := "nonce-456"
	authURL := oauthCfg.AuthCodeURL(
		wantState,
		oauth2.S256ChallengeOption(verifier),
		oidc.Nonce(wantNonce),
	)

	code, gotState, status := authorizeThroughFake(t, authURL)
	if status != http.StatusFound {
		t.Fatalf("authorize status = %d, want 302", status)
	}
	if gotState != wantState {
		t.Fatalf("callback state = %q, want %q", gotState, wantState)
	}
	if code == "" {
		t.Fatal("empty authorization code")
	}

	lastReq, ok := srv.LastAuthorize()
	if !ok {
		t.Fatal("LastAuthorize not recorded")
	}
	if lastReq.State != wantState || lastReq.Nonce != wantNonce || lastReq.CodeChallengeMethod != "S256" || lastReq.CodeChallenge == "" {
		t.Fatalf("LastAuthorize = %+v, want state/nonce/PKCE S256", lastReq)
	}

	token, err := oauthCfg.Exchange(ctx, code, oauth2.VerifierOption(verifier))
	if err != nil {
		t.Fatalf("oauthCfg.Exchange: %v", err)
	}
	rawIDToken, ok := token.Extra("id_token").(string)
	if !ok || rawIDToken == "" {
		t.Fatalf("missing id_token in token response: %+v", token)
	}

	oidcVerifier := provider.Verifier(&oidc.Config{ClientID: testClientID})
	idToken, err := oidcVerifier.Verify(ctx, rawIDToken)
	if err != nil {
		t.Fatalf("oidcVerifier.Verify: %v", err)
	}
	if idToken.Subject != "08a8684b-db88-4b73-90a9-3cd1661f5466" {
		t.Errorf("Subject = %q, want alice subject", idToken.Subject)
	}
	if idToken.Nonce != wantNonce {
		t.Errorf("Nonce = %q, want %q", idToken.Nonce, wantNonce)
	}
	var claims profileClaims
	if err := idToken.Claims(&claims); err != nil {
		t.Fatalf("idToken.Claims: %v", err)
	}
	if claims.Email != "alice@example.com" || !claims.EmailVerified || claims.Name != "Alice" {
		t.Errorf("claims = %+v, want verified alice@example.com", claims)
	}

	// 同一个授权码是一次性的，重复兑换必须失败。
	if _, err := oauthCfg.Exchange(ctx, code, oauth2.VerifierOption(verifier)); err == nil {
		t.Fatal("reusing authorization code succeeded, want invalid_grant")
	}
}

func TestFaultInjectionAndValidation(t *testing.T) {
	srv := Start(t, testClientID, testClientSecret)
	ctx := context.Background()

	provider, err := oidc.NewProvider(ctx, srv.URL())
	if err != nil {
		t.Fatalf("oidc.NewProvider: %v", err)
	}
	oauthCfg := oauth2.Config{
		ClientID:     testClientID,
		ClientSecret: testClientSecret,
		Endpoint:     provider.Endpoint(),
		RedirectURL:  testRedirectURL,
		Scopes:       []string{oidc.ScopeOpenID, "email", "profile"},
	}
	oidcVerifier := provider.Verifier(&oidc.Config{ClientID: testClientID})

	t.Run("tampered state in callback", func(t *testing.T) {
		srv.SetTamper(Tamper{State: "tampered-state"})
		t.Cleanup(func() { srv.SetTamper(Tamper{}) })

		verifier := oauth2.GenerateVerifier()
		authURL := oauthCfg.AuthCodeURL("expected-state", oauth2.S256ChallengeOption(verifier), oidc.Nonce("n1"))
		_, gotState, status := authorizeThroughFake(t, authURL)
		if status != http.StatusFound {
			t.Fatalf("status = %d, want 302", status)
		}
		if gotState != "tampered-state" {
			t.Fatalf("gotState = %q, want tampered-state", gotState)
		}
	})

	t.Run("expired code rejected at token endpoint", func(t *testing.T) {
		srv.SetTamper(Tamper{CodeExpired: true})
		t.Cleanup(func() { srv.SetTamper(Tamper{}) })

		verifier := oauth2.GenerateVerifier()
		authURL := oauthCfg.AuthCodeURL("s1", oauth2.S256ChallengeOption(verifier), oidc.Nonce("n1"))
		code, _, _ := authorizeThroughFake(t, authURL)
		if _, err := oauthCfg.Exchange(ctx, code, oauth2.VerifierOption(verifier)); err == nil {
			t.Fatal("expired code exchange succeeded, want error")
		}
	})

	t.Run("wrong pkce verifier rejected at token endpoint", func(t *testing.T) {
		srv.SetTamper(Tamper{})
		verifier := oauth2.GenerateVerifier()
		authURL := oauthCfg.AuthCodeURL("s1", oauth2.S256ChallengeOption(verifier), oidc.Nonce("n1"))
		code, _, _ := authorizeThroughFake(t, authURL)

		wrongVerifier := oauth2.GenerateVerifier()
		if _, err := oauthCfg.Exchange(ctx, code, oauth2.VerifierOption(wrongVerifier)); err == nil {
			t.Fatal("exchange with wrong PKCE verifier succeeded, want error")
		}
	})

	t.Run("wrong client secret rejected at token endpoint", func(t *testing.T) {
		verifier := oauth2.GenerateVerifier()
		authURL := oauthCfg.AuthCodeURL("s1", oauth2.S256ChallengeOption(verifier), oidc.Nonce("n1"))
		code, _, _ := authorizeThroughFake(t, authURL)

		badSecretCfg := oauthCfg
		badSecretCfg.ClientSecret = "wrong-secret"
		if _, err := badSecretCfg.Exchange(ctx, code, oauth2.VerifierOption(verifier)); err == nil {
			t.Fatal("exchange with wrong client_secret succeeded, want 401 invalid_client")
		}
	})

	t.Run("redirect_uri mismatch rejected at token endpoint", func(t *testing.T) {
		verifier := oauth2.GenerateVerifier()
		authURL := oauthCfg.AuthCodeURL("s1", oauth2.S256ChallengeOption(verifier), oidc.Nonce("n1"))
		code, _, _ := authorizeThroughFake(t, authURL)

		wrongRedirectCfg := oauthCfg
		wrongRedirectCfg.RedirectURL = "http://127.0.0.1:9999/other/callback"
		if _, err := wrongRedirectCfg.Exchange(ctx, code, oauth2.VerifierOption(verifier)); err == nil {
			t.Fatal("exchange with mismatched redirect_uri succeeded, want error")
		}
	})

	t.Run("client_secret_post auth style accepted", func(t *testing.T) {
		postCfg := oauthCfg
		postCfg.Endpoint.AuthStyle = oauth2.AuthStyleInParams

		verifier := oauth2.GenerateVerifier()
		authURL := postCfg.AuthCodeURL("s-post", oauth2.S256ChallengeOption(verifier), oidc.Nonce("n-post"))
		code, _, _ := authorizeThroughFake(t, authURL)
		if _, err := postCfg.Exchange(ctx, code, oauth2.VerifierOption(verifier)); err != nil {
			t.Fatalf("client_secret_post Exchange: %v", err)
		}
	})

	tokenVerificationTests := []struct {
		name      string
		tamper    Tamper
		wantNonce string
		wantErr   string
	}{
		{
			name:      "wrong signature rejected by go-oidc",
			tamper:    Tamper{WrongSignature: true},
			wantNonce: "nonce-ok",
			wantErr:   "failed to verify signature",
		},
		{
			name:      "wrong audience rejected by go-oidc",
			tamper:    Tamper{Audience: "other-client"},
			wantNonce: "nonce-ok",
			wantErr:   "expected audience",
		},
		{
			name:      "wrong issuer rejected by go-oidc",
			tamper:    Tamper{Issuer: "https://evil.example.com"},
			wantNonce: "nonce-ok",
			wantErr:   "issued by a different provider",
		},
		{
			name:      "expired id token rejected by go-oidc",
			tamper:    Tamper{ExpiredIDToken: true},
			wantNonce: "nonce-ok",
			wantErr:   "token is expired",
		},
		{
			name:      "tampered nonce detected on idToken.Nonce",
			tamper:    Tamper{Nonce: "wrong-nonce"},
			wantNonce: "nonce-ok",
			wantErr:   "nonce mismatch",
		},
	}

	for _, tt := range tokenVerificationTests {
		t.Run(tt.name, func(t *testing.T) {
			srv.SetTamper(tt.tamper)
			t.Cleanup(func() { srv.SetTamper(Tamper{}) })

			verifier := oauth2.GenerateVerifier()
			authURL := oauthCfg.AuthCodeURL("state-1", oauth2.S256ChallengeOption(verifier), oidc.Nonce("nonce-ok"))
			code, _, status := authorizeThroughFake(t, authURL)
			if status != http.StatusFound {
				t.Fatalf("authorize status = %d, want 302", status)
			}
			token, err := oauthCfg.Exchange(ctx, code, oauth2.VerifierOption(verifier))
			if err != nil {
				t.Fatalf("Exchange: %v", err)
			}
			rawIDToken, _ := token.Extra("id_token").(string)

			/*
				go-oidc 的 Verify 只校验签名、iss、aud 和 exp，不校验 nonce。
				它把解析出的 nonce 放在 idToken.Nonce，真正的比对必须由 callback handler 执行。
			*/
			idToken, err := oidcVerifier.Verify(ctx, rawIDToken)
			if err == nil && idToken.Nonce != tt.wantNonce {
				err = errors.New("nonce mismatch")
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("verify err = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestAuthorizeRejectsInvalidRequests(t *testing.T) {
	srv := Start(t, testClientID, testClientSecret)

	tests := []struct {
		name  string
		query url.Values
	}{
		{
			name: "missing pkce challenge",
			query: url.Values{
				"response_type": {"code"},
				"client_id":     {testClientID},
				"redirect_uri":  {testRedirectURL},
				"scope":         {"openid email"},
				"state":         {"s"},
				"nonce":         {"n"},
			},
		},
		{
			name: "plain pkce method rejected",
			query: url.Values{
				"response_type":         {"code"},
				"client_id":             {testClientID},
				"redirect_uri":          {testRedirectURL},
				"scope":                 {"openid email"},
				"state":                 {"s"},
				"nonce":                 {"n"},
				"code_challenge":        {"abc"},
				"code_challenge_method": {"plain"},
			},
		},
		{
			name: "missing nonce",
			query: url.Values{
				"response_type":         {"code"},
				"client_id":             {testClientID},
				"redirect_uri":          {testRedirectURL},
				"scope":                 {"openid email"},
				"state":                 {"s"},
				"code_challenge":        {"abc"},
				"code_challenge_method": {"S256"},
			},
		},
		{
			name: "non-http redirect_uri rejected",
			query: url.Values{
				"response_type":         {"code"},
				"client_id":             {testClientID},
				"redirect_uri":          {"javascript://alert(1)"},
				"scope":                 {"openid"},
				"state":                 {"s"},
				"nonce":                 {"n"},
				"code_challenge":        {"abc"},
				"code_challenge_method": {"S256"},
			},
		},
		{
			name: "unknown client id",
			query: url.Values{
				"response_type":         {"code"},
				"client_id":             {"wrong-client"},
				"redirect_uri":          {testRedirectURL},
				"scope":                 {"openid"},
				"state":                 {"s"},
				"nonce":                 {"n"},
				"code_challenge":        {"abc"},
				"code_challenge_method": {"S256"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, status := authorizeThroughFake(t, srv.URL()+"/authorize?"+tt.query.Encode())
			if status != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", status)
			}
		})
	}
}

func TestExactRouteMatching(t *testing.T) {
	srv := Start(t, testClientID, testClientSecret)

	tests := []struct {
		name       string
		method     string
		path       string
		wantStatus int
	}{
		{name: "extra prefix before token rejected", method: http.MethodPost, path: "/xxx/token", wantStatus: http.StatusNotFound},
		{name: "extra prefix before jwks rejected", method: http.MethodGet, path: "/xxx/jwks", wantStatus: http.StatusNotFound},
		{name: "extra prefix before discovery rejected", method: http.MethodGet, path: "/xxx/.well-known/openid-configuration", wantStatus: http.StatusNotFound},
		{name: "wrong method on token rejected", method: http.MethodGet, path: "/token", wantStatus: http.StatusMethodNotAllowed},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := http.NewRequest(tt.method, srv.URL()+tt.path, nil)
			if err != nil {
				t.Fatalf("NewRequest: %v", err)
			}
			resp, err := srv.Client().Do(req)
			if err != nil {
				t.Fatalf("Do: %v", err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != tt.wantStatus {
				t.Errorf("%s %s = %d, want %d", tt.method, tt.path, resp.StatusCode, tt.wantStatus)
			}
		})
	}
}
