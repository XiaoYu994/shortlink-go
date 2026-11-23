/*
Package risk 实现短链目标 URL 的异步风控检测（阶段 12、ADR 0020）。

包含四部分：
- 防 SSRF 的安全 HTTP 客户端（自定义 DialContext 杜绝 DNS Rebinding、重定向跳数与 scheme 校验、响应体上限）；
- 按目标 host 的进程内限速器，防止 worker 放大冲击第三方站点；
- 面向 Redis 的 fail-open 扫描结果缓存；
- 规则检测器与对接 internal/job 的 link.risk_scan 处理器。
*/
package risk

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"
)

const (
	DefaultFetchTimeout = 5 * time.Second
	DefaultMaxRedirects = 5
	DefaultMaxBodyBytes = 64 * 1024
)

var (
	// ErrBlockedAddress 表示目标主机解析到了回环、私网、云元数据或其他禁止访问的地址。
	ErrBlockedAddress = errors.New("risk: target address is blocked")
	// ErrInvalidScheme 表示重定向或请求使用了非 http/https 协议。
	ErrInvalidScheme = errors.New("risk: only http and https schemes are allowed")
	// ErrTooManyRedirects 表示重定向次数超过上限。
	ErrTooManyRedirects = errors.New("risk: too many redirects")
)

// 额外封禁的保留网段（IsPrivate/IsLoopback 之外的 CGNAT、文档、基准和保留地址）。
var extraBlockedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),   // RFC 6598 CGNAT
	netip.MustParsePrefix("192.0.0.0/24"),    // IETF Protocol Assignments
	netip.MustParsePrefix("192.0.2.0/24"),    // TEST-NET-1
	netip.MustParsePrefix("198.18.0.0/15"),   // Benchmarking
	netip.MustParsePrefix("198.51.100.0/24"), // TEST-NET-2
	netip.MustParsePrefix("203.0.113.0/24"),  // TEST-NET-3
	netip.MustParsePrefix("240.0.0.0/4"),     // Reserved / Broadcast
	netip.MustParsePrefix("2001:db8::/32"),   // IPv6 Documentation
}

/*
IsBlockedAddr 报告目标 IP 是否属于禁止风控抓取的内网、回环或保留地址。
先调用 Unmap() 把 ::ffff:127.0.0.1 这类 IPv4-mapped IPv6 还原成 IPv4 再判断，避免绕过。
*/
func IsBlockedAddr(addr netip.Addr) bool {
	if !addr.IsValid() {
		return true
	}
	addr = addr.Unmap()
	if addr.IsLoopback() ||
		addr.IsPrivate() ||
		addr.IsLinkLocalUnicast() ||
		addr.IsLinkLocalMulticast() ||
		addr.IsInterfaceLocalMulticast() ||
		addr.IsMulticast() ||
		addr.IsUnspecified() {
		return true
	}
	for _, prefix := range extraBlockedPrefixes {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

// HostResolver 抽象 DNS 解析，便于测试注入假解析结果。
type HostResolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

/*
HTTPClientOptions 控制防 SSRF 客户端的超时、重定向和仅测试用的精确放行地址。
AllowedHostPorts 只在单测和本地冒烟测试中显式传入（形如 127.0.0.1:18089），
不提供关闭 SSRF 检查的全局开关（ADR 0020）。
*/
type HTTPClientOptions struct {
	Timeout          time.Duration
	MaxRedirects     int
	AllowedHostPorts []string
	Resolver         HostResolver
}

// NewSafeHTTPClient 构造在拨号层拦截 SSRF、在重定向层拦截非法跳转的 HTTP 客户端。
func NewSafeHTTPClient(opts HTTPClientOptions) *http.Client {
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultFetchTimeout
	}
	maxRedirects := opts.MaxRedirects
	if maxRedirects <= 0 {
		maxRedirects = DefaultMaxRedirects
	}
	resolver := opts.Resolver
	if resolver == nil {
		resolver = net.DefaultResolver
	}

	allowed := make(map[string]struct{}, len(opts.AllowedHostPorts))
	for _, hp := range opts.AllowedHostPorts {
		hp = strings.ToLower(strings.TrimSpace(hp))
		if hp != "" {
			allowed[hp] = struct{}{}
		}
	}

	dialer := &net.Dialer{
		Timeout:   timeout,
		KeepAlive: 30 * time.Second,
	}

	transport := &http.Transport{
		Proxy:                 nil, // 禁止走环境变量 HTTP_PROXY，避免绕过本地拨号校验
		MaxIdleConns:          32,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   timeout,
		ResponseHeaderTimeout: timeout,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			return safeDialContext(ctx, dialer, resolver, allowed, network, address)
		},
	}

	return &http.Client{
		Timeout:   timeout,
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return fmt.Errorf("%w: exceeded %d hops", ErrTooManyRedirects, maxRedirects)
			}
			scheme := strings.ToLower(req.URL.Scheme)
			if scheme != "http" && scheme != "https" {
				return fmt.Errorf("%w: got %q", ErrInvalidScheme, req.URL.Scheme)
			}
			return nil
		},
	}
}

/*
safeDialContext 在真正建立 TCP 连接前解析并校验所有候选 IP，然后直接对已通过校验的 IP 拨号。
这样 DNS 解析与连接建立使用同一个 IP，杜绝先校验后拨号的 DNS Rebinding（TOCTOU）漏洞。
*/
func safeDialContext(
	ctx context.Context,
	dialer *net.Dialer,
	resolver HostResolver,
	allowed map[string]struct{},
	network, address string,
) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("invalid dial address %q: %w", address, err)
	}
	if _, ok := allowed[strings.ToLower(net.JoinHostPort(host, port))]; ok {
		return dialer.DialContext(ctx, network, address)
	}

	var ips []netip.Addr
	if literal, err := netip.ParseAddr(host); err == nil {
		ips = []netip.Addr{literal}
	} else {
		ips, err = resolver.LookupNetIP(ctx, "ip", host)
		if err != nil {
			return nil, fmt.Errorf("resolve %q: %w", host, err)
		}
		if len(ips) == 0 {
			return nil, fmt.Errorf("resolve %q: no addresses", host)
		}
	}

	// 只要解析结果里包含任意受禁地址就直接拒绝，防止双记录轮询绕过。
	for _, ip := range ips {
		if IsBlockedAddr(ip) {
			return nil, fmt.Errorf("%w: %s resolved to %s", ErrBlockedAddress, host, ip.Unmap())
		}
	}

	var lastErr error
	for _, ip := range ips {
		target := net.JoinHostPort(ip.Unmap().String(), port)
		conn, err := dialer.DialContext(ctx, network, target)
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	return nil, lastErr
}
