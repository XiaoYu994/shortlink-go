package pgdb

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"unicode"

	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

const (
	pgTracerName     = "shortlink/pgdb"
	maxSQLTextLength = 512
)

// pgSpanKey 使用包内私有空结构体仅存放本层创建的 span，避免 TraceQueryEnd 误关外层父 span。
type pgSpanKey struct{}

type queryTracer struct{}

func (queryTracer) TraceQueryStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	// 未采样或全局 TracerProvider 为 Noop 时零分配直接返回，避免后台空轮询等路径产生额外开销。
	if !trace.SpanFromContext(ctx).IsRecording() {
		return ctx
	}

	op := sqlOperation(data.SQL)
	attrs := make([]attribute.KeyValue, 0, 5)
	attrs = append(attrs,
		attribute.String("db.system.name", "postgresql"),
		attribute.String("db.operation.name", op),
		attribute.String("db.query.text", summarizeSQL(data.SQL, maxSQLTextLength)),
	)
	if conn != nil && conn.Config() != nil {
		if host := conn.Config().Host; host != "" {
			attrs = append(attrs, attribute.String("server.address", host))
		}
		if port := conn.Config().Port; port > 0 {
			attrs = append(attrs, attribute.Int("server.port", int(port)))
		}
	}

	ctx, span := otel.Tracer(pgTracerName).Start(ctx, "pg."+strings.ToLower(op),
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(attrs...),
	)
	return context.WithValue(ctx, pgSpanKey{}, span)
}

func (queryTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	span, ok := ctx.Value(pgSpanKey{}).(trace.Span)
	if !ok || span == nil {
		return
	}
	if data.Err != nil &&
		!errors.Is(data.Err, pgx.ErrNoRows) &&
		!errors.Is(data.Err, sql.ErrNoRows) &&
		!errors.Is(data.Err, context.Canceled) {
		span.RecordError(data.Err)
		span.SetStatus(codes.Error, data.Err.Error())
	}
	span.End()
}

func sqlOperation(rawSQL string) string {
	i := skipSQLTrivia(rawSQL)
	if i >= len(rawSQL) {
		return "QUERY"
	}
	start := i
	for i < len(rawSQL) && ((rawSQL[i] >= 'a' && rawSQL[i] <= 'z') || (rawSQL[i] >= 'A' && rawSQL[i] <= 'Z')) {
		i++
	}
	if i == start {
		return "QUERY"
	}
	switch strings.ToUpper(rawSQL[start:i]) {
	case "SELECT", "INSERT", "UPDATE", "DELETE", "WITH", "SET", "BEGIN", "COMMIT", "ROLLBACK", "CREATE", "DROP":
		return strings.ToUpper(rawSQL[start:i])
	default:
		return "QUERY"
	}
}

func skipSQLTrivia(s string) int {
	i := 0
	for i < len(s) {
		for i < len(s) && unicode.IsSpace(rune(s[i])) {
			i++
		}
		if i+1 < len(s) && s[i] == '-' && s[i+1] == '-' {
			i += 2
			for i < len(s) && s[i] != '\n' {
				i++
			}
			continue
		}
		if i+1 < len(s) && s[i] == '/' && s[i+1] == '*' {
			i += 2
			closed := false
			for i+1 < len(s) {
				if s[i] == '*' && s[i+1] == '/' {
					i += 2
					closed = true
					break
				}
				i++
			}
			if !closed {
				return len(s)
			}
			continue
		}
		break
	}
	return i
}

// summarizeSQL 折叠连续空白并在 UTF-8 字符边界截断参数化 SQL，仅记录语句摘要而不记录任何参数值。
func summarizeSQL(rawSQL string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	var b strings.Builder
	b.Grow(min(len(rawSQL), maxBytes))
	needSpace := false
	truncated := false
	for _, r := range rawSQL {
		if unicode.IsSpace(r) {
			if b.Len() > 0 {
				needSpace = true
			}
			continue
		}
		needLen := len(string(r))
		if needSpace {
			needLen++
		}
		if b.Len()+needLen > maxBytes {
			truncated = true
			break
		}
		if needSpace {
			b.WriteByte(' ')
			needSpace = false
		}
		b.WriteRune(r)
	}
	out := b.String()
	if truncated && maxBytes > 3 {
		// 留出 "..." 空间并按 rune 边界回退，避免截断多字节 UTF-8 字符。
		limit := maxBytes - 3
		for len(out) > limit {
			r, size := lastRune(out)
			_ = r
			out = out[:len(out)-size]
		}
		return out + "..."
	}
	return out
}

func lastRune(s string) (rune, int) {
	if len(s) == 0 {
		return 0, 0
	}
	i := len(s) - 1
	for i > 0 && (s[i]&0xc0) == 0x80 {
		i--
	}
	for _, r := range s[i:] {
		return r, len(s) - i
	}
	return 0, 0
}
