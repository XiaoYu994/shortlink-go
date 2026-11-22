package postgres

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"shortlink/internal/console"
	"shortlink/internal/identity"
	"shortlink/internal/identity/fakegithub"
	"shortlink/internal/identity/fakeoidc"
)

func TestHTTPOIDCLoginPersistsSession(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()

	_, handler := newOIDCSeamHandler(t, ctx, store)
	firstCookie, firstUserID, firstEmail := loginViaFakeOIDC(t, handler)
	secondCookie, secondUserID, secondEmail := loginViaFakeOIDC(t, handler)
	if firstEmail != "alice@example.com" || secondEmail != "alice@example.com" {
		t.Errorf("emails = (%q, %q), want alice@example.com", firstEmail, secondEmail)
	}
	if firstUserID != secondUserID {
		t.Fatalf("second login user id = %d, want %d", secondUserID, firstUserID)
	}
	firstToken, secondToken := firstCookie.Value, secondCookie.Value
	if firstToken == secondToken {
		t.Fatal("second login reused the same session token")
	}

	var storedHash []byte
	var storedUserID int64
	if err := store.db.QueryRowContext(ctx,
		`SELECT user_id, token_hash FROM sessions WHERE token_hash = $1`,
		identity.HashToken(firstToken),
	).Scan(&storedUserID, &storedHash); err != nil {
		t.Fatalf("lookup first session by hash: %v", err)
	}
	if storedUserID != firstUserID || !bytes.Equal(storedHash, identity.HashToken(firstToken)) {
		t.Fatalf("stored session mismatch: user=%d", storedUserID)
	}

	var usersCount, sessionsCount int
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM users`).Scan(&usersCount); err != nil {
		t.Fatalf("count users: %v", err)
	}
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM sessions`).Scan(&sessionsCount); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	if usersCount != 1 || sessionsCount != 2 {
		t.Fatalf("counts (users=%d, sessions=%d), want (1, 2)", usersCount, sessionsCount)
	}
}

