package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"
)

// requestIDHeader 是响应里带回的请求编号，调用方报障时可以拿出这个值来查日志。
const requestIDHeader = "X-Request-ID"

// maxRequestIDLen 限制调用方传入的编号长度，避免超长字符串把日志撑爆。
const maxRequestIDLen = 128

// requestIDContextKey 用空结构体做键，这样不会和别的包放进 context 的字符串键撞上。
type requestIDContextKey struct{}

/*
requestID 为每个请求确定编号，写进响应头和 context。
调用方传来的 X-Request-ID 只在字符安全时沿用，否则改由服务端生成。
编号里如果有换行，写进日志时会被看成新的一行。

这个中间件要放在 logging 外面。它通过 WithContext 交出一个新的 Request，
外层函数里原来的那个 Request 看不到这次放进去的编号。
*/
func requestID() middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := r.Header.Get(requestIDHeader)
			if !validRequestID(id) {
				id = newRequestID()
			}
			w.Header().Set(requestIDHeader, id)
			next.ServeHTTP(w, r.WithContext(withRequestID(r.Context(), id)))
		})
	}
}

func withRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDContextKey{}, id)
}

func requestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(requestIDContextKey{}).(string)
	return id
}

// validRequestID 只接受短的可见 ASCII，拒绝空格、换行和其他符号。
func validRequestID(id string) bool {
	if id == "" || len(id) > maxRequestIDLen {
		return false
	}
	for _, c := range id {
		switch {
		case c >= 'a' && c <= 'z':
		case c >= 'A' && c <= 'Z':
		case c >= '0' && c <= '9':
		case c == '-' || c == '_' || c == '.':
		default:
			return false
		}
	}
	return true
}

// fallbackRequestSeq 只在 crypto/rand 失败时使用，避免同一纳秒内的两次失败拿到同一个编号。
var fallbackRequestSeq atomic.Uint64

// newRequestID 生成 32 位十六进制编号。crypto/rand 失败时退回时间戳加序号，避免这次请求没有编号。
func newRequestID() string {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		n := fallbackRequestSeq.Add(1)
		return fmt.Sprintf("fallback-%d-%d", time.Now().UnixNano(), n)
	}
	return hex.EncodeToString(buf[:])
}

/*
requestIDHandler 从 context 取出编号，加到每一条带 context 的日志上。
不带 context 的日志（例如启动日志）不会被加上 request_id。
WithAttrs 和 WithGroup 必须继续包回这一层，否则 logger.With 会把这层丢掉。
*/
type requestIDHandler struct {
	slog.Handler
}

func (h requestIDHandler) Handle(ctx context.Context, rec slog.Record) error {
	if id := requestIDFrom(ctx); id != "" {
		rec.AddAttrs(slog.String("request_id", id))
	}
	return h.Handler.Handle(ctx, rec)
}

func (h requestIDHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return requestIDHandler{Handler: h.Handler.WithAttrs(attrs)}
}

func (h requestIDHandler) WithGroup(name string) slog.Handler {
	return requestIDHandler{Handler: h.Handler.WithGroup(name)}
}
