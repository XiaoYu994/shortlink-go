package httpx

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

const (
	httpRequestsMetricName = "shortlink_http_requests_total"
	httpDurationMetricName = "shortlink_http_request_duration_seconds"
	// routeUnmatched 是路由未匹配时统一的 route label 值，避免把 404 的路径写进 label。
	routeUnmatched = "unmatched"
	// methodOther 收拢未注册的 HTTP 方法，避免客户端随意构造方法把 label 撑开。
	methodOther = "other"
)

/*
默认桶从 5ms 开始，跳转通常在 1ms 内，会全落在第一个桶里，p99 看不出缓存效果。
从 0.1ms 开始按 2 倍增长，覆盖到约 13s。
*/
var durationBuckets = prometheus.ExponentialBuckets(0.0001, 2, 18)

/*
httpMetrics 持有两个进程共用的 HTTP 指标。
New 里创建并注册一次；Handler 每次只组装中间件，
不能在注册路由时重复注册，否则测试在同一个 Server 上多次调用 Handler 会 panic。
*/
type httpMetrics struct {
	requests *prometheus.CounterVec
	duration *prometheus.HistogramVec
}

func newHTTPMetrics(registry *prometheus.Registry) *httpMetrics {
	m := &httpMetrics{
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: httpRequestsMetricName,
			Help: "Total number of HTTP requests by route, method, and status.",
		}, []string{"route", "method", "status"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    httpDurationMetricName,
			Help:    "HTTP request duration in seconds by route, method, and status.",
			Buckets: durationBuckets,
		}, []string{"route", "method", "status"}),
	}
	registry.MustRegister(m.requests, m.duration)
	return m
}

func (m *httpMetrics) observe(route, method string, status int, dur time.Duration) {
	method = methodLabel(method)
	statusLabel := strconv.Itoa(status)
	m.requests.WithLabelValues(route, method, statusLabel).Inc()
	m.duration.WithLabelValues(route, method, statusLabel).Observe(dur.Seconds())
}

/*
methodLabel 把方法收成有限集合。
已注册的有 GET、HEAD、POST、PATCH、DELETE；net/http 里 GET 的模式同时匹配 HEAD，
所以 HEAD 单独留下。其余方法只会出现在未匹配的请求上，
原样做 label 会随客户端输入无限增长。
*/
func methodLabel(method string) string {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPatch, http.MethodDelete:
		return method
	default:
		return methodOther
	}
}

/*
routePattern 返回请求匹配到的路由模式；未匹配（包括 405）返回固定值。
不用 r.Pattern：timeout 在指标中间件里层用 WithContext 换过 Request，
ServeMux 只把 Pattern 写到那份副本上，外层读到的是空字符串。
mux.Handler 只查路由，不执行 handler，也不会修改请求。
*/
func routePattern(mux *http.ServeMux, r *http.Request) string {
	_, pattern := mux.Handler(r)
	if pattern == "" {
		return routeUnmatched
	}
	return pattern
}
