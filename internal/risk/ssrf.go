/*
Package risk 实现短链目标 URL 的异步风控检测（阶段 12、ADR 0020）。

包含四部分：
- 委托 internal/safehttp 提供的防 SSRF 安全 HTTP 客户端与端口/地址拦截（ADR 0027）；
- 按目标 host 的进程内限速器，防止 worker 放大冲击第三方站点；
- 面向 Redis 的 fail-open 扫描结果缓存；
- 规则检测器与对接 internal/job 的 link.risk_scan 处理器。
*/
package risk

import (
	"net/http"
	"time"

	"shortlink/internal/safehttp"
)

const (
	DefaultFetchTimeout  = 5 * time.Second
	DefaultMaxRedirects  = 5
	DefaultMaxBodyBytes  = 256 * 1024
	DefaultHostRateLimit = 2
)

// NewSafeHTTPClient 构造在拨号层拦截 SSRF、在重定向层允许有限安全跳转的风控 HTTP 客户端。
func NewSafeHTTPClient(opts safehttp.ClientOptions) *http.Client {
	opts.DisableRedirects = false
	if opts.Timeout <= 0 {
		opts.Timeout = DefaultFetchTimeout
	}
	if opts.MaxRedirects <= 0 {
		opts.MaxRedirects = DefaultMaxRedirects
	}
	return safehttp.NewClient(opts)
}
