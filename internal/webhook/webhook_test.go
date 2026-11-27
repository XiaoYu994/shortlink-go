package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"

	"shortlink/internal/job"
	"shortlink/internal/safehttp"
)

func TestSecretCipherRoundTripAndAAD(t *testing.T) {
	key, err := ParseMasterKeyHex(strings.Repeat("ab", 32))
	if err != nil {
		t.Fatalf("ParseMasterKeyHex: %v", err)
	}
	cipher, err := NewSecretCipher(key)
	if err != nil {
		t.Fatalf("NewSecretCipher: %v", err)
	}

	plain, prefix, err := GenerateSecret()
	if err != nil {
		t.Fatalf("GenerateSecret: %v", err)
	}
	if !strings.HasPrefix(plain, SecretTokenPrefix) || len(plain) != 70 || prefix != plain[:12] {
		t.Fatalf("secret = (%q, %q), unexpected shape", plain, prefix)
	}

	ct, err := cipher.Encrypt(7, plain)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	got, err := cipher.Decrypt(7, ct)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if got != plain {
		t.Fatalf("Decrypt = %q, want %q", got, plain)
	}

	// 跨租户 AAD 替换必须失败。
	if _, err := cipher.Decrypt(8, ct); !errors.Is(err, ErrDecryptSecret) {
		t.Fatalf("Decrypt wrong tenant = %v, want ErrDecryptSecret", err)
	}
	// 篡改密文与错误版本必须失败。
	tampered := append([]byte(nil), ct...)
	tampered[len(tampered)-1] ^= 0xff
	if _, err := cipher.Decrypt(7, tampered); !errors.Is(err, ErrDecryptSecret) {
		t.Fatalf("Decrypt tampered = %v, want ErrDecryptSecret", err)
	}
	badVer := append([]byte(nil), ct...)
	badVer[0] = 0x02
	if _, err := cipher.Decrypt(7, badVer); !errors.Is(err, ErrDecryptSecret) {
		t.Fatalf("Decrypt bad version = %v, want ErrDecryptSecret", err)
	}
}

func TestSignAndVerifySignature(t *testing.T) {
	secret := SecretTokenPrefix + strings.Repeat("12", 32)
	now := time.Unix(1700000000, 0).UTC()
	payload := []byte(`{"id":"evt_1","event":"link.created"}`)

	header := Sign(secret, now, payload)
	if err := VerifySignature(secret, header, payload, now.Add(time.Minute), DefaultReplayTolerance); err != nil {
		t.Fatalf("VerifySignature valid failed: %v", err)
	}
	if err := VerifySignature(secret, header, []byte(`{"id":"evt_2"}`), now, DefaultReplayTolerance); !errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("tampered payload = %v, want ErrInvalidSignature", err)
	}
	if err := VerifySignature(secret, header, payload, now.Add(10*time.Minute), DefaultReplayTolerance); !errors.Is(err, ErrSignatureExpired) {
		t.Fatalf("expired = %v, want ErrSignatureExpired", err)
	}
	if err := VerifySignature("wrong", header, payload, now, DefaultReplayTolerance); !errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("wrong secret = %v, want ErrInvalidSignature", err)
	}
}

func TestNormalizeEventsAndDescription(t *testing.T) {
	norm, err := NormalizeSubscribableEvents([]EventType{EventLinkBanned, EventLinkCreated, EventLinkBanned})
	if err != nil {
		t.Fatalf("NormalizeSubscribableEvents: %v", err)
	}
	if len(norm) != 2 || norm[0] != EventLinkCreated || norm[1] != EventLinkBanned {
		t.Fatalf("normalized = %v, want [link.created link.banned]", norm)
	}
	if _, err := NormalizeSubscribableEvents([]EventType{EventPing}); !errors.Is(err, ErrInvalidEvents) {
		t.Fatalf("ping subscribable err = %v, want ErrInvalidEvents", err)
	}
	if _, err := NormalizeSubscribableEvents(nil); !errors.Is(err, ErrInvalidEvents) {
		t.Fatalf("empty events err = %v, want ErrInvalidEvents", err)
	}
	if _, err := NormalizeDescription("bad\ndesc"); !errors.Is(err, ErrInvalidDescription) {
		t.Fatalf("control desc err = %v, want ErrInvalidDescription", err)
	}
}

