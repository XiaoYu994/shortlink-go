package logx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"

	"go.opentelemetry.io/otel/trace"
)

func TestContextHandlerExtractsAndPreservesFields(t *testing.T) {
	var buf bytes.Buffer
	logger, _ := New(&buf, Options{Format: FormatJSON, Level: slog.LevelInfo, Service: "console"})

	// 无字段的普通 context 不输出空属性。
	logger.InfoContext(context.Background(), "plain")
	plain := decodeSingleJSON(t, buf.Bytes())
	for _, key := range []string{"request_id", "client_request_id", "tenant_id", "job_id", "job_kind", "trace_id", "span_id"} {
		if _, ok := plain[key]; ok {
			t.Errorf("unexpected %s in plain log: %+v", key, plain)
		}
	}
	if plain["service"] != "console" {
		t.Errorf("service = %v, want console", plain["service"])
	}

	// nil context 也不应 panic 或写入关联字段。
	buf.Reset()
	//nolint:staticcheck // SA1012: 显式测试传入 nil context 时的防崩溃行为
	logger.InfoContext(nil, "nil-ctx")
	nilEntry := decodeSingleJSON(t, buf.Bytes())
	if _, ok := nilEntry["trace_id"]; ok {
		t.Errorf("unexpected trace_id for nil ctx: %+v", nilEntry)
	}

	buf.Reset()
	traceID, _ := trace.TraceIDFromHex("4bf92f3577b34da6a3ce929d0e0e4736")
	spanID, _ := trace.SpanIDFromHex("00f067aa0ba902b7")
	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    traceID,
		SpanID:     spanID,
		TraceFlags: trace.FlagsSampled,
	})
	ctx := trace.ContextWithSpanContext(context.Background(), sc)
	ctx = WithRequestID(ctx, "req-42")
	ctx = WithClientRequestID(ctx, "gw-77")
	ctx = WithTenantID(ctx, 7)
	ctx = WithJob(ctx, 99, "link.risk_scan", 7)

	// With 必须继续保留 contextHandler 且保持字段在顶层。
	sub := logger.With("component", "risk")
	sub.InfoContext(ctx, "done")

	entry := decodeSingleJSON(t, buf.Bytes())
	if entry["request_id"] != "req-42" {
		t.Errorf("request_id = %v, want req-42", entry["request_id"])
	}
	if entry["client_request_id"] != "gw-77" {
		t.Errorf("client_request_id = %v, want gw-77", entry["client_request_id"])
	}
	if entry["tenant_id"] != float64(7) {
		t.Errorf("tenant_id = %v, want 7", entry["tenant_id"])
	}
	if entry["job_id"] != float64(99) {
		t.Errorf("job_id = %v, want 99", entry["job_id"])
	}
	if entry["job_kind"] != "link.risk_scan" {
		t.Errorf("job_kind = %v, want link.risk_scan", entry["job_kind"])
	}
	if entry["trace_id"] != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Errorf("trace_id = %v, want 4bf92f3577b34da6a3ce929d0e0e4736", entry["trace_id"])
	}
	if entry["span_id"] != "00f067aa0ba902b7" {
		t.Errorf("span_id = %v, want 00f067aa0ba902b7", entry["span_id"])
	}
	if entry["component"] != "risk" {
		t.Errorf("component = %v, want risk", entry["component"])
	}

	// WithGroup 之后，context 关联字段仍保持在顶层，只有业务记录属性落入子组内。
	buf.Reset()
	sub.WithGroup("meta").InfoContext(ctx, "grouped", "ok", true)
	grouped := decodeSingleJSON(t, buf.Bytes())
	if grouped["request_id"] != "req-42" || grouped["tenant_id"] != float64(7) ||
		grouped["job_id"] != float64(99) || grouped["job_kind"] != "link.risk_scan" ||
		grouped["trace_id"] != "4bf92f3577b34da6a3ce929d0e0e4736" || grouped["span_id"] != "00f067aa0ba902b7" ||
		grouped["service"] != "console" || grouped["component"] != "risk" {
		t.Errorf("top-level fields missing after WithGroup: %+v", grouped)
	}
	meta, _ := grouped["meta"].(map[string]any)
	if meta["ok"] != true {
		t.Errorf("meta = %+v, want ok=true", meta)
	}
	if _, leaked := meta["request_id"]; leaked {
		t.Errorf("request_id should stay at top level, not inside meta: %+v", meta)
	}
	if _, leaked := meta["trace_id"]; leaked {
		t.Errorf("trace_id should stay at top level, not inside meta: %+v", meta)
	}
}

