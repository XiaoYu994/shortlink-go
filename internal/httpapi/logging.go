package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"
)

// statusClientClosedRequest 是 nginx 约定的“客户端提前断开”；只用于日志和指标，不发给客户端。
const statusClientClosedRequest = 499

/*
logging 在请求结束后记录方法、路径、状态码和耗时。
只记路径，不记查询串，避免把敏感参数写进日志。

用 defer 写日志，next 返回后这行一定会执行。
logging 放在 recoverer 外层：普通 panic 被 recoverer 写成 500 后再回来记日志；
recoverer 继续抛出 http.ErrAbortHandler 时，defer 也照样执行。
InfoContext 带上请求的 context，requestIDHandler 才能把 request_id 加进这条访问日志。
*/
func logging(logger *slog.Logger) middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Prometheus 每 5 秒抓一次，记进访问日志会把业务日志淹没。
			if isMetricsRequest(r) {
				next.ServeHTTP(w, r)
				return
			}

			start := time.Now()
			sw := &statusWriter{ResponseWriter: w}
			defer func() {
				status := responseStatus(sw, r)
				logger.InfoContext(r.Context(), "request",
					"method", r.Method,
					"path", r.URL.Path,
					"status", status,
					"duration", time.Since(start),
				)
			}()
			next.ServeHTTP(sw, r)
		})
	}
}

/*
statusWriter 记住第一个写出的状态码。
后面再调用 WriteHeader 不再覆盖：net/http 也只认第一次，日志要和客户端实际看到的一致。
*/
type statusWriter struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (w *statusWriter) WriteHeader(code int) {
	if w.wrote {
		return
	}
	w.status = code
	w.wrote = true
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	// 只写 body、不写状态码时，net/http 按 200 处理。这里先记下来，否则日志里的状态会留在 0。
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(b)
}

// Unwrap 把内层 ResponseWriter 暴露给 http.ResponseController，否则外层中间件会挡住 Flush、SetWriteDeadline 等能力。
func (w *statusWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

// responseStatus 补全 statusWriter 记录的状态码：没写响应按 200，客户端断开按 499。
func responseStatus(w *statusWriter, r *http.Request) int {
	if w.status != 0 {
		return w.status
	}
	if errors.Is(r.Context().Err(), context.Canceled) {
		return statusClientClosedRequest
	}
	return http.StatusOK
}