type fakeDeliveryStore struct {
	target       DeliveryTarget
	getErr       error
	recordErr    error
	recorded     []RecordDeliveryParams
	autoDisabled bool
}

func (f *fakeDeliveryStore) GetWebhookForDelivery(_ context.Context, _ int64) (DeliveryTarget, error) {
	if f.getErr != nil {
		return DeliveryTarget{}, f.getErr
	}
	return f.target, nil
}

func (f *fakeDeliveryStore) RecordWebhookDelivery(_ context.Context, p RecordDeliveryParams) (bool, error) {
	f.recorded = append(f.recorded, p)
	if f.recordErr != nil {
		return false, f.recordErr
	}
	return f.autoDisabled, nil
}

func TestHandlerBehaviorMatrix(t *testing.T) {
	key, _ := ParseMasterKeyHex(strings.Repeat("cd", 32))
	secretCipher, _ := NewSecretCipher(key)
	secretPlain, _, _ := GenerateSecret()
	ct, _ := secretCipher.Encrypt(9, secretPlain)

	t.Run("returns permanent error without querying store when cipher is nil", func(t *testing.T) {
		store := &fakeDeliveryStore{}
		h := NewHandler(HandlerOptions{Store: store})
		err := h(context.Background(), job.Job{ID: 1, Attempts: 1, MaxAttempts: 6, Payload: []byte(`{}`)})
		if !job.IsPermanent(err) {
			t.Fatalf("err = %v, want job.Permanent", err)
		}
		if _, deferred := job.DeferDelay(err); deferred {
			t.Fatalf("err = %v must not be job.Defer", err)
		}
		if len(store.recorded) != 0 {
			t.Fatalf("recorded deliveries = %d, want 0", len(store.recorded))
		}
	})

	t.Run("skips deleted or disabled non-ping webhook", func(t *testing.T) {
		h := NewHandler(HandlerOptions{
			Store:  &fakeDeliveryStore{getErr: ErrNotFound},
			Cipher: secretCipher,
		})
		payload, _ := json.Marshal(DeliverPayload{WebhookID: 1, EventID: "evt_1", Event: EventLinkCreated, Data: json.RawMessage(`{}`)})
		if err := h(context.Background(), job.Job{ID: 1, Payload: payload}); !job.IsSkipped(err) {
			t.Fatalf("deleted err = %v, want job.Skip", err)
		}

		disabledStore := &fakeDeliveryStore{target: DeliveryTarget{ID: 1, TenantID: 9, URL: "https://example.com/hook", Enabled: false, SecretCiphertext: ct}}
		h2 := NewHandler(HandlerOptions{Store: disabledStore, Cipher: secretCipher})
		if err := h2(context.Background(), job.Job{ID: 2, Payload: payload}); !job.IsSkipped(err) {
			t.Fatalf("disabled err = %v, want job.Skip", err)
		}
	})

	t.Run("signs envelope and verifies delivery headers across ping even when disabled", func(t *testing.T) {
		var gotSig, gotEvent, gotDelivery string
		var gotBody []byte
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotSig = r.Header.Get(HeaderSignature)
			gotEvent = r.Header.Get(HeaderEvent)
			gotDelivery = r.Header.Get(HeaderDelivery)
			gotBody, _ = io.ReadAll(r.Body)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("accepted"))
		}))
		defer srv.Close()

		store := &fakeDeliveryStore{target: DeliveryTarget{ID: 5, TenantID: 9, URL: srv.URL, Enabled: false, SecretCiphertext: ct}}
		h := NewHandler(HandlerOptions{
			Store:      store,
			HTTPClient: safehttp.NewClient(safehttp.ClientOptions{Timeout: time.Second, DisableRedirects: true, AllowedHostPorts: []string{srv.Listener.Addr().String()}}),
			Cipher:     secretCipher,
			Registry:   prometheus.NewRegistry(),
		})
		payload, _ := json.Marshal(DeliverPayload{WebhookID: 5, EventID: "evt_ping", Event: EventPing, OccurredAt: time.Now().UTC(), Data: json.RawMessage(`{"ping":true}`)})
		if err := h(context.Background(), job.Job{ID: 88, Attempts: 1, MaxAttempts: 1, Payload: payload}); err != nil {
			t.Fatalf("ping handler err = %v", err)
		}
		if gotEvent != string(EventPing) || gotDelivery != "dlv_88" {
			t.Fatalf("headers = (%q, %q)", gotEvent, gotDelivery)
		}
		if err := VerifySignature(secretPlain, gotSig, gotBody, time.Now().UTC(), DefaultReplayTolerance); err != nil {
			t.Fatalf("VerifySignature failed: %v", err)
		}
		if len(store.recorded) != 1 || !store.recorded[0].IsPing || store.recorded[0].Status != StatusSuccess {
			t.Fatalf("recorded = %+v", store.recorded)
		}
	})

	t.Run("classifies 302 permanent, 404 permanent, 500 retry, and final attempt terminal failure", func(t *testing.T) {
		var nextStatus int
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if nextStatus == http.StatusFound {
				w.Header().Set("Location", "https://other.example.com")
			}
			w.WriteHeader(nextStatus)
			_, _ = w.Write([]byte("resp"))
		}))
		defer srv.Close()

		store := &fakeDeliveryStore{target: DeliveryTarget{ID: 5, TenantID: 9, URL: srv.URL, Enabled: true, SecretCiphertext: ct}}
		h := NewHandler(HandlerOptions{
			Store:          store,
			HTTPClient:     safehttp.NewClient(safehttp.ClientOptions{Timeout: time.Second, DisableRedirects: true, AllowedHostPorts: []string{srv.Listener.Addr().String()}}),
			Cipher:         secretCipher,
			RetryBaseDelay: 10 * time.Second,
			RetryMaxDelay:  10 * time.Minute,
		})
		payload, _ := json.Marshal(DeliverPayload{WebhookID: 5, EventID: "evt_2", Event: EventLinkCreated, OccurredAt: time.Now().UTC(), Data: json.RawMessage(`{"code":"abc"}`)})

		nextStatus = http.StatusFound
		err302 := h(context.Background(), job.Job{ID: 10, Attempts: 1, MaxAttempts: 6, Payload: payload})
		if !job.IsPermanent(err302) {
			t.Fatalf("302 err = %v, want Permanent", err302)
		}

		nextStatus = http.StatusNotFound
		err404 := h(context.Background(), job.Job{ID: 11, Attempts: 1, MaxAttempts: 6, Payload: payload})
		if !job.IsPermanent(err404) {
			t.Fatalf("404 err = %v, want Permanent", err404)
		}

		nextStatus = http.StatusInternalServerError
		err500Retry := h(context.Background(), job.Job{ID: 12, Attempts: 1, MaxAttempts: 6, Payload: payload})
		if delay, ok := job.RetryAfterDelay(err500Retry); !ok || delay < 8*time.Second || delay > 10*time.Second {
			t.Fatalf("500 attempt 1 = (%v, delay=%v, ok=%v), want RetryAfter in [8s,10s]", err500Retry, delay, ok)
		}
		if got := store.recorded[len(store.recorded)-1]; got.Status != StatusRetry || got.TerminalFailure {
			t.Fatalf("500 attempt 1 record = %+v, want retry non-terminal", got)
		}

		err500Final := h(context.Background(), job.Job{ID: 13, Attempts: 6, MaxAttempts: 6, Payload: payload})
		if err500Final == nil || job.IsPermanent(err500Final) {
			t.Fatalf("500 final attempt = %v, want non-permanent error for Runner exhaustion", err500Final)
		}
		if got := store.recorded[len(store.recorded)-1]; got.Status != StatusFailed || !got.TerminalFailure {
			t.Fatalf("500 final record = %+v, want failed terminal", got)
		}
	})

	t.Run("pre-canceled ctx skips downstream store, limiter, HTTP call and delivery record", func(t *testing.T) {
		httpCalls := 0
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			httpCalls++
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()

		store := &fakeDeliveryStore{target: DeliveryTarget{ID: 5, TenantID: 9, URL: srv.URL, Enabled: true, SecretCiphertext: ct}}
		h := NewHandler(HandlerOptions{
			Store:      store,
			HTTPClient: safehttp.NewClient(safehttp.ClientOptions{Timeout: time.Second, DisableRedirects: true, AllowedHostPorts: []string{srv.Listener.Addr().String()}}),
			Limiter:    safehttp.NewHostLimiter(1),
			Cipher:     secretCipher,
		})
		payload, _ := json.Marshal(DeliverPayload{WebhookID: 5, EventID: "evt_cancel_1", Event: EventLinkCreated, OccurredAt: time.Now().UTC(), Data: json.RawMessage(`{"code":"abc"}`)})

		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		err := h(ctx, job.Job{ID: 14, Attempts: 6, MaxAttempts: 6, Payload: payload})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
		if httpCalls != 0 {
			t.Fatalf("httpCalls = %d, want 0", httpCalls)
		}
		if len(store.recorded) != 0 {
			t.Fatalf("recorded = %d, want 0", len(store.recorded))
		}
	})

	t.Run("in-flight ctx cancellation returns context.Canceled without recording terminal failure", func(t *testing.T) {
		started := make(chan struct{})
		release := make(chan struct{})
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			close(started)
			select {
			case <-r.Context().Done():
			case <-release:
			}
		}))
		// LIFO：先 close(release) 放行仍在等待的 handler，再 srv.Close() 避免测试收尾阻塞。
		defer srv.Close()
		defer close(release)

		store := &fakeDeliveryStore{target: DeliveryTarget{ID: 5, TenantID: 9, URL: srv.URL, Enabled: true, SecretCiphertext: ct}}
		h := NewHandler(HandlerOptions{
			Store:      store,
			HTTPClient: safehttp.NewClient(safehttp.ClientOptions{Timeout: 5 * time.Second, DisableRedirects: true, AllowedHostPorts: []string{srv.Listener.Addr().String()}}),
			Cipher:     secretCipher,
		})
		payload, _ := json.Marshal(DeliverPayload{WebhookID: 5, EventID: "evt_cancel_2", Event: EventLinkCreated, OccurredAt: time.Now().UTC(), Data: json.RawMessage(`{"code":"abc"}`)})

		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			<-started
			cancel()
		}()

		err := h(ctx, job.Job{ID: 15, Attempts: 6, MaxAttempts: 6, Payload: payload})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
		if len(store.recorded) != 0 {
			t.Fatalf("recorded = %+v, want 0 records on shutdown cancel", store.recorded)
		}
	})

	t.Run("preserves Permanent and RetryAfter semantics when RecordWebhookDelivery fails", func(t *testing.T) {
		var nextStatus int
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(nextStatus)
		}))
		defer srv.Close()

		store := &fakeDeliveryStore{
			target:    DeliveryTarget{ID: 5, TenantID: 9, URL: srv.URL, Enabled: true, SecretCiphertext: ct},
			recordErr: errors.New("db hiccup"),
		}
		h := NewHandler(HandlerOptions{
			Store:          store,
			HTTPClient:     safehttp.NewClient(safehttp.ClientOptions{Timeout: time.Second, DisableRedirects: true, AllowedHostPorts: []string{srv.Listener.Addr().String()}}),
			Cipher:         secretCipher,
			RetryBaseDelay: 10 * time.Second,
			RetryMaxDelay:  10 * time.Minute,
		})
		payload, _ := json.Marshal(DeliverPayload{WebhookID: 5, EventID: "evt_rec_err", Event: EventLinkCreated, OccurredAt: time.Now().UTC(), Data: json.RawMessage(`{"code":"abc"}`)})

		nextStatus = http.StatusNotFound
		if err := h(context.Background(), job.Job{ID: 16, Attempts: 1, MaxAttempts: 6, Payload: payload}); !job.IsPermanent(err) {
			t.Fatalf("404 with recordErr = %v, want Permanent", err)
		}

		nextStatus = http.StatusInternalServerError
		err500 := h(context.Background(), job.Job{ID: 17, Attempts: 1, MaxAttempts: 6, Payload: payload})
		if delay, ok := job.RetryAfterDelay(err500); !ok || delay < 8*time.Second || delay > 10*time.Second {
			t.Fatalf("500 with recordErr = (%v, delay=%v, ok=%v), want RetryAfter in [8s,10s]", err500, delay, ok)
		}
	})
}

