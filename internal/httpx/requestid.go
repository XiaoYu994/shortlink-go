package httpx

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"sync/atomic"
	"time"

	"shortlink/internal/logx"
)

// requestIDHeader 是响应里带回的请求编号，调用方报障时可以拿出这个值来查日志。
const requestIDHeader = "X-Request-ID"

// maxRequestIDLen 限制调用方传入的编号长度，避免超长字符串把日志撑爆。
const maxRequestIDLen = 128

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
	return logx.WithRequestID(ctx, id)
}

// RequestIDFrom 从请求 context 取出当前请求的编号；不存在时返回空字符串。
func RequestIDFrom(ctx context.Context) string {
	return logx.RequestIDFrom(ctx)
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