func TestWrapContextIsIdempotent(t *testing.T) {
	var buf bytes.Buffer
	logger, _ := New(&buf, Options{Format: FormatJSON, Level: slog.LevelInfo})
	rewrapped := slog.New(WrapContext(logger.Handler()))

	ctx := WithRequestID(context.Background(), "req-once")
	ctx = WithTenantID(ctx, 12)
	rewrapped.InfoContext(ctx, "check")

	raw := buf.String()
	if count := strings.Count(raw, `"request_id"`); count != 1 {
		t.Errorf("request_id appeared %d times in %s", count, raw)
	}
	if count := strings.Count(raw, `"tenant_id"`); count != 1 {
		t.Errorf("tenant_id appeared %d times in %s", count, raw)
	}
}

func TestNewFormatsAndDynamicLevel(t *testing.T) {
	var buf bytes.Buffer
	logger, levelVar := New(&buf, Options{Format: FormatText, Level: slog.LevelInfo, Service: "redirector"})

	logger.Debug("hidden")
	if buf.Len() != 0 {
		t.Fatalf("debug log emitted at info level: %s", buf.String())
	}

	levelVar.Set(slog.LevelDebug)
	logger.Debug("visible", "code", "AbC123xy", "err", errors.New("boom"))
	out := buf.String()
	if !strings.Contains(out, "DBG") || !strings.Contains(out, "visible") ||
		!strings.Contains(out, "service=redirector") || !strings.Contains(out, "code=AbC123xy") ||
		!strings.Contains(out, "err=boom") {
		t.Errorf("tint text log = %q", out)
	}
	if strings.Contains(out, "\x1b[") {
		t.Errorf("non-tty buffer log should not contain ANSI escape codes: %q", out)
	}
}

func TestShouldDisableColor(t *testing.T) {
	var buf bytes.Buffer
	if !shouldDisableColor(&buf) {
		t.Error("bytes.Buffer should disable color")
	}

	f, err := os.CreateTemp(t.TempDir(), "log-*")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	defer func() { _ = f.Close() }()
	if !shouldDisableColor(f) {
		t.Error("regular file should disable color")
	}

	t.Setenv("NO_COLOR", "1")
	if !shouldDisableColor(os.Stdout) {
		t.Error("NO_COLOR=1 should disable color")
	}
}

func TestNewFallback(t *testing.T) {
	t.Run("honors valid json and debug env", func(t *testing.T) {
		t.Setenv("LOG_FORMAT", "json")
		t.Setenv("LOG_LEVEL", "debug")

		var buf bytes.Buffer
		logger := NewFallback(&buf, "console-cli")
		logger.Debug("probe")

		entry := decodeSingleJSON(t, buf.Bytes())
		if entry["level"] != "DEBUG" || entry["service"] != "console-cli" || entry["msg"] != "probe" {
			t.Errorf("entry = %+v", entry)
		}
	})

	t.Run("falls back gracefully on invalid env", func(t *testing.T) {
		t.Setenv("LOG_FORMAT", "yaml")
		t.Setenv("LOG_LEVEL", "verbose")

		var buf bytes.Buffer
		logger := NewFallback(&buf, "redirector")
		logger.Debug("filtered")
		if buf.Len() != 0 {
			t.Fatalf("unexpected debug output: %s", buf.String())
		}
		logger.Info("fatal")
		out := buf.String()
		if !strings.Contains(out, "INF") || !strings.Contains(out, "fatal") || !strings.Contains(out, "service=redirector") {
			t.Errorf("fallback log = %q", out)
		}
	})
}