func TestWebhookTracingDoesNotLeakTraceparentHeader(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() {
		_ = tp.Shutdown(t.Context())
		otel.SetTracerProvider(noop.NewTracerProvider())
		otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator())
	})

	key, _ := ParseMasterKeyHex(strings.Repeat("cd", 32))
	secretCipher, _ := NewSecretCipher(key)
	secretPlain, _, _ := GenerateSecret()
	ct, _ := secretCipher.Encrypt(9, secretPlain)

	var gotTraceparent string
	var statusToReturn int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotTraceparent = r.Header.Get("traceparent")
		w.WriteHeader(statusToReturn)
	}))
	defer srv.Close()

	store := &fakeDeliveryStore{target: DeliveryTarget{ID: 5, TenantID: 9, URL: srv.URL, Enabled: true, SecretCiphertext: ct}}
	h := NewHandler(HandlerOptions{
		Store:      store,
		HTTPClient: safehttp.NewClient(safehttp.ClientOptions{Timeout: time.Second, DisableRedirects: true, AllowedHostPorts: []string{srv.Listener.Addr().String()}}),
		Cipher:     secretCipher,
	})
	payload, _ := json.Marshal(DeliverPayload{WebhookID: 5, EventID: "evt_trace", Event: EventLinkCreated, OccurredAt: time.Now().UTC(), Data: json.RawMessage(`{"code":"abc"}`)})

	ctx, root := otel.Tracer("test").Start(context.Background(), "job.process webhook.deliver")
	statusToReturn = http.StatusOK
	if err := h(ctx, job.Job{ID: 21, Attempts: 1, MaxAttempts: 6, Payload: payload}); err != nil {
		t.Fatalf("200 handler err = %v", err)
	}
	if gotTraceparent != "" {
		t.Errorf("outbound webhook request leaked traceparent header %q", gotTraceparent)
	}

	statusToReturn = http.StatusInternalServerError
	_ = h(ctx, job.Job{ID: 22, Attempts: 1, MaxAttempts: 6, Payload: payload})
	root.End()

	spans := exp.GetSpans()
	var sawOK, sawErr bool
	for _, sp := range spans {
		if sp.Name != "webhook.http_deliver" {
			continue
		}
		if sp.SpanKind != trace.SpanKindClient {
			t.Errorf("span kind = %v, want client", sp.SpanKind)
		}
		switch sp.Status.Code {
		case codes.Unset:
			sawOK = true
		case codes.Error:
			sawErr = true
		}
	}
	if !sawOK || !sawErr {
		t.Errorf("expected both Unset and Error webhook.http_deliver spans: ok=%v err=%v", sawOK, sawErr)
	}
}