/*
TestHTTPSessionAuthOrgSwitchAndLogoutImmediateEffect 端到端验证：
1. 登录但未加入组织时 /me 返回空组织数组，携带 X-Org-ID 调管理 API 返回 404；
2. 加入多组织后通过 X-Org-ID 在不同组织与角色间切换；
3. 改角色、移出组织与登出后，同一个会话的下一次请求立刻生效；
4. 带会话 Cookie 的跨站写请求被 CrossOriginProtection 拒绝。
*/
func TestHTTPSessionAuthOrgSwitchAndLogoutImmediateEffect(t *testing.T) {
	store, tenant1 := newTestStore(t)
	ctx := context.Background()

	var tenant2 int64
	if err := store.db.QueryRowContext(ctx,
		`INSERT INTO tenants (name, rate_limit_per_second) VALUES ('org-two', 0) RETURNING id`,
	).Scan(&tenant2); err != nil {
		t.Fatalf("insert second tenant: %v", err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE tenants SET rate_limit_per_second = 0 WHERE id = $1`, tenant1); err != nil {
		t.Fatalf("disable tenant1 rate limit: %v", err)
	}

	_, handler := newOIDCSeamHandler(t, ctx, store)

	// 1. 走完 OIDC 登录拿到会话 Cookie。
	sessCookie, userID, _ := loginViaFakeOIDC(t, handler)

	doWithSession := func(method, path string, orgID int64, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.AddCookie(sessCookie)
		if orgID > 0 {
			req.Header.Set("X-Org-ID", strconv.FormatInt(orgID, 10))
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}

	// 2. 无成员关系时 /me 返回空组织数组，管理 API 返回 404。
	meRec := doWithSession(http.MethodGet, "/api/v1/me", 0, "")
	if meRec.Code != http.StatusOK || !strings.Contains(meRec.Body.String(), `"organizations":[]`) {
		t.Fatalf("/me before membership = %d, body = %s", meRec.Code, meRec.Body.String())
	}
	if rec := doWithSession(http.MethodGet, "/api/v1/links", tenant1, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("list before membership = %d, want 404", rec.Code)
	}

	// 3. 加入 tenant1(viewer) 和 tenant2(admin)，验证 /me 和 X-Org-ID 跨组织切换。
	if _, err := store.db.ExecContext(ctx,
		`INSERT INTO memberships (user_id, tenant_id, role) VALUES ($1, $2, 'viewer'), ($1, $3, 'admin')`,
		userID, tenant1, tenant2,
	); err != nil {
		t.Fatalf("insert memberships: %v", err)
	}
	meRec = doWithSession(http.MethodGet, "/api/v1/me", 0, "")
	if meRec.Code != http.StatusOK || !strings.Contains(meRec.Body.String(), `"role":"viewer"`) || !strings.Contains(meRec.Body.String(), `"role":"admin"`) {
		t.Fatalf("/me with memberships = %d, body = %s", meRec.Code, meRec.Body.String())
	}
	if rec := doWithSession(http.MethodGet, "/api/v1/links", tenant1, ""); rec.Code != http.StatusOK {
		t.Fatalf("viewer list tenant1 = %d, want 200", rec.Code)
	}
	if rec := doWithSession(http.MethodPost, "/api/v1/links", tenant1, `{"url":"https://example.com/t1-denied"}`); rec.Code != http.StatusForbidden {
		t.Fatalf("viewer create tenant1 = %d, want 403", rec.Code)
	}
	createT2 := doWithSession(http.MethodPost, "/api/v1/links", tenant2, `{"url":"https://example.com/t2-ok"}`)
	if createT2.Code != http.StatusCreated {
		t.Fatalf("admin create tenant2 = %d, want 201, body = %s", createT2.Code, createT2.Body.String())
	}

	// 4. 改角色为 member，同一个会话的下一次请求立刻放行；跨站 POST 仍被拒绝。
	if _, err := store.db.ExecContext(ctx,
		`UPDATE memberships SET role = 'member' WHERE user_id = $1 AND tenant_id = $2`, userID, tenant1,
	); err != nil {
		t.Fatalf("promote role: %v", err)
	}
	if rec := doWithSession(http.MethodPost, "/api/v1/links", tenant1, `{"url":"https://example.com/t1-ok"}`); rec.Code != http.StatusCreated {
		t.Fatalf("member create tenant1 after promotion = %d, want 201, body = %s", rec.Code, rec.Body.String())
	}
	csrfReq := httptest.NewRequest(http.MethodPost, "/api/v1/links", strings.NewReader(`{"url":"https://example.com/csrf"}`))
	csrfReq.AddCookie(sessCookie)
	csrfReq.Header.Set("X-Org-ID", strconv.FormatInt(tenant1, 10))
	csrfReq.Header.Set("Sec-Fetch-Site", "cross-site")
	csrfRec := httptest.NewRecorder()
	handler.ServeHTTP(csrfRec, csrfReq)
	if csrfRec.Code != http.StatusForbidden {
		t.Fatalf("cross-site create = %d, want 403", csrfRec.Code)
	}

	// 5. 移出 tenant1，同一个会话的下一次请求立刻 404，tenant2 仍可用。
	if _, err := store.db.ExecContext(ctx,
		`DELETE FROM memberships WHERE user_id = $1 AND tenant_id = $2`, userID, tenant1,
	); err != nil {
		t.Fatalf("remove membership: %v", err)
	}
	if rec := doWithSession(http.MethodGet, "/api/v1/links", tenant1, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("list tenant1 after removal = %d, want 404", rec.Code)
	}
	if rec := doWithSession(http.MethodGet, "/api/v1/links", tenant2, ""); rec.Code != http.StatusOK {
		t.Fatalf("list tenant2 while still member = %d, want 200", rec.Code)
	}

	// 6. 登出后，同一个会话的下一次 /me 和管理 API 请求立刻 401。
	logoutRec := doWithSession(http.MethodPost, "/api/v1/auth/logout", 0, "")
	if logoutRec.Code != http.StatusNoContent {
		t.Fatalf("logout status = %d, want 204, body = %s", logoutRec.Code, logoutRec.Body.String())
	}
	if rec := doWithSession(http.MethodGet, "/api/v1/me", 0, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("/me after logout = %d, want 401", rec.Code)
	}
	if rec := doWithSession(http.MethodGet, "/api/v1/links", tenant2, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("list after logout = %d, want 401", rec.Code)
	}
}

/*
TestHTTPOIDCLoginRedeemsVerifiedInvitationOnly 端到端验证 Step 5：
通过 fakeoidc 完成 OIDC 登录时，未验证邮箱不兑现待处理邀请；
一旦以 email_verified=true 完成登录，同一事务内兑现多个租户的待兑现邀请，
紧接着 /me 和带 X-Org-ID 的管理接口立刻生效。
*/
func TestHTTPOIDCLoginRedeemsVerifiedInvitationOnly(t *testing.T) {
	store, tenant1 := newTestStore(t)
	ctx := context.Background()

	var tenant2 int64
	if err := store.db.QueryRowContext(ctx,
		`INSERT INTO tenants (name, rate_limit_per_second) VALUES ('org-invite-two', 0) RETURNING id`,
	).Scan(&tenant2); err != nil {
		t.Fatalf("insert second tenant: %v", err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE tenants SET rate_limit_per_second = 0 WHERE id = $1`, tenant1); err != nil {
		t.Fatalf("disable tenant1 rate limit: %v", err)
	}

	inv1, err := store.CreateInvitation(ctx, CreateInvitationParams{
		TenantID: tenant1,
		Email:    "alice@example.com",
		Role:     identity.RoleOwner,
	})
	if err != nil {
		t.Fatalf("CreateInvitation tenant1: %v", err)
	}
	inv2, err := store.CreateInvitation(ctx, CreateInvitationParams{
		TenantID: tenant2,
		Email:    "alice@example.com",
		Role:     identity.RoleMember,
	})
	if err != nil {
		t.Fatalf("CreateInvitation tenant2: %v", err)
	}

	fake, handler := newOIDCSeamHandler(t, ctx, store)

	loginAs := func(u fakeoidc.User) (*http.Cookie, int64) {
		t.Helper()
		fake.SetUser(u)
		sessCookie, userID, _ := loginViaFakeOIDC(t, handler)
		return sessCookie, userID
	}

	// 1. email_verified=false 登录不兑现邀请。
	unverifiedCookie, userID := loginAs(fakeoidc.User{
		Subject:       "sub-alice",
		Email:         "alice@example.com",
		EmailVerified: false,
		Name:          "Alice",
	})
	meReq := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	meReq.AddCookie(unverifiedCookie)
	meRec := httptest.NewRecorder()
	handler.ServeHTTP(meRec, meReq)
	if meRec.Code != http.StatusOK || !strings.Contains(meRec.Body.String(), `"organizations":[]`) {
		t.Fatalf("/me unverified = %d, body = %s", meRec.Code, meRec.Body.String())
	}
	listReq := httptest.NewRequest(http.MethodGet, "/api/v1/links", nil)
	listReq.AddCookie(unverifiedCookie)
	listReq.Header.Set("X-Org-ID", strconv.FormatInt(tenant1, 10))
	listRec := httptest.NewRecorder()
	handler.ServeHTTP(listRec, listReq)
	if listRec.Code != http.StatusNotFound {
		t.Fatalf("list unverified = %d, want 404", listRec.Code)
	}

	// 2. email_verified=true（含大写）登录立刻兑现两家组织的邀请。
	verifiedCookie, verifiedUserID := loginAs(fakeoidc.User{
		Subject:       "sub-alice",
		Email:         "Alice@Example.COM",
		EmailVerified: true,
		Name:          "Alice",
	})
	if verifiedUserID != userID {
		t.Fatalf("user id changed from %d to %d", userID, verifiedUserID)
	}
	meReq2 := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	meReq2.AddCookie(verifiedCookie)
	meRec2 := httptest.NewRecorder()
	handler.ServeHTTP(meRec2, meReq2)
	if meRec2.Code != http.StatusOK ||
		!strings.Contains(meRec2.Body.String(), `"role":"owner"`) ||
		!strings.Contains(meRec2.Body.String(), `"role":"member"`) {
		t.Fatalf("/me verified = %d, body = %s", meRec2.Code, meRec2.Body.String())
	}

	for _, orgID := range []int64{tenant1, tenant2} {
		createReq := httptest.NewRequest(http.MethodPost, "/api/v1/links", strings.NewReader(`{"url":"https://example.com/invited"}`))
		createReq.AddCookie(verifiedCookie)
		createReq.Header.Set("X-Org-ID", strconv.FormatInt(orgID, 10))
		createRec := httptest.NewRecorder()
		handler.ServeHTTP(createRec, createReq)
		if createRec.Code != http.StatusCreated {
			t.Fatalf("create in org %d after redeem = %d, want 201, body = %s", orgID, createRec.Code, createRec.Body.String())
		}
	}

	for _, invID := range []int64{inv1.ID, inv2.ID} {
		var acceptedBy *int64
		if err := store.db.QueryRowContext(ctx, `SELECT accepted_by FROM invitations WHERE id = $1`, invID).Scan(&acceptedBy); err != nil {
			t.Fatalf("select invitation %d: %v", invID, err)
		}
		if acceptedBy == nil || *acceptedBy != userID {
			t.Errorf("invitation %d accepted_by = %v, want %d", invID, acceptedBy, userID)
		}
	}
}

/*
TestHTTPMemberManagementImmediateEffect 端到端验证 Step 6（ADR 0018）：
1. API Key 访问成员管理返回 403；viewer 可读列表但写操作与建链返回 403；
2. 最后一名 owner 不能自我降级或移出（409）；admin 不能改/删 owner 或提拔 owner（403）；
3. 修改角色（升级/降级）或移出组织后，受影响成员的现有会话下一次请求立刻生效，跨站写被拒绝；
4. 移出组织不吊销会话，目标用户在其他组织的操作保持正常。
*/
func TestHTTPMemberManagementImmediateEffect(t *testing.T) {
	store, tenant1 := newTestStore(t)
	ctx := context.Background()

	var tenant2 int64
	if err := store.db.QueryRowContext(ctx,
		`INSERT INTO tenants (name, rate_limit_per_second) VALUES ('org-member-two', 0) RETURNING id`,
	).Scan(&tenant2); err != nil {
		t.Fatalf("insert tenant2: %v", err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE tenants SET rate_limit_per_second = 0 WHERE id = $1`, tenant1); err != nil {
		t.Fatalf("disable tenant1 rate limit: %v", err)
	}

	if _, err := store.CreateInvitation(ctx, CreateInvitationParams{
		TenantID: tenant1,
		Email:    "alice@example.com",
		Role:     identity.RoleOwner,
	}); err != nil {
		t.Fatalf("invite alice: %v", err)
	}
	if _, err := store.CreateInvitation(ctx, CreateInvitationParams{
		TenantID: tenant1,
		Email:    "bob@example.com",
		Role:     identity.RoleViewer,
	}); err != nil {
		t.Fatalf("invite bob tenant1: %v", err)
	}
	if _, err := store.CreateInvitation(ctx, CreateInvitationParams{
		TenantID: tenant2,
		Email:    "bob@example.com",
		Role:     identity.RoleMember,
	}); err != nil {
		t.Fatalf("invite bob tenant2: %v", err)
	}

	fake, handler := newOIDCSeamHandler(t, ctx, store)
	fake.SetUser(fakeoidc.User{Subject: "sub-alice", Email: "alice@example.com", EmailVerified: true, Name: "Alice"})
	aliceCookie, aliceID, _ := loginViaFakeOIDC(t, handler)
	fake.SetUser(fakeoidc.User{Subject: "sub-bob", Email: "bob@example.com", EmailVerified: true, Name: "Bob"})
	bobCookie, bobID, _ := loginViaFakeOIDC(t, handler)

	doWithSession := func(cookie *http.Cookie, method, path string, orgID int64, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.AddCookie(cookie)
		if orgID > 0 {
			req.Header.Set("X-Org-ID", strconv.FormatInt(orgID, 10))
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}

	// 1. 全权限 API Key 对成员管理三个端点一律返回 403。
	issuedKey, err := store.IssueAPIKey(ctx, IssueAPIKeyParams{
		TenantID: tenant1,
		Scopes:   identity.AllAPIKeyScopes(),
	})
	if err != nil {
		t.Fatalf("IssueAPIKey: %v", err)
	}
	for _, tc := range []struct {
		method string
		path   string
		body   string
	}{
		{method: http.MethodGet, path: "/api/v1/members"},
		{method: http.MethodPatch, path: "/api/v1/members/" + strconv.FormatInt(bobID, 10), body: `{"role":"member"}`},
		{method: http.MethodDelete, path: "/api/v1/members/" + strconv.FormatInt(bobID, 10)},
	} {
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		req.Header.Set("Authorization", "Bearer "+issuedKey.Plaintext)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("api key %s %s = %d, want 403, body = %s", tc.method, tc.path, rec.Code, rec.Body.String())
		}
	}

	// 2. Bob（viewer）可读列表（按 user_id 升序），但改成员和建链均返回 403。
	listRec := doWithSession(bobCookie, http.MethodGet, "/api/v1/members", tenant1, "")
	if listRec.Code != http.StatusOK {
		t.Fatalf("bob list members = %d, want 200, body = %s", listRec.Code, listRec.Body.String())
	}
	var listResp struct {
		Items []struct {
			UserID int64  `json:"user_id"`
			Role   string `json:"role"`
		} `json:"items"`
	}
	if err := json.Unmarshal(listRec.Body.Bytes(), &listResp); err != nil {
		t.Fatalf("decode members list: %v", err)
	}
	if len(listResp.Items) != 2 || listResp.Items[0].UserID != aliceID || listResp.Items[0].Role != "owner" ||
		listResp.Items[1].UserID != bobID || listResp.Items[1].Role != "viewer" {
		t.Fatalf("members list = %+v", listResp.Items)
	}
	if rec := doWithSession(bobCookie, http.MethodPatch, "/api/v1/members/"+strconv.FormatInt(aliceID, 10), tenant1, `{"role":"viewer"}`); rec.Code != http.StatusForbidden {
		t.Fatalf("viewer patch member = %d, want 403", rec.Code)
	}
	if rec := doWithSession(bobCookie, http.MethodDelete, "/api/v1/members/"+strconv.FormatInt(aliceID, 10), tenant1, ""); rec.Code != http.StatusForbidden {
		t.Fatalf("viewer delete member = %d, want 403", rec.Code)
	}
	if rec := doWithSession(bobCookie, http.MethodPost, "/api/v1/links", tenant1, `{"url":"https://example.com/viewer-denied"}`); rec.Code != http.StatusForbidden {
		t.Fatalf("viewer create link = %d, want 403", rec.Code)
	}

	// 3. Alice 是组织唯一 owner，自我降级或移出返回 409。
	if rec := doWithSession(aliceCookie, http.MethodPatch, "/api/v1/members/"+strconv.FormatInt(aliceID, 10), tenant1, `{"role":"admin"}`); rec.Code != http.StatusConflict {
		t.Fatalf("last owner demote = %d, want 409, body = %s", rec.Code, rec.Body.String())
	}
	if rec := doWithSession(aliceCookie, http.MethodDelete, "/api/v1/members/"+strconv.FormatInt(aliceID, 10), tenant1, ""); rec.Code != http.StatusConflict {
		t.Fatalf("last owner delete = %d, want 409, body = %s", rec.Code, rec.Body.String())
	}

	// 4. Alice 把 Bob 改为 member，Bob 原会话不重登立刻可以创建短链。
	if rec := doWithSession(aliceCookie, http.MethodPatch, "/api/v1/members/"+strconv.FormatInt(bobID, 10), tenant1, `{"role":"member"}`); rec.Code != http.StatusOK {
		t.Fatalf("promote bob to member = %d, want 200, body = %s", rec.Code, rec.Body.String())
	}
	if rec := doWithSession(bobCookie, http.MethodPost, "/api/v1/links", tenant1, `{"url":"https://example.com/bob-member"}`); rec.Code != http.StatusCreated {
		t.Fatalf("bob create link as member = %d, want 201, body = %s", rec.Code, rec.Body.String())
	}

	// 5. Alice 把 Bob 升为 admin；Bob 不能改/删 Alice（owner）或把任何人设为 owner。
	if rec := doWithSession(aliceCookie, http.MethodPatch, "/api/v1/members/"+strconv.FormatInt(bobID, 10), tenant1, `{"role":"admin"}`); rec.Code != http.StatusOK {
		t.Fatalf("promote bob to admin = %d, want 200", rec.Code)
	}
	if rec := doWithSession(bobCookie, http.MethodPatch, "/api/v1/members/"+strconv.FormatInt(aliceID, 10), tenant1, `{"role":"member"}`); rec.Code != http.StatusForbidden {
		t.Fatalf("admin patch owner = %d, want 403", rec.Code)
	}
	if rec := doWithSession(bobCookie, http.MethodDelete, "/api/v1/members/"+strconv.FormatInt(aliceID, 10), tenant1, ""); rec.Code != http.StatusForbidden {
		t.Fatalf("admin delete owner = %d, want 403", rec.Code)
	}
	if rec := doWithSession(bobCookie, http.MethodPatch, "/api/v1/members/"+strconv.FormatInt(bobID, 10), tenant1, `{"role":"owner"}`); rec.Code != http.StatusForbidden {
		t.Fatalf("admin promote to owner = %d, want 403", rec.Code)
	}

	// 6. Alice 把 Bob 降回 viewer，Bob 原会话立刻被禁止建链。
	if rec := doWithSession(aliceCookie, http.MethodPatch, "/api/v1/members/"+strconv.FormatInt(bobID, 10), tenant1, `{"role":"viewer"}`); rec.Code != http.StatusOK {
		t.Fatalf("demote bob to viewer = %d, want 200", rec.Code)
	}
	if rec := doWithSession(bobCookie, http.MethodPost, "/api/v1/links", tenant1, `{"url":"https://example.com/bob-demoted"}`); rec.Code != http.StatusForbidden {
		t.Fatalf("bob create link after demotion = %d, want 403", rec.Code)
	}

	// 7. 跨站 DELETE 被 CrossOriginProtection 拒绝，Bob 仍在 tenant1。
	csrfReq := httptest.NewRequest(http.MethodDelete, "/api/v1/members/"+strconv.FormatInt(bobID, 10), nil)
	csrfReq.AddCookie(aliceCookie)
	csrfReq.Header.Set("X-Org-ID", strconv.FormatInt(tenant1, 10))
	csrfReq.Header.Set("Sec-Fetch-Site", "cross-site")
	csrfRec := httptest.NewRecorder()
	handler.ServeHTTP(csrfRec, csrfReq)
	if csrfRec.Code != http.StatusForbidden || !strings.Contains(csrfRec.Body.String(), "cross-origin request rejected") {
		t.Fatalf("cross-site delete = (%d, %s), want 403 cross-origin request rejected", csrfRec.Code, csrfRec.Body.String())
	}
	if rec := doWithSession(bobCookie, http.MethodGet, "/api/v1/members", tenant1, ""); rec.Code != http.StatusOK {
		t.Fatalf("bob still in tenant1 after rejected csrf = %d, want 200", rec.Code)
	}

	// 8. Alice 移出 Bob：Bob 在 tenant1 立刻 404，/me 不再包含 tenant1 但仍保留 tenant2，且 tenant2 建链正常。
	if rec := doWithSession(aliceCookie, http.MethodDelete, "/api/v1/members/"+strconv.FormatInt(bobID, 10), tenant1, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("remove bob = %d, want 204, body = %s", rec.Code, rec.Body.String())
	}
	if rec := doWithSession(bobCookie, http.MethodGet, "/api/v1/links", tenant1, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("bob tenant1 after removal = %d, want 404", rec.Code)
	}
	meRec := doWithSession(bobCookie, http.MethodGet, "/api/v1/me", 0, "")
	if meRec.Code != http.StatusOK {
		t.Fatalf("bob /me = %d, body = %s", meRec.Code, meRec.Body.String())
	}
	var meBody struct {
		Organizations []struct {
			ID   int64  `json:"id"`
			Role string `json:"role"`
		} `json:"organizations"`
	}
	if err := json.Unmarshal(meRec.Body.Bytes(), &meBody); err != nil {
		t.Fatalf("decode bob /me: %v", err)
	}
	if len(meBody.Organizations) != 1 || meBody.Organizations[0].ID != tenant2 || meBody.Organizations[0].Role != "member" {
		t.Fatalf("bob orgs after removal = %+v, want only tenant2(member)", meBody.Organizations)
	}
	if rec := doWithSession(bobCookie, http.MethodPost, "/api/v1/links", tenant2, `{"url":"https://example.com/bob-t2"}`); rec.Code != http.StatusCreated {
		t.Fatalf("bob create link in tenant2 = %d, want 201, body = %s", rec.Code, rec.Body.String())
	}
}

func newOIDCSeamHandler(t *testing.T, ctx context.Context, store *Store) (*fakeoidc.Server, http.Handler) {
	t.Helper()
	const (
		clientID     = "shortlink-console"
		clientSecret = "shortlink-console-secret"
		redirectURL  = "http://127.0.0.1:8081/api/v1/auth/callback"
	)
	fake := fakeoidc.Start(t, clientID, clientSecret)
	provider, err := identity.NewOIDCProvider(ctx, identity.OIDCConfig{
		Issuer:       fake.URL(),
		ClientID:     clientID,
		ClientSecret: clientSecret,
		RedirectURL:  redirectURL,
		HTTPClient:   fake.Client(),
	})
	if err != nil {
		t.Fatalf("NewOIDCProvider: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := console.New(logger, store, 5*time.Second, prometheus.NewRegistry(), console.Options{
		Keys:       store,
		Authorizer: identity.NewMapAuthorizer(),
		OIDC:       provider,
		Sessions:   store,
		Members:    store,
	})
	return fake, srv.Routes()
}

func loginViaFakeOIDC(t *testing.T, handler http.Handler) (*http.Cookie, int64, string) {
	t.Helper()
	return loginViaProvider(t, handler, "/api/v1/auth/login", console.OIDCFlowCookieName)
}

func loginViaFakeGitHub(t *testing.T, handler http.Handler) (*http.Cookie, int64, string) {
	t.Helper()
	return loginViaProvider(t, handler, "/api/v1/auth/github/login", console.GitHubFlowCookieName)
}

func loginViaProvider(t *testing.T, handler http.Handler, loginPath, flowCookieName string) (*http.Cookie, int64, string) {
	t.Helper()
	loginRec := httptest.NewRecorder()
	handler.ServeHTTP(loginRec, httptest.NewRequest(http.MethodGet, loginPath, nil))
	if loginRec.Code != http.StatusFound {
		t.Fatalf("login %s status = %d, want 302", loginPath, loginRec.Code)
	}
	var flowCookie *http.Cookie
	for _, c := range loginRec.Result().Cookies() {
		if c.Name == flowCookieName {
			flowCookie = c
		}
	}
	if flowCookie == nil {
		t.Fatalf("missing flow cookie %s", flowCookieName)
	}

	noRedirect := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	authResp, err := noRedirect.Get(loginRec.Header().Get("Location"))
	if err != nil {
		t.Fatalf("GET fake authorize: %v", err)
	}
	defer func() { _ = authResp.Body.Close() }()
	if authResp.StatusCode != http.StatusFound {
		t.Fatalf("fake authorize status = %d, want 302", authResp.StatusCode)
	}
	cbURL, err := url.Parse(authResp.Header.Get("Location"))
	if err != nil {
		t.Fatalf("parse callback url: %v", err)
	}

	cbReq := httptest.NewRequest(http.MethodGet, cbURL.RequestURI(), nil)
	cbReq.AddCookie(flowCookie)
	cbRec := httptest.NewRecorder()
	handler.ServeHTTP(cbRec, cbReq)
	if cbRec.Code != http.StatusOK {
		t.Fatalf("callback status = %d, want 200, body = %s", cbRec.Code, cbRec.Body.String())
	}

	var sessCookie *http.Cookie
	for _, c := range cbRec.Result().Cookies() {
		if c.Name == console.SessionCookieName {
			sessCookie = c
		}
	}
	if sessCookie == nil {
		t.Fatal("missing session cookie")
	}
	var resp struct {
		User struct {
			ID    int64  `json:"id"`
			Email string `json:"email"`
		} `json:"user"`
	}
	if err := json.Unmarshal(cbRec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode callback response: %v", err)
	}
	return sessCookie, resp.User.ID, resp.User.Email
}

func TestHTTPGitHubLoginIsolationAndInvitationRedemption(t *testing.T) {
	store, tenant1 := newTestStore(t)
	ctx := context.Background()

	fakeOIDC := fakeoidc.Start(t, "shortlink-console", "shortlink-console-secret")
	oidcProvider, err := identity.NewOIDCProvider(ctx, identity.OIDCConfig{
		Issuer:       fakeOIDC.URL(),
		ClientID:     "shortlink-console",
		ClientSecret: "shortlink-console-secret",
		RedirectURL:  "http://127.0.0.1:8081/api/v1/auth/callback",
		HTTPClient:   fakeOIDC.Client(),
	})
	if err != nil {
		t.Fatalf("NewOIDCProvider: %v", err)
	}

	fakeGH := fakegithub.Start(t, "gh-console", "gh-console-secret")
	ghProvider, err := identity.NewGitHubProvider(identity.GitHubConfig{
		ClientID:     "gh-console",
		ClientSecret: "gh-console-secret",
		RedirectURL:  "http://127.0.0.1:8081/api/v1/auth/github/callback",
		AuthURL:      fakeGH.AuthURL(),
		TokenURL:     fakeGH.TokenURL(),
		APIBaseURL:   fakeGH.APIBaseURL(),
		HTTPClient:   fakeGH.Client(),
	})
	if err != nil {
		t.Fatalf("NewGitHubProvider: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	handler := console.New(logger, store, 5*time.Second, prometheus.NewRegistry(), console.Options{
		Keys:       store,
		Authorizer: identity.NewMapAuthorizer(),
		OIDC:       oidcProvider,
		GitHub:     ghProvider,
		Sessions:   store,
		Members:    store,
	}).Routes()

	// 1. OIDC 与 GitHub 同一邮箱登录，各自建独立用户。
	fakeOIDC.SetUser(fakeoidc.User{
		Subject:       "sub-alice-oidc",
		Email:         "alice@example.com",
		EmailVerified: true,
		Name:          "Alice OIDC",
	})
	_, oidcUserID, _ := loginViaFakeOIDC(t, handler)

	fakeGH.SetUser(fakegithub.User{
		ID:     10001,
		Login:  "alice-gh",
		Name:   "Alice GH",
		Emails: []fakegithub.Email{{Email: "alice@example.com", Primary: true, Verified: true}},
	})
	_, ghUserID, _ := loginViaFakeGitHub(t, handler)
	if oidcUserID == ghUserID {
		t.Fatalf("different providers merged into same user %d by email", oidcUserID)
	}

	// 2. GitHub 邀请兑现只认 primary && verified 邮箱。
	if _, err := store.CreateInvitation(ctx, CreateInvitationParams{
		TenantID: tenant1,
		Email:    "dev@example.com",
		Role:     identity.RoleMember,
	}); err != nil {
		t.Fatalf("CreateInvitation: %v", err)
	}

	fakeGH.SetUser(fakegithub.User{
		ID:    2026,
		Login: "dev-gh",
		Name:  "Dev GH",
		Emails: []fakegithub.Email{
			{Email: "dev@example.com", Primary: true, Verified: false},
		},
	})
	unverifiedCookie, devUserID1, _ := loginViaFakeGitHub(t, handler)

	readMe := func(sess *http.Cookie) []struct {
		ID   int64  `json:"id"`
		Role string `json:"role"`
	} {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
		req.AddCookie(sess)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /me = %d, body = %s", rec.Code, rec.Body.String())
		}
		var body struct {
			Organizations []struct {
				ID   int64  `json:"id"`
				Role string `json:"role"`
			} `json:"organizations"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode /me: %v", err)
		}
		return body.Organizations
	}
	if orgs := readMe(unverifiedCookie); len(orgs) != 0 {
		t.Fatalf("unverified GitHub login redeemed invitation: orgs=%+v", orgs)
	}

	fakeGH.SetUser(fakegithub.User{
		ID:    2026,
		Login: "dev-gh",
		Name:  "Dev GH",
		Emails: []fakegithub.Email{
			{Email: "dev@example.com", Primary: true, Verified: true},
		},
	})
	verifiedCookie, devUserID2, _ := loginViaFakeGitHub(t, handler)
	if devUserID2 != devUserID1 {
		t.Fatalf("same GitHub ID created new user %d (want %d)", devUserID2, devUserID1)
	}
	orgs := readMe(verifiedCookie)
	if len(orgs) != 1 || orgs[0].ID != tenant1 || orgs[0].Role != "member" {
		t.Fatalf("verified GitHub orgs = %+v, want tenant1(member)", orgs)
	}

	createReq := httptest.NewRequest(http.MethodPost, "/api/v1/links", strings.NewReader(`{"url":"https://example.com/from-github"}`))
	createReq.AddCookie(verifiedCookie)
	createReq.Header.Set("X-Org-ID", strconv.FormatInt(tenant1, 10))
	createRec := httptest.NewRecorder()
	handler.ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusCreated {
		t.Fatalf("POST /links with GitHub session = %d, want 201, body = %s", createRec.Code, createRec.Body.String())
	}
}

/*
listAllCodes 用 limit=1 逐页跟 next_cursor，直到没有下一页。
游标原样放进 query，服务端按 sort 解码后再交给 SQL。
*/
