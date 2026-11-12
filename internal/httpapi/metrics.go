package httpapi

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
httpMetrics 持有 HTTP 指标。
NewServer 里创建并注册一次；Routes() 每次只组装中间件，不能在这里注册，
否则测试在同一个 Server 上重复调用 Routes() 时会 panic。
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

/*
httpMiddleware 记录请求数和耗时。
label 只放取值有限的维度：route 用 ServeMux 的模式，未匹配统一记成 unmatched；
短码和 request_id 不做 label。方法收成有限集合，见 methodLabel。
中间件放在 recoverer 外层，panic 被 recoverer 写成 500 后才能记到这个状态码。
*/
func (m *httpMetrics) httpMiddleware(mux *http.ServeMux) middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// 抓取请求不算业务请求，别让 /metrics 自己变成一条时间序列。
			if isMetricsRequest(r) {
				next.ServeHTTP(w, r)
				return
			}

			start := time.Now()
			sw := &statusWriter{ResponseWriter: w}
			defer func() {
				status := responseStatus(sw, r)
				route := routePattern(mux, r)
				method := methodLabel(r.Method)
				statusLabel := strconv.Itoa(status)
				m.requests.WithLabelValues(route, method, statusLabel).Inc()
				m.duration.WithLabelValues(route, method, statusLabel).Observe(time.Since(start).Seconds())
			}()
			next.ServeHTTP(sw, r)
		})
	}
}

/*
methodLabel 把方法收成有限集合。
已注册的只有 GET 和 POST；net/http 里 GET 的模式同时匹配 HEAD，所以 HEAD 单独留下。
其余方法只会出现在未匹配的请求上，原样做 label 会随客户端输入无限增长。
*/
func methodLabel(method string) string {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPost:
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
