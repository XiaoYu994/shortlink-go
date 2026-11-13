package httpapi

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus/promhttp"
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
Routes 返回注册好全部路由的 handler。
requestID 在最外层，里面的 logging、recoverer 和 handler 拿到的 context 里已经有编号。
logging 在 recoverer 外面：panic 被写成 500 之后，访问日志里的状态码也是 500。
HTTP 指标也放在 recoverer 外面，才能把 recoverer 写的 500 记进去。
timeout 在最里层：它只约束 handler，日志和 panic 兜底要完整执行，不能被时限打断。
*/
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	// /metrics 用注入的 Registry；promhttp.Handler() 抓的是 DefaultGatherer，注入的指标不会出现。
	mux.Handle("GET "+metricsPath, promhttp.HandlerFor(s.registry, promhttp.HandlerOpts{}))
	// 鉴权只包管理 API。公开跳转、健康检查和指标不认密钥。
	mux.Handle("POST "+apiV1Prefix+"/links", s.authenticate(http.HandlerFunc(s.handleCreate)))
	mux.HandleFunc("GET "+redirectPrefix+"{code}", s.handleRedirect)
	return chain(mux,
		requestID(),
		logging(s.logger),
		s.metrics.httpMiddleware(mux),
		recoverer(s.logger),
		timeout(s.requestTimeout),
	)
}
