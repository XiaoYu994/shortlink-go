package httpapi

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"shortlink/internal/identity"
)

const (
	// apiV1Prefix 是管理 API 的版本前缀。
	apiV1Prefix = "/api/v1"
	// redirectPrefix 是公开短链的固定前缀。短链一旦发出去就是永久地址，不能改路径。
	redirectPrefix = "/r/"
	// metricsPath 是抓取路径。logging 和 HTTP 指标跳过它，避免抓取污染业务指标。
	metricsPath = "/metrics"
)

// isMetricsRequest 判断请求是否打到抓取端点；logging 和 HTTP 指标都跳过它。
func isMetricsRequest(r *http.Request) bool {
	return r.URL.Path == metricsPath
}

/*
RedirectRoutes 返回跳转进程的路由：公开跳转、存活检查、就绪检查和指标。
它不注册任何管理路由，API Key 鉴权只存在于 ConsoleRoutes。
*/
func (s *Server) RedirectRoutes() http.Handler {
	mux := http.NewServeMux()
	s.registerOperational(mux)
	mux.HandleFunc("GET "+redirectPrefix+"{code}", s.handleRedirect)
	return s.withMiddleware(mux)
}

/*
ConsoleRoutes 返回管理进程的路由：短链增删改查、统计、存活检查、就绪检查和指标。
鉴权和限流只包管理 API。公开跳转、存活检查、就绪检查和指标不认密钥，也不限流。
每个管理路由单独包：ServeMux 没有路由组，
套在 /api/v1/ 前缀上的中间件不会被更具体的路由继承。
*/
func (s *Server) ConsoleRoutes() http.Handler {
	mux := http.NewServeMux()
	s.registerOperational(mux)
	mux.Handle("POST "+apiV1Prefix+"/links", s.consoleRoute(identity.PermLinksWrite, s.handleCreate))
	mux.Handle("GET "+apiV1Prefix+"/links", s.consoleRoute(identity.PermLinksRead, s.handleList))
	mux.Handle("GET "+apiV1Prefix+"/links/{code}", s.consoleRoute(identity.PermLinksRead, s.handleGet))
	mux.Handle("PATCH "+apiV1Prefix+"/links/{code}", s.consoleRoute(identity.PermLinksWrite, s.handleUpdate))
	mux.Handle("DELETE "+apiV1Prefix+"/links/{code}", s.consoleRoute(identity.PermLinksWrite, s.handleDelete))
	mux.Handle("GET "+apiV1Prefix+"/links/{code}/stats", s.consoleRoute(identity.PermStatsRead, s.handleStats))
	return s.withMiddleware(mux)
}

/*
consoleRoute 给管理 handler 套上鉴权、按租户限流和权限校验。
顺序不能反：
1. authenticate 先确定租户和执行者身份；
2. rateLimit 放在 requirePerm 外面，避免低权限密钥疯狂刷写接口时绕过限流（也保护以后更重的 Authorizer 实现）；
3. requirePerm 最后按动作校验权限。
超限或无权限的请求都不会进入 handler，也不会查库或更新缓存。
*/
func (s *Server) consoleRoute(perm identity.Perm, h http.HandlerFunc) http.Handler {
	return s.authenticate(s.rateLimit(s.requirePerm(perm, h)))
}

// registerOperational 注册两个进程都有的运维端点。
func (s *Server) registerOperational(mux *http.ServeMux) {
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /readyz", s.handleReadyz)
	// /metrics 用注入的 Registry；promhttp.Handler() 抓的是 DefaultGatherer，注入的指标不会出现。
	mux.Handle("GET "+metricsPath, promhttp.HandlerFor(s.registry, promhttp.HandlerOpts{}))
}

/*
withMiddleware 给一组路由套上中间件。
requestID 在最外层，里面的 logging、recoverer 和 handler 拿到的 context 里已经有编号。
logging 在 recoverer 外面：panic 被写成 500 之后，访问日志里的状态码也是 500。
HTTP 指标也放在 recoverer 外面，才能把 recoverer 写的 500 记进去。
timeout 在最里层：它只约束 handler，日志和 panic 兜底要完整执行，不能被时限打断。
mux 必须是这组路由自己的，指标中间件靠它查路由模式。
*/
func (s *Server) withMiddleware(mux *http.ServeMux) http.Handler {
	return chain(mux,
		requestID(),
		logging(s.logger),
		s.metrics.httpMiddleware(mux),
		recoverer(s.logger),
		timeout(s.requestTimeout),
	)
}
