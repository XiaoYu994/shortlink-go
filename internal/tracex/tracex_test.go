package tracex

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

func resetGlobalOTel(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		otel.SetTracerProvider(noop.NewTracerProvider())
		otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator())
	})
}

func TestInitDisabledByDefault(t *testing.T) {
	resetGlobalOTel(t)
	otel.SetTracerProvider(noop.NewTracerProvider())

	shutdown, enabled, err := Init(context.Background(), Options{
		ServiceName: "redirector",
	})
	if err != nil {
		t.Fatalf("Init returned error: %v", err)
	}
	if enabled {
		t.Fatal("expected enabled=false when Endpoint and Exporter are empty")
	}
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("noop shutdown error: %v", err)
	}
	if _, span := otel.Tracer("test").Start(context.Background(), "noop"); span.IsRecording() {
		t.Fatal("expected global tracer provider to remain noop")
	}
}

func TestInitInMemorySamplingAndServiceName(t *testing.T) {
	resetGlobalOTel(t)

	exp := tracetest.NewInMemoryExporter()
	shutdown, enabled, err := Init(context.Background(), Options{
		ServiceName: "console",
		SampleRate:  1.0,
		Exporter:    exp,
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("Init returned error: %v", err)
	}
	if !enabled {
		t.Fatal("expected enabled=true")
	}

	_, span := otel.Tracer("test").Start(context.Background(), "root-op")
	span.End()

	// tracetest.InMemoryExporter.Shutdown 会清空内存里的 span，因此先 ForceFlush 再 Shutdown。
	tp, ok := otel.GetTracerProvider().(*sdktrace.TracerProvider)
	if !ok {
		t.Fatalf("tracer provider type = %T, want *sdktrace.TracerProvider", otel.GetTracerProvider())
	}
	if err := tp.ForceFlush(context.Background()); err != nil {
		t.Fatalf("ForceFlush error: %v", err)
	}
	spans := exp.GetSpans()
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown error: %v", err)
	}

	if len(spans) != 1 {
		t.Fatalf("exported spans = %d, want 1", len(spans))
	}
	if spans[0].Name != "root-op" {
		t.Errorf("span name = %q, want root-op", spans[0].Name)
	}

	var gotService string
	for _, attr := range spans[0].Resource.Attributes() {
		if attr.Key == semconv.ServiceNameKey {
			gotService = attr.Value.AsString()
		}
	}
	if gotService != "console" {
		t.Errorf("service.name = %q, want console", gotService)
	}
}

func TestInitParentBasedZeroSampleRate(t *testing.T) {
	resetGlobalOTel(t)

	exp := tracetest.NewInMemoryExporter()
	shutdown, enabled, err := Init(context.Background(), Options{
		ServiceName: "redirector",
		SampleRate:  0.0,
		Exporter:    exp,
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("Init returned error: %v", err)
	}
	if !enabled {
		t.Fatal("expected enabled=true")
	}

	// 新建根 span 应按 0 采样率丢弃。
	_, rootSpan := otel.Tracer("test").Start(context.Background(), "dropped-root")
	if rootSpan.IsRecording() {
		t.Error("expected 0 sample rate root span not to record")
	}
	rootSpan.End()

	// 携带已采样远端父上下文的请求应按 ParentBased 保留。
	parentCtx := otel.GetTextMapPropagator().Extract(context.Background(), propagation.MapCarrier{
		"traceparent": "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
	})
	_, childSpan := otel.Tracer("test").Start(parentCtx, "sampled-child")
	if !childSpan.IsRecording() {
		t.Error("expected child of sampled remote parent to record")
	}
	childSpan.End()

	tp := otel.GetTracerProvider().(*sdktrace.TracerProvider)
	if err := tp.ForceFlush(context.Background()); err != nil {
		t.Fatalf("ForceFlush error: %v", err)
	}
	spans := exp.GetSpans()
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown error: %v", err)
	}

	if len(spans) != 1 || spans[0].Name != "sampled-child" {
		t.Fatalf("exported spans = %+v, want only sampled-child", spans)
	}
	wantTraceID, _ := trace.TraceIDFromHex("4bf92f3577b34da6a3ce929d0e0e4736")
	if spans[0].SpanContext.TraceID() != wantTraceID {
		t.Errorf("trace_id = %s, want %s", spans[0].SpanContext.TraceID(), wantTraceID)
	}
}

func TestInitOTLPHTTPExportOnShutdown(t *testing.T) {
	resetGlobalOTel(t)

	var (
		gotPath  atomic.Value
		gotBytes atomic.Int64
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath.Store(r.URL.Path)
		body, _ := io.ReadAll(r.Body)
		gotBytes.Add(int64(len(body)))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("{}"))
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	shutdown, enabled, err := Init(ctx, Options{
		ServiceName: "console-worker",
		Endpoint:    srv.URL,
		SampleRate:  1.0,
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("Init returned error: %v", err)
	}
	if !enabled {
		t.Fatal("expected enabled=true")
	}

	_, span := otel.Tracer("test").Start(context.Background(), "otlp-export")
	span.End()

	if err := shutdown(ctx); err != nil {
		t.Fatalf("shutdown error: %v", err)
	}

	if path, _ := gotPath.Load().(string); path != "/v1/traces" {
		t.Errorf("OTLP request path = %q, want /v1/traces", path)
	}
	if gotBytes.Load() == 0 {
		t.Error("expected non-empty OTLP protobuf payload")
	}
}
