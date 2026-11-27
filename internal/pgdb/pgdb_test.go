package pgdb

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

func setupPGDBTestTracer(t *testing.T) *tracetest.InMemoryExporter {
	t.Helper()
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	otel.SetTracerProvider(tp)
	t.Cleanup(func() {
		_ = tp.Shutdown(t.Context())
		otel.SetTracerProvider(noop.NewTracerProvider())
	})
	return exp
}

func TestSQLOperationAndSummarize(t *testing.T) {
	tests := []struct {
		name string
		sql  string
		want string
	}{
		{name: "select", sql: "SELECT id FROM links WHERE code = $1", want: "SELECT"},
		{name: "leading comment and cte", sql: "  -- comment\n  /* block */ with candidate AS (SELECT 1) UPDATE jobs SET status = 'done'", want: "WITH"},
		{name: "lowercase insert", sql: "\n\tinsert into links values ($1)", want: "INSERT"},
		{name: "empty", sql: "   ", want: "QUERY"},
		{name: "unclosed comment", sql: "/* unclosed", want: "QUERY"},
		{name: "unknown verb", sql: "EXPLAIN SELECT 1", want: "QUERY"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sqlOperation(tt.sql); got != tt.want {
				t.Errorf("sqlOperation(%q) = %q, want %q", tt.sql, got, tt.want)
			}
		})
	}

	gotSummary := summarizeSQL("  SELECT  id,\n\turl   FROM  links  WHERE  code = $1  ", 512)
	if gotSummary != "SELECT id, url FROM links WHERE code = $1" {
		t.Errorf("summarizeSQL = %q", gotSummary)
	}

	// 验证在包含中文注释时按 UTF-8 rune 边界安全截断。
	truncated := summarizeSQL("SELECT '你好世界你好世界' FROM links", 16)
	if len(truncated) > 16 || !strings.HasSuffix(truncated, "...") {
		t.Errorf("truncated = %q (len=%d)", truncated, len(truncated))
	}
}

func TestQueryTracer(t *testing.T) {
	exp := setupPGDBTestTracer(t)
	tracer := queryTracer{}

	// 无激活父 span 时零开销跳过，不产生孤立 span，且 TraceQueryEnd 不 panic。
	plainCtx := tracer.TraceQueryStart(context.Background(), nil, pgx.TraceQueryStartData{
		SQL:  "SELECT 1",
		Args: []any{"secret-arg-value"},
	})
	tracer.TraceQueryEnd(plainCtx, nil, pgx.TraceQueryEndData{})
	if spans := exp.GetSpans(); len(spans) != 0 {
		t.Fatalf("expected 0 spans without active parent, got %d", len(spans))
	}

	// 孤立调用 TraceQueryEnd 绝不能误关父 span。
	parentCtx, parentSpan := otel.Tracer("test").Start(context.Background(), "parent")
	tracer.TraceQueryEnd(parentCtx, nil, pgx.TraceQueryEndData{Err: errors.New("ignored")})
	if !parentSpan.IsRecording() {
		t.Fatal("TraceQueryEnd unexpectedly ended parent span")
	}

	// 正常 SELECT + Args：绝不记录参数值。
	qCtx := tracer.TraceQueryStart(parentCtx, nil, pgx.TraceQueryStartData{
		SQL:  "SELECT id, url\nFROM links WHERE code = $1",
		Args: []any{"secret-short-code"},
	})
	tracer.TraceQueryEnd(qCtx, nil, pgx.TraceQueryEndData{})

	// ErrNoRows 与 context.Canceled 不标为 Error。
	noRowsCtx := tracer.TraceQueryStart(parentCtx, nil, pgx.TraceQueryStartData{SQL: "SELECT id FROM links WHERE code = $1"})
	tracer.TraceQueryEnd(noRowsCtx, nil, pgx.TraceQueryEndData{Err: sql.ErrNoRows})

	canceledCtx := tracer.TraceQueryStart(parentCtx, nil, pgx.TraceQueryStartData{SQL: "UPDATE links SET url = $1"})
	tracer.TraceQueryEnd(canceledCtx, nil, pgx.TraceQueryEndData{Err: context.Canceled})

	// 真实 DB 错误应标为 Error。
	errCtx := tracer.TraceQueryStart(parentCtx, nil, pgx.TraceQueryStartData{SQL: "INSERT INTO links (code) VALUES ($1)"})
	tracer.TraceQueryEnd(errCtx, nil, pgx.TraceQueryEndData{Err: errors.New("connection refused")})

	parentSpan.End()

	spans := exp.GetSpans()
	if len(spans) != 5 {
		t.Fatalf("spans = %d, want 5", len(spans))
	}
	selectSpan := spans[0]
	if selectSpan.Name != "pg.select" || selectSpan.SpanKind != trace.SpanKindClient {
		t.Errorf("select span = (%q, %v)", selectSpan.Name, selectSpan.SpanKind)
	}
	for _, attr := range selectSpan.Attributes {
		if strings.Contains(attr.Value.AsString(), "secret-short-code") {
			t.Errorf("query span leaked parameter value in %s=%q", attr.Key, attr.Value.AsString())
		}
	}
	if spans[1].Status.Code != codes.Unset {
		t.Errorf("sql.ErrNoRows status = %v, want Unset", spans[1].Status.Code)
	}
	if spans[2].Status.Code != codes.Unset {
		t.Errorf("context.Canceled status = %v, want Unset", spans[2].Status.Code)
	}
	if spans[3].Status.Code != codes.Error || len(spans[3].Events) == 0 {
		t.Errorf("real error span status=%v events=%d, want Error with exception event", spans[3].Status.Code, len(spans[3].Events))
	}
}
