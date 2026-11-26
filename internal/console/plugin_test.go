package console

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"shortlink/internal/httpx"
	"shortlink/internal/identity"
	"shortlink/internal/link"
	"shortlink/internal/safehttp"
)

/*
stubPhoneOTPMethod 模拟未来的非跳转直连登录插件（如手机号短信验证码），
用来验证两层抽象允许零侵入扩展 POST 型登录方式并复用统一的 SessionIssuer 与 CSRF 底座。
*/
type stubPhoneOTPMethod struct {
	verifiedCalls int
}

func (m *stubPhoneOTPMethod) Descriptor() AuthMethodDescriptor {
	return AuthMethodDescriptor{
		ID:   "phone",
		Kind: AuthFlowPhoneOTP,
	}
}

func (m *stubPhoneOTPMethod) SecureCookies() bool { return false }

func (m *stubPhoneOTPMethod) RegisterRoutes(tk AuthToolkit) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Phone string `json:"phone"`
			Code  string `json:"code"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Phone == "" || body.Code != "123456" {
			httpx.WriteError(tk.Logger, r.Context(), w, http.StatusUnauthorized, "invalid verification code")
			return
		}
		m.verifiedCalls++
		tk.Issuer.IssueSession(w, r, identity.ExternalIdentity{
			Issuer:        "urn:shortlink:phone",
			Subject:       body.Phone,
			Email:         body.Phone + "@phone.local",
			EmailVerified: true,
			Name:          "Phone User",
		}, IssueSessionOptions{})
	})
	wrapped := tk.CSRF.Handler(handler)
	if tk.IPRateLimit != nil {
		wrapped = tk.IPRateLimit(wrapped)
	}
	tk.Mux.Handle("POST /api/v1/auth/phone/verify", wrapped)
}

func TestCustomDirectAuthMethodIssuesSession(t *testing.T) {
	var issuedToken string
	sessions := &stubSessionStore{
		infoFn: func(_ context.Context, token string) (identity.SessionInfo, error) {
			if token != issuedToken {
				return identity.SessionInfo{}, identity.ErrUnauthenticated
			}
			return identity.SessionInfo{
				User:      identity.User{ID: 100, Email: "+8613800138000@phone.local", Name: "Phone User"},
				ExpiresAt: time.Now().UTC().Add(time.Hour),
			}, nil
		},
	}
	phone := &stubPhoneOTPMethod{}
	srv := New(selfServiceLogger(), &stubStore{}, 2*time.Second, prometheus.NewRegistry(), Options{
		Authorizer:  identity.NewMapAuthorizer(),
		Sessions:    sessions,
		AuthMethods: []AuthMethod{phone},
	})
	h := srv.Routes()

	// 1. /api/v1/capabilities 自动暴露 direct 类型插件，且不输出 login_url。
	capRec := httptest.NewRecorder()
	h.ServeHTTP(capRec, httptest.NewRequest(http.MethodGet, "/api/v1/capabilities", nil))
	if capRec.Code != http.StatusOK {
		t.Fatalf("GET /capabilities = %d, want 200", capRec.Code)
	}
	var caps capabilitiesResponse
	if err := json.Unmarshal(capRec.Body.Bytes(), &caps); err != nil {
		t.Fatalf("decode capabilities: %v", err)
	}
	if len(caps.AuthMethods) != 1 || caps.AuthMethods[0].ID != "phone" || caps.AuthMethods[0].Kind != AuthFlowPhoneOTP || caps.AuthMethods[0].LoginURL != "" {
		t.Fatalf("auth_methods = %+v, want single phone_otp method without login_url", caps.AuthMethods)
	}

	// 2. 跨站 POST 验证码登录被 CSRF 拦截，绝不调用业务校验也不签发会话。
	csrfReq := httptest.NewRequest(http.MethodPost, "/api/v1/auth/phone/verify", strings.NewReader(`{"phone":"+8613800138000","code":"123456"}`))
	csrfReq.Header.Set("Sec-Fetch-Site", "cross-site")
	csrfRec := httptest.NewRecorder()
	h.ServeHTTP(csrfRec, csrfReq)
	if csrfRec.Code != http.StatusForbidden {
		t.Fatalf("cross-site verify = %d, want 403", csrfRec.Code)
	}
	if phone.verifiedCalls != 0 || sessions.calls != 0 {
		t.Fatalf("side effects on CSRF failure: verifiedCalls=%d sessions.calls=%d, want 0/0", phone.verifiedCalls, sessions.calls)
	}

	// 3. 合法 POST 签发会话 Cookie，并可直接访问 /api/v1/me。
	okReq := httptest.NewRequest(http.MethodPost, "/api/v1/auth/phone/verify", strings.NewReader(`{"phone":"+8613800138000","code":"123456"}`))
	okRec := httptest.NewRecorder()
	h.ServeHTTP(okRec, okReq)
	if okRec.Code != http.StatusOK {
		t.Fatalf("verify = %d, want 200 (body=%s)", okRec.Code, okRec.Body.String())
	}
	sessCookie := findCookie(okRec.Result().Cookies(), SessionCookieName)
	if sessCookie == nil || !identity.SessionToken.Valid(sessCookie.Value) {
		t.Fatalf("session cookie = %+v, want valid sl_session", sessCookie)
	}
	issuedToken = sessCookie.Value

	meReq := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	meReq.AddCookie(sessCookie)
	meRec := httptest.NewRecorder()
	h.ServeHTTP(meRec, meReq)
	if meRec.Code != http.StatusOK || !strings.Contains(meRec.Body.String(), "Phone User") {
		t.Fatalf("GET /me = %d %s, want 200 Phone User", meRec.Code, meRec.Body.String())
	}

	// 4. 旧路径已彻底删除（不留伪兼容壳）。
	for _, legacy := range []string{"/api/v1/auth/providers", "/api/v1/auth/login", "/api/v1/auth/callback"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, legacy, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", legacy, rec.Code)
		}
	}
}

func TestValidateAuthMethodsAndDuplicatePanics(t *testing.T) {
	if _, err := NewOAuthAuthMethod("OIDC", selfServiceOIDC{}); err == nil {
		t.Error("NewOAuthAuthMethod accepted uppercase id, want error")
	}
	if _, err := NewOAuthAuthMethod("a/b", selfServiceOIDC{}); err == nil {
		t.Error("NewOAuthAuthMethod accepted slash id, want error")
	}
	m1 := mustOAuthMethod(t, "oidc", selfServiceOIDC{})
	m2 := mustOAuthMethod(t, "oidc", selfServiceOIDC{})
	if err := ValidateAuthMethods([]AuthMethod{m1, m2}); err == nil {
		t.Error("ValidateAuthMethods accepted duplicate id, want error")
	}

	t.Run("duplicate auth method panics", func(t *testing.T) {
		defer func() {
			if recover() == nil {
				t.Fatal("expected panic on duplicate auth method")
			}
		}()
		New(selfServiceLogger(), &stubStore{}, time.Second, prometheus.NewRegistry(), Options{
			AuthMethods: []AuthMethod{m1, m2},
		})
	})

	t.Run("duplicate module panics", func(t *testing.T) {
		defer func() {
			if recover() == nil {
				t.Fatal("expected panic on duplicate module")
			}
		}()
		admin := &stubAdmin{}
		New(selfServiceLogger(), &stubStore{}, time.Second, prometheus.NewRegistry(), Options{
			Modules: []Module{NewStatsModule(admin), NewStatsModule(admin)},
		})
	})
}

func TestCapabilitiesDerivesFromRegisteredModules(t *testing.T) {
	cipher := newTestWebhookCipher(t)
	srv := New(selfServiceLogger(), &stubStore{}, time.Second, prometheus.NewRegistry(), Options{
		Authorizer: identity.NewMapAuthorizer(),
		AuthMethods: []AuthMethod{
			mustOAuthMethod(t, "oidc", selfServiceOIDC{}),
			&stubPhoneOTPMethod{},
		},
		Modules: []Module{
			NewStatsModule(&stubAdmin{}),
			NewWebhooksModule(&stubWebhookStore{}, cipher, safehttp.URLValidationOptions{}),
		},
		RiskScanEnabled: true,
		EmailEnabled:    true,
		PublicBaseURL:   "https://s.example.com",
	})
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/capabilities", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /capabilities = %d, want 200", rec.Code)
	}
	var caps capabilitiesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &caps); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(caps.AuthMethods) != 2 || caps.AuthMethods[0].ID != "oidc" || caps.AuthMethods[1].ID != "phone" {
		t.Fatalf("auth_methods = %+v", caps.AuthMethods)
	}
	wantFeat := featureCapabilities{Analytics: true, Webhooks: true, RiskScan: true, Email: true}
	if caps.Features != wantFeat {
		t.Fatalf("features = %+v, want %+v", caps.Features, wantFeat)
	}

	// 空装配时 auth_methods 必须序列化为 [] 而非 null，且所有特性为 false。
	emptySrv := New(selfServiceLogger(), &stubStore{}, time.Second, prometheus.NewRegistry(), Options{
		Authorizer: identity.NewMapAuthorizer(),
	})
	emptyRec := httptest.NewRecorder()
	emptySrv.Routes().ServeHTTP(emptyRec, httptest.NewRequest(http.MethodGet, "/api/v1/capabilities", nil))
	if emptyRec.Code != http.StatusOK {
		t.Fatalf("empty GET /capabilities = %d, want 200", emptyRec.Code)
	}
	if !strings.Contains(emptyRec.Body.String(), `"auth_methods":[]`) {
		t.Errorf("empty body = %s, want auth_methods:[]", emptyRec.Body.String())
	}
}

func TestAnalyticsDisabledOmitsClickFieldsAndRejectsClickSort(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	var listCalls int
	admin := &stubAdmin{
		listFn: func(context.Context, link.ListOptions) ([]link.AdminLink, error) {
			listCalls++
			return []link.AdminLink{{
				Link:        link.Link{ID: 1, Code: "AbC123xy", URL: "https://example.com", CreatedAt: now},
				Clicks:      99,
				ClicksToday: 12,
			}}, nil
		},
		getFn: func(context.Context, int64, string) (*link.AdminLink, error) {
			return &link.AdminLink{
				Link:        link.Link{ID: 1, Code: "AbC123xy", URL: "https://example.com", CreatedAt: now},
				Clicks:      99,
				ClicksToday: 12,
			}, nil
		},
		updFn: func(_ context.Context, _ int64, code string, u link.Update) (*link.AdminLink, error) {
			return &link.AdminLink{
				Link:        link.Link{ID: 1, Code: code, URL: *u.URL, CreatedAt: now},
				Clicks:      99,
				ClicksToday: 12,
			}, nil
		},
	}

	// 不挂载 ModuleAnalytics 时，统计端点 404，clicks/clicks_today 字段省略，按点击排序返回 400 且不查库。
	srv := newServer(serverDeps{
		logger:         selfServiceLogger(),
		store:          &stubStore{},
		admin:          admin,
		requestTimeout: 5 * time.Second,
		registry:       newTestRegistry(),
		keys:           allowKey{},
		authorizer:     identity.NewMapAuthorizer(),
	})
	h := srv.Routes()

	assertNoClickFields := func(label string, body []byte) {
		t.Helper()
		s := string(body)
		if strings.Contains(s, `"clicks"`) || strings.Contains(s, `"clicks_today"`) {
			t.Errorf("%s leaked click fields when analytics disabled: %s", label, s)
		}
	}

	createRec := doAdminRequest(t, h, http.MethodPost, "/api/v1/links", `{"url":"https://example.com/a"}`)
	if createRec.Code != http.StatusCreated {
		t.Fatalf("POST /links = %d, want 201 (%s)", createRec.Code, createRec.Body.String())
	}
	assertNoClickFields("POST /links", createRec.Body.Bytes())

	getRec := doAdminRequest(t, h, http.MethodGet, "/api/v1/links/AbC123xy", "")
	if getRec.Code != http.StatusOK {
		t.Fatalf("GET /links/code = %d, want 200", getRec.Code)
	}
	assertNoClickFields("GET /links/code", getRec.Body.Bytes())

	patchRec := doAdminRequest(t, h, http.MethodPatch, "/api/v1/links/AbC123xy", `{"url":"https://example.com/b"}`)
	if patchRec.Code != http.StatusOK {
		t.Fatalf("PATCH /links/code = %d, want 200", patchRec.Code)
	}
	assertNoClickFields("PATCH /links/code", patchRec.Body.Bytes())

	listRec := doAdminRequest(t, h, http.MethodGet, "/api/v1/links?sort=id", "")
	if listRec.Code != http.StatusOK {
		t.Fatalf("GET /links?sort=id = %d, want 200", listRec.Code)
	}
	assertNoClickFields("GET /links?sort=id", listRec.Body.Bytes())
	if listCalls != 1 {
		t.Fatalf("listCalls = %d, want 1", listCalls)
	}

	for _, sortVal := range []string{"clicks", "clicks_today"} {
		rec := doAdminRequest(t, h, http.MethodGet, "/api/v1/links?sort="+sortVal, "")
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "analytics is disabled") {
			t.Errorf("GET /links?sort=%s = %d %s, want 400 analytics is disabled", sortVal, rec.Code, rec.Body.String())
		}
	}
	if listCalls != 1 {
		t.Errorf("listCalls after rejected click sorts = %d, want 1 (must not query store)", listCalls)
	}

	statsRec := doAdminRequest(t, h, http.MethodGet, "/api/v1/links/AbC123xy/stats", "")
	if statsRec.Code != http.StatusNotFound {
		t.Errorf("GET /stats when analytics disabled = %d, want 404", statsRec.Code)
	}
}

type trackingOAuthClient struct {
	selfServiceOIDC
	exchangeCalls int
}

func (c *trackingOAuthClient) Exchange(ctx context.Context, code, codeVerifier, expectedNonce string) (identity.ExternalIdentity, error) {
	c.exchangeCalls++
	return c.selfServiceOIDC.Exchange(ctx, code, codeVerifier, expectedNonce)
}

func TestOAuthAuthMethodIsolatesFlowCookieByID(t *testing.T) {
	sharedClient := &trackingOAuthClient{}
	methodA := mustOAuthMethod(t, "oidc-a", sharedClient)
	methodB := mustOAuthMethod(t, "oidc-b", sharedClient)
	sessions := &stubSessionStore{}

	srv := New(selfServiceLogger(), &stubStore{}, 2*time.Second, prometheus.NewRegistry(), Options{
		Authorizer:  identity.NewMapAuthorizer(),
		Sessions:    sessions,
		AuthMethods: []AuthMethod{methodA, methodB},
	})
	h := srv.Routes()

	loginRec := httptest.NewRecorder()
	h.ServeHTTP(loginRec, httptest.NewRequest(http.MethodGet, "/api/v1/auth/oidc-a/login", nil))
	if loginRec.Code != http.StatusFound {
		t.Fatalf("GET oidc-a/login = %d, want 302", loginRec.Code)
	}
	cookieA := findCookie(loginRec.Result().Cookies(), "sl_oidc-a_flow")
	if cookieA == nil {
		t.Fatal("missing sl_oidc-a_flow cookie")
	}

	// 将 oidc-a 签发的流程 Cookie 改名重放到共享同一底层 Client 密钥的 oidc-b 回调，必须验签失败且不调 Exchange。
	replayReq := httptest.NewRequest(http.MethodGet, "/api/v1/auth/oidc-b/callback?code=c1&state=any", nil)
	replayReq.AddCookie(&http.Cookie{Name: "sl_oidc-b_flow", Value: cookieA.Value})
	replayRec := httptest.NewRecorder()
	h.ServeHTTP(replayRec, replayReq)
	if replayRec.Code != http.StatusBadRequest {
		t.Fatalf("replayed callback to oidc-b = %d, want 400", replayRec.Code)
	}
	if sharedClient.exchangeCalls != 0 || sessions.calls != 0 {
		t.Fatalf("unexpected side effects on cross-method cookie replay: exchange=%d sessions=%d", sharedClient.exchangeCalls, sessions.calls)
	}
}

type stubReturnToAuthMethod struct{}

func (m *stubReturnToAuthMethod) Descriptor() AuthMethodDescriptor {
	return AuthMethodDescriptor{ID: "return-to-probe", Kind: AuthFlowPhoneOTP}
}

func (m *stubReturnToAuthMethod) SecureCookies() bool { return false }

func (m *stubReturnToAuthMethod) RegisterRoutes(tk AuthToolkit) {
	tk.Mux.Handle("POST /api/v1/auth/return-to-probe/verify", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tk.Issuer.IssueSession(w, r, identity.ExternalIdentity{
			Issuer:        "urn:shortlink:probe",
			Subject:       "u1",
			EmailVerified: false,
		}, IssueSessionOptions{
			ReturnTo: r.URL.Query().Get("return_to"),
		})
	}))
}

func TestIssueSessionRejectsInvalidReturnTo(t *testing.T) {
	sessions := &stubSessionStore{}
	srv := New(selfServiceLogger(), &stubStore{}, 2*time.Second, prometheus.NewRegistry(), Options{
		Authorizer:  identity.NewMapAuthorizer(),
		Sessions:    sessions,
		AuthMethods: []AuthMethod{&stubReturnToAuthMethod{}},
	})
	h := srv.Routes()

	for _, bad := range []string{"//evil.com", "https://evil.com", "/api/v1/me", "/r/AbC123xy"} {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/return-to-probe/verify?return_to="+url.QueryEscape(bad), nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("return_to=%q status = %d, want 400 (body=%s)", bad, rec.Code, rec.Body.String())
		}
		if findCookie(rec.Result().Cookies(), SessionCookieName) != nil {
			t.Errorf("return_to=%q issued sl_session cookie on 400", bad)
		}
	}
	if sessions.calls != 0 {
		t.Fatalf("sessions.calls = %d, want 0 when return_to is invalid", sessions.calls)
	}
}

type stubNamedModule struct {
	name string
}

func (m stubNamedModule) Name() string                  { return m.name }
func (m stubNamedModule) RegisterRoutes(RouteRegistrar) {}

func TestValidateModules(t *testing.T) {
	if err := ValidateModules([]Module{nil, stubNamedModule{name: "alpha"}, stubNamedModule{name: "beta"}}); err != nil {
		t.Fatalf("ValidateModules(valid) = %v, want nil", err)
	}
	if err := ValidateModules([]Module{stubNamedModule{name: ""}}); err == nil {
		t.Fatal("ValidateModules(empty name) = nil, want error")
	}
	if err := ValidateModules([]Module{stubNamedModule{name: "alpha"}, stubNamedModule{name: "alpha"}}); err == nil {
		t.Fatal("ValidateModules(duplicate) = nil, want error")
	}
}

func TestHandleListMapsErrAnalyticsDisabledToBadRequest(t *testing.T) {
	admin := &stubAdmin{
		listFn: func(context.Context, link.ListOptions) ([]link.AdminLink, error) {
			return nil, fmt.Errorf("list links: %w", link.ErrAnalyticsDisabled)
		},
	}
	srv := newServer(serverDeps{
		logger:         selfServiceLogger(),
		store:          &stubStore{},
		admin:          admin,
		requestTimeout: 5 * time.Second,
		registry:       newTestRegistry(),
		keys:           allowKey{},
		authorizer:     identity.NewMapAuthorizer(),
		modules:        []Module{NewStatsModule(admin)},
	})

	rec := doAdminRequest(t, srv.Routes(), http.MethodGet, "/api/v1/links?sort=clicks", "")
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), link.ErrAnalyticsDisabled.Error()) {
		t.Fatalf("GET /links?sort=clicks = %d %s, want 400 %q", rec.Code, rec.Body.String(), link.ErrAnalyticsDisabled.Error())
	}
}
