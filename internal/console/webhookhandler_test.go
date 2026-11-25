package console

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"shortlink/internal/identity"
	"shortlink/internal/safehttp"
	"shortlink/internal/webhook"
)

type stubWebhookStore struct {
	items      []webhook.Subscription
	deliveries []webhook.Delivery
	createErr  error
	rotateErr  error
	deleteErr  error
	testResult webhook.TestEnqueueResult

	gotCreate  *webhook.CreateParams
	gotUpdate  *webhook.UpdateParams
	gotDeliver *webhook.DeliveryListOptions
}

func (s *stubWebhookStore) ListWebhooks(_ context.Context, _ int64) ([]webhook.Subscription, error) {
	return s.items, nil
}

func (s *stubWebhookStore) CreateWebhook(_ context.Context, p webhook.CreateParams) (webhook.Subscription, error) {
	s.gotCreate = &p
	if s.createErr != nil {
		return webhook.Subscription{}, s.createErr
	}
	now := time.Now().UTC()
	return webhook.Subscription{
		ID:           10,
		TenantID:     p.TenantID,
		URL:          p.URL,
		Description:  p.Description,
		Events:       p.Events,
		SecretPrefix: p.SecretPrefix,
		Enabled:      true,
		CreatedBy:    p.CreatedBy,
		CreatedAt:    now,
		UpdatedAt:    now,
	}, nil
}

func (s *stubWebhookStore) UpdateWebhook(_ context.Context, p webhook.UpdateParams) (webhook.Subscription, error) {
	s.gotUpdate = &p
	now := time.Now().UTC()
	return webhook.Subscription{
		ID:           p.WebhookID,
		TenantID:     p.TenantID,
		URL:          "https://hooks.example.com/updated",
		Events:       []webhook.EventType{webhook.EventLinkCreated},
		SecretPrefix: "whsec_abcdef",
		Enabled:      p.Enabled == nil || *p.Enabled,
		CreatedAt:    now,
		UpdatedAt:    now,
	}, nil
}

func (s *stubWebhookStore) DeleteWebhook(_ context.Context, _, _ int64) error {
	return s.deleteErr
}

func (s *stubWebhookStore) RotateWebhookSecret(_ context.Context, tenantID, webhookID int64, _ []byte, prefix string) (webhook.Subscription, error) {
	if s.rotateErr != nil {
		return webhook.Subscription{}, s.rotateErr
	}
	now := time.Now().UTC()
	return webhook.Subscription{
		ID:           webhookID,
		TenantID:     tenantID,
		URL:          "https://hooks.example.com/hook",
		Events:       []webhook.EventType{webhook.EventLinkCreated},
		SecretPrefix: prefix,
		Enabled:      true,
		CreatedAt:    now,
		UpdatedAt:    now,
	}, nil
}

func (s *stubWebhookStore) EnqueueWebhookTest(_ context.Context, _, _ int64) (webhook.TestEnqueueResult, error) {
	if s.testResult.JobID != 0 {
		return s.testResult, nil
	}
	return webhook.TestEnqueueResult{JobID: 77, DeliveryID: "dlv_77"}, nil
}

func (s *stubWebhookStore) ListWebhookDeliveries(_ context.Context, opts webhook.DeliveryListOptions) ([]webhook.Delivery, error) {
	s.gotDeliver = &opts
	return s.deliveries, nil
}

func newTestWebhookCipher(t *testing.T) *webhook.SecretCipher {
	t.Helper()
	key, err := webhook.ParseMasterKeyHex(strings.Repeat("12", 32))
	if err != nil {
		t.Fatalf("ParseMasterKeyHex: %v", err)
	}
	c, err := webhook.NewSecretCipher(key)
	if err != nil {
		t.Fatalf("NewSecretCipher: %v", err)
	}
	return c
}

