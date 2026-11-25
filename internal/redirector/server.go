// Package redirector 放公开短链跳转进程的 HTTP handler 和路由（ADR 0009、ADR 0019）。
package redirector

import (
	"context"
	"log/slog"
	"net/http"
	"net/netip"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"shortlink/internal/click"
	"shortlink/internal/httpx"
	"shortlink/internal/link"
)

// redirectPrefix 是公开短链的固定前缀。短链一旦发出去就是永久地址，不能改路径。
const redirectPrefix = "/r/"

/*
LinkStore 是跳转进程需要的存储能力。
接口定义在使用方，只包含按短码查询，不耦合管理接口与写方法。
*/
type LinkStore interface {
	GetByCode(ctx context.Context, code string) (*link.Link, error)
}

/*
ClickRecorder 接收一次成功的跳转。
Record 必须马上返回。nil 表示这个 Server 不记点击，部分测试走这条路径。
*/
type ClickRecorder interface {
	Record(click.Click)
}

// Options 是跳转进程的额外依赖。
type Options struct {
	/*
		Clicks 为 nil 时跳转不记点击。传入的必须是未装箱的 nil：
		带类型的 nil 指针放进接口后不等于 nil，调用 Record 会 panic。
	*/
	Clicks ClickRecorder
	// VisitorSalt 是按天访客哈希的 HMAC 密钥；为空时不记访客，点击本身照常记录。
	VisitorSalt []byte
	/*
		TrustProxyHeaders 为 true 时按 X-Forwarded-For 与 TrustedProxyCIDRs 取客户端 IP。
		最左一段在代理追加头时可以由客户端伪造。只能开在可信反向代理后面。
	*/
	TrustProxyHeaders bool
	// TrustedProxyCIDRs 是多跳可信代理网段（如 CDN + 网关）；为空时沿用单跳最右合法 IP。
	TrustedProxyCIDRs []netip.Prefix
	// AccessLogSampleRate 控制正常跳转 INFO 访问日志采样率；nil 默认全量 1.0。
	AccessLogSampleRate *float64
	// AccessLogSlowThreshold 为正时覆盖默认慢请求门槛。
	AccessLogSlowThreshold time.Duration
}

// Server 持有跳转进程需要的依赖，复用 httpx 提供的运维端点与通用中间件。
type Server struct {
	base        *httpx.Server
	logger      *slog.Logger
	store       LinkStore
	clicks      ClickRecorder
	visitorSalt []byte
}

// New 组装公开跳转进程，只挂载 /r/{code} 与通用运维端点。
func New(logger *slog.Logger, store LinkStore, requestTimeout time.Duration, registry *prometheus.Registry, opts Options) *Server {
	base := httpx.New(logger, requestTimeout, registry)
	sampleRate := 1.0
	if opts.AccessLogSampleRate != nil {
		sampleRate = *opts.AccessLogSampleRate
	}
	base.SetTrustedProxies(opts.TrustProxyHeaders, opts.TrustedProxyCIDRs)
	base.SetAccessLog(httpx.AccessLogOptions{
		SampleRate:     sampleRate,
		SlowThreshold:  opts.AccessLogSlowThreshold,
		Treat404AsInfo: true,
	})
	return &Server{
		base:        base,
		logger:      base.Logger(),
		store:       store,
		clicks:      opts.Clicks,
		visitorSalt: opts.VisitorSalt,
	}
}

// SetReadiness 接入就绪检查；不传参数表示启动完成就算就绪。
func (s *Server) SetReadiness(checks ...httpx.ReadyCheck) {
	s.base.SetReadiness(checks...)
}

// SetDraining 让 /readyz 开始返回 503，/healthz 保持 200。
func (s *Server) SetDraining() {
	s.base.SetDraining()
}

func (s *Server) registerRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET "+redirectPrefix+"{code}", s.handleRedirect)
}

/*
Routes 返回跳转进程的路由：公开跳转、存活检查、就绪检查和指标。
不注册任何管理、认证或成员路由。
*/
func (s *Server) Routes() http.Handler {
	return s.base.Handler(s.registerRoutes)
}

// RoutesBusiness 只返回公开跳转业务路由（供启用独立运维端口 OPS_ADDR 时使用）。
func (s *Server) RoutesBusiness() http.Handler {
	return s.base.BusinessHandler(s.registerRoutes)
}

// OpsHandler 只返回 /healthz、/readyz、/metrics 运维端点。
func (s *Server) OpsHandler() http.Handler {
	return s.base.OpsHandler()
}
