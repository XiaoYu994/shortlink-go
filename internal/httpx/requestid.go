package httpx

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"

	"shortlink/internal/logx"
)

// requestIDHeader 是响应里带回的请求编号，调用方报障时可以拿出这个值来查日志。
const requestIDHeader = "X-Request-ID"

// maxRequestIDLen 限制调用方传入的编号长度，避免超长字符串把日志撑爆。
const maxRequestIDLen = 128

/*
requestID 为每个请求生成服务端权威编号，写进响应头和 context（ADR 0025）。
客户端传来的 X-Request-ID 即使字符合法也不再覆写服务端 request_id 或审计表 request_id，
而是在通过字符校验后额外挂进 client_request_id 日志字段，保留跨系统排障线索。

这个中间件要放在 observe 外面。它通过 WithContext 交出一个新的 Request，
外层函数里原来的那个 Request 看不到这次放进去的编号。
*/
func requestID() middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := newRequestID()
			ctx := withRequestID(r.Context(), id)
			if clientID := r.Header.Get(requestIDHeader); validRequestID(clientID) {
				ctx = logx.WithClientRequestID(ctx, clientID)
			}
			w.Header().Set(requestIDHeader, id)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func withRequestID(ctx context.Context, id string) context.Context {
	return logx.WithRequestID(ctx, id)
}

// RequestIDFrom 从请求 context 取出服务端签发的请求编号；不存在时返回空字符串。
func RequestIDFrom(ctx context.Context) string {
	return logx.RequestIDFrom(ctx)
}

// ClientRequestIDFrom 从请求 context 取出调用方传入的合法 X-Request-ID；未传入或非法时返回空字符串。
func ClientRequestIDFrom(ctx context.Context) string {
	return logx.ClientRequestIDFrom(ctx)
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

// newRequestID 生成 32 位十六进制编号；Go 1.24+ 保证 crypto/rand.Read 永不返回错误。
func newRequestID() string {
	var buf [16]byte
	_, _ = rand.Read(buf[:])
	return hex.EncodeToString(buf[:])
}