func TestWebhookRoutesRBACValidationAndLifecycle(t *testing.T) {
	cipher := newTestWebhookCipher(t)
	now := time.Now().UTC()
	sessToken := "sls_" + strings.Repeat("bb", 32)

	t.Run("webhooks disabled when store or cipher unset", func(t *testing.T) {
		srv := New(selfServiceLogger(), &stubStore{}, 2*time.Second, prometheus.NewRegistry(), Options{
			Authorizer: identity.NewMapAuthorizer(),
			Sessions:   &selfServiceSessionStore{userID: 1, role: identity.RoleOwner, orgID: 1},
		})
		h := srv.Routes()

		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/capabilities", nil))
		var caps capabilitiesResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &caps); err != nil {
			t.Fatalf("decode capabilities: %v", err)
		}
		if caps.Features.Webhooks {
			t.Fatal("expected capabilities.Features.Webhooks=false when webhooks module is absent")
		}

		rec2 := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/v1/webhooks", nil)
		req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: sessToken})
		req.Header.Set("X-Org-ID", "1")
		h.ServeHTTP(rec2, req)
		if rec2.Code != http.StatusNotFound {
			t.Fatalf("disabled GET /webhooks = %d, want 404", rec2.Code)
		}
	})

	whStore := &stubWebhookStore{
		items: []webhook.Subscription{{
			ID: 1, TenantID: 1, URL: "https://hooks.example.com/a", Events: []webhook.EventType{webhook.EventLinkCreated},
			SecretPrefix: "whsec_112233", Enabled: true, CreatedAt: now, UpdatedAt: now,
		}},
		deliveries: []webhook.Delivery{
			{ID: 3, WebhookID: 1, JobID: 30, DeliveryID: "dlv_30", Event: webhook.EventLinkCreated, Attempt: 1, Status: webhook.StatusSuccess, DurationMs: 12, CreatedAt: now},
			{ID: 2, WebhookID: 1, JobID: 29, DeliveryID: "dlv_29", Event: webhook.EventPing, Attempt: 1, Status: webhook.StatusFailed, DurationMs: 50, ErrorMessage: "http 500", CreatedAt: now},
		},
	}

	makeServer := func(role identity.Role) http.Handler {
		srv := New(selfServiceLogger(), &stubStore{}, 2*time.Second, prometheus.NewRegistry(), Options{
			Authorizer: identity.NewMapAuthorizer(),
			Keys:       selfServiceAPIKeyAuth{scopes: identity.AllAPIKeyScopes()},
			Sessions:   &selfServiceSessionStore{userID: 7, role: role, orgID: 1},
			Modules:    []Module{NewWebhooksModule(whStore, cipher, safehttp.URLValidationOptions{})},
		})
		return srv.Routes()
	}

	// API Key、Member、Viewer 一律 403。
	ownerHandler := makeServer(identity.RoleOwner)
	recAPIKey := httptest.NewRecorder()
	reqAPIKey := httptest.NewRequest(http.MethodGet, "/api/v1/webhooks", nil)
	reqAPIKey.Header.Set("Authorization", "Bearer slk_"+strings.Repeat("aa", 32))
	ownerHandler.ServeHTTP(recAPIKey, reqAPIKey)
	if recAPIKey.Code != http.StatusForbidden {
		t.Fatalf("API key GET /webhooks = %d, want 403", recAPIKey.Code)
	}

	for _, role := range []identity.Role{identity.RoleMember, identity.RoleViewer} {
		h := makeServer(role)
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/v1/webhooks", nil)
		req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: sessToken})
		req.Header.Set("X-Org-ID", "1")
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s GET /webhooks = %d, want 403", role, rec.Code)
		}
	}

	// 入口 SSRF 拦截回环与非法事件。
	for _, body := range []string{
		`{"url":"http://127.0.0.1/hook","events":["link.created"]}`,
		`{"url":"https://hooks.example.com/a","events":["webhook.ping"]}`,
		`{"url":"https://hooks.example.com/a","events":[]}`,
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks", strings.NewReader(body))
		req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: sessToken})
		req.Header.Set("X-Org-ID", "1")
		ownerHandler.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("POST invalid %s = %d, want 400 (%s)", body, rec.Code, rec.Body.String())
		}
	}
	if whStore.gotCreate != nil {
		t.Fatalf("gotCreate = %+v, want nil when RBAC or validation rejected request", whStore.gotCreate)
	}

	// 正常创建返回一次明文 secret 并加密落库。
	recCreate := httptest.NewRecorder()
	reqCreate := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks", strings.NewReader(`{"url":"https://hooks.example.com/events","description":"prod","events":["link.banned","link.created"]}`))
	reqCreate.AddCookie(&http.Cookie{Name: SessionCookieName, Value: sessToken})
	reqCreate.Header.Set("X-Org-ID", "1")
	ownerHandler.ServeHTTP(recCreate, reqCreate)
	if recCreate.Code != http.StatusCreated || recCreate.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("POST /webhooks = %d cc=%q body=%s", recCreate.Code, recCreate.Header().Get("Cache-Control"), recCreate.Body.String())
	}
	var created webhookWithSecretResponse
	if err := json.Unmarshal(recCreate.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode created webhook: %v", err)
	}
	if !strings.HasPrefix(created.Secret, webhook.SecretTokenPrefix) || created.SecretPrefix != created.Secret[:12] {
		t.Fatalf("created secret = (%q, %q)", created.Secret, created.SecretPrefix)
	}
	decrypted, err := cipher.Decrypt(1, whStore.gotCreate.SecretCiphertext)
	if err != nil || decrypted != created.Secret {
		t.Fatalf("stored ciphertext decrypt mismatch: err=%v", err)
	}

	// 测试 Ping 返回 202；deliveries 支持游标分页 limit=1 -> next_cursor="3"。
	recTest := httptest.NewRecorder()
	reqTest := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/1/test", nil)
	reqTest.AddCookie(&http.Cookie{Name: SessionCookieName, Value: sessToken})
	reqTest.Header.Set("X-Org-ID", "1")
	ownerHandler.ServeHTTP(recTest, reqTest)
	if recTest.Code != http.StatusAccepted {
		t.Fatalf("POST /webhooks/1/test = %d, want 202 (%s)", recTest.Code, recTest.Body.String())
	}

	recDlv := httptest.NewRecorder()
	reqDlv := httptest.NewRequest(http.MethodGet, "/api/v1/webhooks/1/deliveries?limit=1", nil)
	reqDlv.AddCookie(&http.Cookie{Name: SessionCookieName, Value: sessToken})
	reqDlv.Header.Set("X-Org-ID", "1")
	ownerHandler.ServeHTTP(recDlv, reqDlv)
	if recDlv.Code != http.StatusOK {
		t.Fatalf("GET deliveries = %d, want 200", recDlv.Code)
	}
	var dlvResp listWebhookDeliveriesResponse
	if err := json.Unmarshal(recDlv.Body.Bytes(), &dlvResp); err != nil {
		t.Fatalf("decode deliveries: %v", err)
	}
	if len(dlvResp.Items) != 1 || dlvResp.NextCursor != "3" {
		t.Fatalf("deliveries = %+v, want 1 item and next_cursor=3", dlvResp)
	}

	// devtest 允许配置的回环测试端口能创建成功。
	srvDev := New(selfServiceLogger(), &stubStore{}, 2*time.Second, prometheus.NewRegistry(), Options{
		Authorizer: identity.NewMapAuthorizer(),
		Sessions:   &selfServiceSessionStore{userID: 7, role: identity.RoleAdmin, orgID: 1},
		Modules: []Module{NewWebhooksModule(
			whStore,
			cipher,
			safehttp.URLValidationOptions{AllowedHostPorts: []string{"127.0.0.1:18090"}},
		)},
	})
	recDev := httptest.NewRecorder()
	reqDev := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks", strings.NewReader(`{"url":"http://127.0.0.1:18090/hook","events":["link.created"]}`))
	reqDev.AddCookie(&http.Cookie{Name: SessionCookieName, Value: sessToken})
	reqDev.Header.Set("X-Org-ID", "1")
	srvDev.Routes().ServeHTTP(recDev, reqDev)
	if recDev.Code != http.StatusCreated {
		t.Fatalf("admin devtest POST /webhooks = %d, want 201 (%s)", recDev.Code, recDev.Body.String())
	}
}