func TestReplaceAttrRedaction(t *testing.T) {
	var buf bytes.Buffer
	logger, _ := New(&buf, Options{Format: FormatJSON, Level: slog.LevelInfo})

	logger.Info("startup",
		"database_url", "postgres://shortlink:pg-secret@db:5432/shortlink?sslmode=disable",
		"redis_url", "redis://:redis-secret@redis:6379/0",
		"authorization", "Bearer slk_topsecret",
		"client_secret", "oidc-secret",
		"key", "shortlink:link:AbC123xy",
	)

	raw := buf.String()
	for _, secret := range []string{"pg-secret", "redis-secret", "slk_topsecret", "oidc-secret"} {
		if strings.Contains(raw, secret) {
			t.Errorf("log leaked secret %q: %s", secret, raw)
		}
	}
	entry := decodeSingleJSON(t, buf.Bytes())
	if entry["authorization"] != Redacted || entry["client_secret"] != Redacted {
		t.Errorf("secret attrs not redacted: %+v", entry)
	}
	if entry["key"] != "shortlink:link:AbC123xy" {
		t.Errorf("cache key was unexpectedly redacted: %v", entry["key"])
	}
	dbURL, _ := entry["database_url"].(string)
	if !strings.Contains(dbURL, "sslmode=disable") {
		t.Errorf("database_url lost safe query param: %q", dbURL)
	}
}

func TestRedactURLAndSecret(t *testing.T) {
	if got := RedactSecret(""); got != "" {
		t.Errorf("RedactSecret empty = %q, want empty", got)
	}
	if got := RedactSecret("abc"); got != Redacted {
		t.Errorf("RedactSecret = %q, want %q", got, Redacted)
	}

	tests := []struct {
		name      string
		raw       string
		wantEmpty bool
		forbidden []string
		required  []string
	}{
		{name: "empty", raw: "", wantEmpty: true},
		{
			name:      "postgres dsn",
			raw:       "postgres://user:supersecret@db:5432/shortlink?sslmode=disable",
			forbidden: []string{"supersecret"},
			required:  []string{"postgres://", "db:5432/shortlink", "sslmode=disable"},
		},
		{
			name:      "redis password only",
			raw:       "rediss://:redispass@redis:6380/1",
			forbidden: []string{"redispass"},
			required:  []string{"rediss://", "redis:6380/1"},
		},
		{
			name:      "sensitive query param",
			raw:       "https://example.com/cb?token=secret-tok&ok=1",
			forbidden: []string{"secret-tok"},
			required:  []string{"ok=1", "token=REDACTED"},
		},
		{
			name:      "malformed url",
			raw:       "://bad:secret@",
			forbidden: []string{"secret"},
			required:  []string{Redacted},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := RedactURL(tt.raw)
			if tt.wantEmpty {
				if got != "" {
					t.Fatalf("RedactURL(%q) = %q, want empty", tt.raw, got)
				}
				return
			}
			for _, f := range tt.forbidden {
				if strings.Contains(got, f) {
					t.Errorf("RedactURL(%q) = %q leaked %q", tt.raw, got, f)
				}
			}
			for _, r := range tt.required {
				if !strings.Contains(got, r) {
					t.Errorf("RedactURL(%q) = %q missing %q", tt.raw, got, r)
				}
			}
		})
	}
}

func TestRequestEventLifecycleAndConcurrency(t *testing.T) {
	// 未挂载时各 setter 均为空操作，Snapshot 返回零值。
	SetActor(context.Background(), 1, "user", 2)
	SetCacheResult(context.Background(), "hit")
	SetError(context.Background(), "bad")
	SetRateLimited(context.Background(), 3)
	if snap := RequestEventFrom(context.Background()).Snapshot(); snap != (RequestEventSnapshot{}) {
		t.Fatalf("nil snapshot = %+v, want zero", snap)
	}

	ctx, ev := WithRequestEvent(context.Background())
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			SetActor(ctx, 7, "api_key", 10)
			SetCacheResult(ctx, "hit")
			SetError(ctx, "quota exceeded")
			SetRateLimited(ctx, 2)
			_ = ev.Snapshot()
		})
	}
	wg.Wait()

	snap := ev.Snapshot()
	if snap.TenantID != 7 || snap.ActorKind != "api_key" || snap.ActorID != 10 ||
		snap.Cache != "hit" || snap.ErrorMsg != "quota exceeded" ||
		!snap.RateLimited || snap.RetryAfter != 2 {
		t.Errorf("snapshot = %+v", snap)
	}
}

func decodeSingleJSON(t *testing.T, data []byte) map[string]any {
	t.Helper()
	var entry map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(data), &entry); err != nil {
		t.Fatalf("decode json log: %v\n%s", err, data)
	}
	return entry
}
