package httpx

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"time"

	"shortlink/internal/logx"
)

const (
	// statusClientClosedRequest 是 nginx 约定的“客户端提前断开”；只用于日志和指标，不发给客户端。
	statusClientClosedRequest = 499
	// DefaultSlowRequestThreshold 是访问日志标记 slow=true 并升为 Warn 的默认耗时门槛。
	DefaultSlowRequestThreshold = 200 * time.Millisecond
)

// AccessLogOptions 控制 HTTP 访问日志（Canonical Log Line）的分级、采样与慢请求门槛。
type AccessLogOptions struct {
	TrustProxyHeaders bool
	// SampleRate 取值 [0, 1]：仅作用于非慢、正常 INFO 流量；5xx、4xx Warn 与慢请求全量保留。
	SampleRate    float64
	SlowThreshold time.Duration
	// Treat404AsInfo 为 true 时（redirector 数据面），未超时的 404 保持 Info，避免公网扫库刷告警。
	Treat404AsInfo bool
}

func normalizeAccessLogOptions(opts AccessLogOptions) AccessLogOptions {
	if opts.SampleRate < 0 {
		opts.SampleRate = 0
	}
	if opts.SampleRate > 1 {
		opts.SampleRate = 1
	}
	if opts.SlowThreshold <= 0 {
		opts.SlowThreshold = DefaultSlowRequestThreshold
	}
	return opts
}

type accessLogger struct {
	logger *slog.Logger
	opts   AccessLogOptions
}

func newAccessLogger(logger *slog.Logger, opts AccessLogOptions) accessLogger {
	return accessLogger{
		logger: logger,
		opts:   normalizeAccessLogOptions(opts),
	}
}

/*
logging 是供独立中间件测试使用的访问日志包装；Server.withMiddleware 使用统一的 observe。
请求入口挂载 RequestEvent 收集槽，结束时输出包含业务维度的单条 Canonical Log Line。
*/
func logging(logger *slog.Logger) middleware {
	al := newAccessLogger(logger, AccessLogOptions{
		SampleRate:    1,
		SlowThreshold: DefaultSlowRequestThreshold,
	})
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if isMetricsRequest(r) {
				next.ServeHTTP(w, r)
				return
			}

			ctx, ev := logx.WithRequestEvent(r.Context())
			r = r.WithContext(ctx)
			start := time.Now()
			sw := &statusWriter{ResponseWriter: w}
			defer func() {
				al.log(r.Context(), r, "", responseStatus(sw, r), sw.bytes, time.Since(start), ev.Snapshot())
			}()
			next.ServeHTTP(sw, r)
		})
	}
}

func (a accessLogger) log(
	ctx context.Context,
	r *http.Request,
	route string,
	status int,
	bytesWritten int,
	dur time.Duration,
	snap logx.RequestEventSnapshot,
) {
	probe := isProbeRequest(r)
	slow := !probe && dur >= a.opts.SlowThreshold
	level := a.classifyLevel(probe, status, slow)

	if !probe && level == slog.LevelInfo && !slow && status < http.StatusInternalServerError {
		if a.opts.SampleRate <= 0 {
			return
		}
		if a.opts.SampleRate < 1 && rand.Float64() >= a.opts.SampleRate {
			return
		}
	}

	if !a.logger.Enabled(ctx, level) {
		return
	}

	attrs := make([]slog.Attr, 0, 14)
	attrs = append(attrs, slog.String("method", r.Method))
	if route != "" {
		attrs = append(attrs, slog.String("route", route))
	}
	attrs = append(attrs,
		slog.String("path", r.URL.Path),
		slog.Int("status", status),
		slog.Float64("duration_ms", durationMillis(dur)),
		slog.Int("bytes", bytesWritten),
	)
	if ip := ClientIP(r, a.opts.TrustProxyHeaders); ip != "" {
		attrs = append(attrs, slog.String("ip", ip))
	}
	if snap.TenantID > 0 && logx.TenantIDFrom(ctx) == 0 {
		attrs = append(attrs, slog.Int64("tenant_id", snap.TenantID))
	}
	if snap.ActorKind != "" {
		if snap.ActorID > 0 {
			attrs = append(attrs, slog.String("actor", fmt.Sprintf("%s:%d", snap.ActorKind, snap.ActorID)))
		} else {
			attrs = append(attrs, slog.String("actor", snap.ActorKind))
		}
	}
	if snap.Cache != "" {
		attrs = append(attrs, slog.String("cache", snap.Cache))
	}
	if snap.RateLimited {
		attrs = append(attrs, slog.Int("retry_after", snap.RetryAfter))
	}
	if snap.ErrorMsg != "" {
		attrs = append(attrs, slog.String("err_msg", snap.ErrorMsg))
	}
	if slow {
		attrs = append(attrs, slog.Bool("slow", true))
	}

	a.logger.LogAttrs(ctx, level, "request", attrs...)
}

func (a accessLogger) classifyLevel(probe bool, status int, slow bool) slog.Level {
	if probe {
		if status >= http.StatusOK && status < http.StatusMultipleChoices {
			return slog.LevelDebug
		}
		// 非 2xx 探针保留 Info：真实依赖故障已由 handleReadyz 记 Warn，draining/canceled 探针不误报 Warn。
		return slog.LevelInfo
	}
	switch {
	case status >= http.StatusInternalServerError:
		return slog.LevelError
	case status == statusClientClosedRequest:
		return slog.LevelInfo
	case status == http.StatusNotFound && a.opts.Treat404AsInfo:
		if slow {
			return slog.LevelWarn
		}
		return slog.LevelInfo
	case status >= http.StatusBadRequest:
		return slog.LevelWarn
	case slow:
		return slog.LevelWarn
	default:
		return slog.LevelInfo
	}
}

func durationMillis(d time.Duration) float64 {
	return float64(d.Microseconds()) / 1000.0
}

/*
statusWriter 记住第一个写出的状态码并累计响应字节数。
后面再调用 WriteHeader 不再覆盖：net/http 也只认第一次，日志和指标要和客户端实际看到的一致。
*/
type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int
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
	n, err := w.ResponseWriter.Write(b)
	w.bytes += n
	return n, err
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
