package httpx

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"

	"shortlink/internal/logx"
)

func setupHTTPTestTracer(t *testing.T) *tracetest.InMemoryExporter {
	t.Helper()
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() {
		_ = tp.Shutdown(t.Context())
		otel.SetTracerProvider(noop.NewTracerProvider())
		otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator())
	})
	return exp
}

func TestObserveTracingPropagationAndLogCorrelation(t *testing.T) {
	exp := setupHTTPTestTracer(t)

	var buf bytes.Buffer
	logger, _ := logx.New(&buf, logx.Options{Format: logx.FormatJSON, Level: slog.LevelInfo, Service: "redirector"})
	srv := New(logger, 5*time.Second, newTestRegistry())
	handler := srv.Handler(func(mux *http.ServeMux) {
		mux.HandleFunc("GET /r/{code}", func(w http.ResponseWriter, r *http.Request) {
			logx.SetActor(r.Context(), 42, "api_key", 9)
			logx.SetCacheResult(r.Context(), "hit")
			http.Redirect(w, r, "https://example.com", http.StatusFound)
		})
	})

	req := httptest.NewRequest(http.MethodGet, "/r/AbC123xy", nil)
	req.Header.Set("traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", rec.Code)
	}

	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("spans = %d, want 1", len(spans))
	}
	sp := spans[0]
	if sp.Name != "GET /r/{code}" {
		t.Errorf("span name = %q, want GET /r/{code}", sp.Name)
	}
	if sp.SpanKind != trace.SpanKindServer {
		t.Errorf("span kind = %v, want server", sp.SpanKind)
	}
	if sp.SpanContext.TraceID().String() != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Errorf("trace_id = %s, want upstream trace id", sp.SpanContext.TraceID())
	}
	if sp.Parent.SpanID().String() != "00f067aa0ba902b7" {
		t.Errorf("parent span_id = %s, want 00f067aa0ba902b7", sp.Parent.SpanID())
	}
	if sp.Status.Code != codes.Unset {
		t.Errorf("status code = %v, want Unset for 302", sp.Status.Code)
	}

	attrs := attrMap(sp.Attributes)
	if attrs["http.route"] != "/r/{code}" || attrs["http.response.status_code"] != int64(302) ||
		attrs["shortlink.tenant_id"] != int64(42) || attrs["shortlink.actor.kind"] != "api_key" ||
		attrs["shortlink.cache.result"] != "hit" {
		t.Errorf("unexpected span attributes: %+v", attrs)
	}

	var entry map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &entry); err != nil {
		t.Fatalf("decode access log: %v\n%s", err, buf.String())
	}
	if entry["trace_id"] != sp.SpanContext.TraceID().String() {
		t.Errorf("access log trace_id = %v, want %s", entry["trace_id"], sp.SpanContext.TraceID())
	}
	if entry["span_id"] != sp.SpanContext.SpanID().String() {
		t.Errorf("access log span_id = %v, want %s", entry["span_id"], sp.SpanContext.SpanID())
	}
}

func TestObserveTracingSkipsProbesAndAvoidsHighCardinalityOn404(t *testing.T) {
	exp := setupHTTPTestTracer(t)

	srv := newTestServer()
	srv.SetReadiness()
	handler := srv.Handler(nil)

	for _, path := range []string{"/healthz", "/readyz", "/metrics"} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d, want 200", path, rec.Code)
		}
	}
	if spans := exp.GetSpans(); len(spans) != 0 {
		t.Fatalf("probes and metrics produced %d spans, want 0", len(spans))
	}

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/random-unmatched-path-12345", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET unmatched = %d, want 404", rec.Code)
	}

	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("spans = %d, want 1", len(spans))
	}
	if spans[0].Name != "GET" {
		t.Errorf("unmatched span name = %q, want GET", spans[0].Name)
	}
	if spans[0].Status.Code != codes.Unset {
		t.Errorf("404 span status = %v, want Unset", spans[0].Status.Code)
	}
	attrs := attrMap(spans[0].Attributes)
	if _, ok := attrs["http.route"]; ok {
		t.Errorf("unmatched route should not set http.route: %+v", attrs)
	}
}

func TestObserveTracingMarks5xxAndPanicAsError(t *testing.T) {
	exp := setupHTTPTestTracer(t)

	var buf bytes.Buffer
	logger, _ := logx.New(&buf, logx.Options{Format: logx.FormatJSON, Level: slog.LevelInfo, Service: "console"})
	srv := New(logger, 5*time.Second, newTestRegistry())
	handler := srv.Handler(func(mux *http.ServeMux) {
		mux.HandleFunc("GET /boom", func(http.ResponseWriter, *http.Request) {
			panic("boom")
		})
	})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/boom", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("GET /boom = %d, want 500", rec.Code)
	}

	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("spans = %d, want 1", len(spans))
	}
	if spans[0].Status.Code != codes.Error || spans[0].Status.Description != "panic" {
		t.Errorf("span status = (%v, %q), want (Error, panic)", spans[0].Status.Code, spans[0].Status.Description)
	}

	// recoverer 与 access log 两条日志都应带上同一个 trace_id。
	lines := bytes.Split(bytes.TrimSpace(buf.Bytes()), []byte("\n"))
	if len(lines) != 2 {
		t.Fatalf("log lines = %d, want 2: %s", len(lines), buf.String())
	}
	for i, line := range lines {
		var entry map[string]any
		if err := json.Unmarshal(line, &entry); err != nil {
			t.Fatalf("decode line %d: %v", i, err)
		}
		if entry["trace_id"] != spans[0].SpanContext.TraceID().String() {
			t.Errorf("line %d trace_id = %v, want %s", i, entry["trace_id"], spans[0].SpanContext.TraceID())
		}
	}
}

func attrMap(attrs []attribute.KeyValue) map[string]any {
	out := make(map[string]any, len(attrs))
	for _, a := range attrs {
		out[string(a.Key)] = a.Value.AsInterface()
	}
	return out
}
