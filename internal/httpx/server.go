/*
Package httpx 提供零业务依赖的通用 HTTP 底座（ADR 0019 第 3 步）。

包含中间件链、请求编号（及 slog 注入）、访问日志、HTTP 指标、panic 兜底、请求超时、
/healthz /readyz /metrics /debug/pprof/ 运维端点、客户端 IP 提取与通用 JSON/错误响应。
本包不 import 任何业务领域包，由 redirector 和 console 的 HTTP 包复用。
*/
package httpx

import (
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"shortlink/internal/logx"
)

const (
	healthzPath = "/healthz"
	readyzPath  = "/readyz"
	// metricsPath 是抓取路径。logging 和 HTTP 指标跳过它，避免抓取污染业务指标。
	metricsPath = "/metrics"
)

// isMetricsRequest 判断请求是否打到抓取端点；logging 和 HTTP 指标都跳过它。
func isMetricsRequest(r *http.Request) bool {
	return r.URL.Path == metricsPath
}

// isProbeRequest 判断请求是否打到存活或就绪探针。
func isProbeRequest(r *http.Request) bool {
	return r.URL.Path == healthzPath || r.URL.Path == readyzPath
}

/*
Server 提供两个进程共用的 HTTP 底座：已包装 requestIDHandler 的 logger、
/healthz /readyz /metrics、摘流状态与标准中间件链。
由 redirector 和 console 的 Server 组合使用。
*/
type Server struct {
	logger         *slog.Logger
	accessLog      accessLogger
	requestTimeout time.Duration
	registry       *prometheus.Registry
	metrics        *httpMetrics
	/*
		ready 是 /readyz 要问的检查项，readyWired 记录 SetReadiness 有没有调用过。
		两者分开：没接线要返回 503，接了线但一项都不查是合法的（redirector 就是这样）。
	*/
	ready      []ReadyCheck
	readyWired bool
	/*
		draining 为 true 时 /readyz 直接返回 503。关停时先置上，等探针把副本摘掉，再关闭监听。
		/healthz 不看它：进程还活着，不该被当成需要重启。
	*/
	draining atomic.Bool
}

/*
New 创建 HTTP 底座：给 logger 幂等套上 logx.WrapContext，并在 registry 上注册一次 HTTP 指标。
requestTimeout 是每个请求的处理时限，由 timeout 中间件执行，必须为正。
registry 由 main 创建并注入，测试每次传一个新的 Registry，避免同名指标重复注册。
*/
func New(logger *slog.Logger, requestTimeout time.Duration, registry *prometheus.Registry) *Server {
	var handler slog.Handler
	if logger != nil {
		handler = logger.Handler()
	}
	wrapped := slog.New(logx.WrapContext(handler))
	return &Server{
		logger: wrapped,
		accessLog: newAccessLogger(wrapped, AccessLogOptions{
			SampleRate:    1,
			SlowThreshold: DefaultSlowRequestThreshold,
		}),
		requestTimeout: requestTimeout,
		registry:       registry,
		metrics:        newHTTPMetrics(registry),
	}
}

// SetAccessLog 配置 HTTP 访问日志的分级、采样与慢请求门槛。
func (s *Server) SetAccessLog(opts AccessLogOptions) {
	s.accessLog = newAccessLogger(s.logger, opts)
}

// Logger 返回已经包好上下文提取器的日志器；带请求 context 的日志会自动写上关联字段。
func (s *Server) Logger() *slog.Logger {
	return s.logger
}

/*
Handler 创建独立的 ServeMux，注册 /healthz、/readyz、/metrics，调用 register 挂载业务路由，
再按固定顺序包上全套通用中间件。每次都用新 mux 并将其传给指标中间件查路由模式。
*/
func (s *Server) Handler(register func(mux *http.ServeMux)) http.Handler {
	mux := http.NewServeMux()
	s.registerOperational(mux)
	if register != nil {
		register(mux)
	}
	return s.withMiddleware(mux)
}

/*
BusinessHandler 创建仅含业务路由的 ServeMux 并包上标准中间件（不包含 /healthz、/readyz、/metrics）。
当业务进程配置独立运维端口（OPS_ADDR / CONSOLE_OPS_ADDR）时使用（ADR 0024）。
*/
func (s *Server) BusinessHandler(register func(mux *http.ServeMux)) http.Handler {
	mux := http.NewServeMux()
	if register != nil {
		register(mux)
	}
	return s.withMiddleware(mux)
}

/*
OpsHandler 创建仅含 /healthz、/readyz、/metrics 运维端点的 ServeMux 并包上标准中间件（ADR 0024）。
*/
func (s *Server) OpsHandler() http.Handler {
	mux := http.NewServeMux()
	s.registerOperational(mux)
	return s.withMiddleware(mux)
}

// registerOperational 注册两个进程都有的运维端点。
func (s *Server) registerOperational(mux *http.ServeMux) {
	mux.HandleFunc("GET "+healthzPath, s.handleHealthz)
	mux.HandleFunc("GET "+readyzPath, s.handleReadyz)
	// /metrics 用注入的 Registry；promhttp.Handler() 抓的是 DefaultGatherer，注入的指标不会出现。
	mux.Handle("GET "+metricsPath, promhttp.HandlerFor(s.registry, promhttp.HandlerOpts{}))
}

/*
withMiddleware 给一组路由套上中间件。
requestID 在最外层，里面的 observe、recoverer 和 handler 拿到的 context 里已经有编号。
observe 合并了访问日志和 HTTP 指标（共用同一次 statusWriter 与耗时统计），
放在 recoverer 外面：panic 被写成 500 之后，访问日志和指标里的状态码也是 500。
timeout 在最里层：它只约束 handler，日志和 panic 兜底要完整执行，不能被时限打断。
mux 必须是这组路由自己的，observe 靠它查路由模式。
*/
func (s *Server) withMiddleware(mux *http.ServeMux) http.Handler {
	return chain(mux,
		requestID(),
		s.observe(mux),
		recoverer(s.logger),
		timeout(s.requestTimeout),
	)
}

func (s *Server) observe(mux *http.ServeMux) middleware {
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
				dur := time.Since(start)
				status := responseStatus(sw, r)
				route := routePattern(mux, r)
				s.metrics.observe(route, r.Method, status, dur)
				s.accessLog.log(r.Context(), r, route, status, sw.bytes, dur, ev.Snapshot())
			}()
			next.ServeHTTP(sw, r)
		})
	}
}
