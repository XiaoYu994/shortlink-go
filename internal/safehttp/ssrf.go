/*
Package safehttp 提供跨后台任务与出站校验共用的防 SSRF 安全 HTTP 客户端与目标主机限速原语（ADR 0020、ADR 0027）。

所有出站连接都在拨号层解析并校验全部候选 IP 与端口后再直接对已校验 IP 拨号，
禁止走环境变量代理（HTTP_PROXY），杜绝 DNS Rebinding 与内网地址绕过。
*/
package safehttp

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
)

const (
	DefaultTimeout      = 5 * time.Second
	DefaultMaxRedirects = 5
	DefaultMaxURLLength = 2048
)

var (
	// ErrBlockedAddress 表示目标主机属于或解析到了回环、私网、云元数据或其他禁止访问的地址。
	ErrBlockedAddress = errors.New("safehttp: target address is blocked")
	// ErrBlockedPort 表示目标端口不在允许的出站端口白名单内（默认仅 80/443）。
	ErrBlockedPort = errors.New("safehttp: target port is blocked")
	// ErrInvalidScheme 表示目标或重定向使用了非 http/https 协议。
	ErrInvalidScheme = errors.New("safehttp: only http and https schemes are allowed")
	// ErrTooManyRedirects 表示重定向次数超过上限。
	ErrTooManyRedirects = errors.New("safehttp: too many redirects")
	// ErrInvalidURL 表示 URL 格式非法、超长、包含用户信息或片段等不允许的成分。
	ErrInvalidURL = errors.New("safehttp: invalid url")
)

// 额外封禁的保留网段（IsPrivate/IsLoopback 之外的 CGNAT、文档、过渡隧道、NAT64、基准和保留地址）。
var extraBlockedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),   // RFC 6598 CGNAT
	netip.MustParsePrefix("192.0.0.0/24"),    // IETF Protocol Assignments
	netip.MustParsePrefix("192.0.2.0/24"),    // TEST-NET-1
	netip.MustParsePrefix("198.18.0.0/15"),   // Benchmarking
	netip.MustParsePrefix("198.51.100.0/24"), // TEST-NET-2
	netip.MustParsePrefix("203.0.113.0/24"),  // TEST-NET-3
	netip.MustParsePrefix("240.0.0.0/4"),     // Reserved / Broadcast
	netip.MustParsePrefix("100::/64"),        // RFC 6666 Discard-Only
	netip.MustParsePrefix("2001:db8::/32"),   // IPv6 Documentation
	netip.MustParsePrefix("2001::/32"),       // RFC 4380 Teredo
	netip.MustParsePrefix("2002::/16"),       // RFC 3056 6to4
	netip.MustParsePrefix("64:ff9b::/96"),    // RFC 6052 NAT64
	netip.MustParsePrefix("64:ff9b:1::/48"),  // RFC 8215 Local-Use NAT64
	netip.MustParsePrefix("fec0::/10"),       // RFC 3879 Site-Local (deprecated)
}

/*
IsBlockedAddr 报告目标 IP 是否属于禁止出站访问的内网、回环或保留地址。
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
ClientOptions 控制防 SSRF 客户端的超时、重定向策略和仅测试用的精确放行地址。
AllowedHostPorts 只在单测和本地冒烟测试中显式传入（形如 127.0.0.1:18089），
不提供关闭 SSRF 检查的全局开关（ADR 0020、ADR 0027）。
*/
type ClientOptions struct {
	Timeout          time.Duration
	MaxRedirects     int
	DisableRedirects bool
	AllowedPorts     []int
	AllowedHostPorts []string
	Resolver         HostResolver
}

// URLValidationOptions 控制静态入口 URL 校验的端口与测试/冒烟精确放行地址。
type URLValidationOptions struct {
	AllowedPorts     []int
	AllowedHostPorts []string
	MaxLength        int
}

// DefaultAllowedPorts 返回默认允许的出站端口白名单（HTTP 80 与 HTTPS 443）。
func DefaultAllowedPorts() []int {
	return []int{80, 443}
}

/*
ValidateURL 在订阅创建/更新等同步入口预先拦截明显非法的出站 URL：
- 只允许 http/https，拒绝控制字符、userinfo 与 #fragment；
- 要求合法主机名与允许的端口（默认 80/443）；
- 除非显式命中 AllowedHostPorts（仅测试/本地冒烟使用），否则直接拦截 localhost 与字面量内网/保留 IP。
动态域名在真实出站时仍由 safeDialContext 二次防 DNS Rebinding 校验。
*/
func ValidateURL(raw string, opts URLValidationOptions) (*url.URL, error) {
	trimmed := strings.TrimSpace(raw)
	maxLen := opts.MaxLength
	if maxLen <= 0 {
		maxLen = DefaultMaxURLLength
	}
	if trimmed == "" || len(trimmed) > maxLen {
		return nil, fmt.Errorf("%w: length must be between 1 and %d bytes", ErrInvalidURL, maxLen)
	}
	if strings.ContainsFunc(trimmed, func(r rune) bool { return unicode.IsControl(r) || unicode.IsSpace(r) }) {
		return nil, fmt.Errorf("%w: contains control or whitespace character", ErrInvalidURL)
	}

	u, err := url.Parse(trimmed)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidURL, err)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return nil, fmt.Errorf("%w: %q", ErrInvalidScheme, u.Scheme)
	}
	if u.User != nil || u.Fragment != "" {
		return nil, fmt.Errorf("%w: userinfo and fragment are not allowed", ErrInvalidURL)
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return nil, fmt.Errorf("%w: host is required", ErrInvalidURL)
	}

	port := 80
	if scheme == "https" {
		port = 443
	}
	if rawPort := u.Port(); rawPort != "" {
		parsedPort, err := strconv.Atoi(rawPort)
		if err != nil || parsedPort < 1 || parsedPort > 65535 {
			return nil, fmt.Errorf("%w: %q", ErrBlockedPort, rawPort)
		}
		port = parsedPort
	}

	allowedHostPorts := normalizeHostPorts(opts.AllowedHostPorts)
	if _, ok := allowedHostPorts[strings.ToLower(net.JoinHostPort(host, strconv.Itoa(port)))]; ok {
		return u, nil
	}

	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return nil, fmt.Errorf("%w: localhost is not allowed", ErrBlockedAddress)
	}
	if addr, err := netip.ParseAddr(host); err == nil {
		if IsBlockedAddr(addr) {
			return nil, fmt.Errorf("%w: %s is blocked", ErrBlockedAddress, addr.Unmap())
		}
	}

	allowedPorts := buildAllowedPorts(opts.AllowedPorts)
	if _, ok := allowedPorts[port]; !ok {
		return nil, fmt.Errorf("%w: port %d is not allowed", ErrBlockedPort, port)
	}
	return u, nil
}

// NewClient 构造在拨号层拦截 SSRF、在重定向层按策略控制跳转的 HTTP 客户端。
func NewClient(opts ClientOptions) *http.Client {
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	maxRedirects := opts.MaxRedirects
	if maxRedirects <= 0 {
		maxRedirects = DefaultMaxRedirects
	}
	resolver := opts.Resolver
	if resolver == nil {
		resolver = net.DefaultResolver
	}

	allowedPorts := buildAllowedPorts(opts.AllowedPorts)
	allowed := normalizeHostPorts(opts.AllowedHostPorts)

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
			return safeDialContext(ctx, dialer, resolver, allowedPorts, allowed, network, address)
		},
	}

	checkRedirect := func(req *http.Request, via []*http.Request) error {
		if len(via) >= maxRedirects {
			return fmt.Errorf("%w: exceeded %d hops", ErrTooManyRedirects, maxRedirects)
		}
		scheme := strings.ToLower(req.URL.Scheme)
		if scheme != "http" && scheme != "https" {
			return fmt.Errorf("%w: got %q", ErrInvalidScheme, req.URL.Scheme)
		}
		return nil
	}
	if opts.DisableRedirects {
		checkRedirect = func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}
	}

	return &http.Client{
		Timeout:       timeout,
		Transport:     transport,
		CheckRedirect: checkRedirect,
	}
}

func buildAllowedPorts(ports []int) map[int]struct{} {
	if len(ports) == 0 {
		ports = DefaultAllowedPorts()
	}
	allowedPorts := make(map[int]struct{}, len(ports))
	for _, p := range ports {
		if p >= 1 && p <= 65535 {
			allowedPorts[p] = struct{}{}
		}
	}
	return allowedPorts
}

func normalizeHostPorts(hostPorts []string) map[string]struct{} {
	allowed := make(map[string]struct{}, len(hostPorts))
	for _, hp := range hostPorts {
		hp = strings.ToLower(strings.TrimSpace(hp))
		if hp != "" {
			allowed[hp] = struct{}{}
		}
	}
	return allowed
}

/*
safeDialContext 在真正建立 TCP 连接前解析并校验所有候选 IP 与端口，然后直接对已通过校验的 IP 拨号。
这样 DNS 解析与连接建立使用同一个 IP，杜绝先校验后拨号的 DNS Rebinding（TOCTOU）漏洞。
*/
func safeDialContext(
	ctx context.Context,
	dialer *net.Dialer,
	resolver HostResolver,
	allowedPorts map[int]struct{},
	allowed map[string]struct{},
	network, address string,
) (net.Conn, error) {
	host, portStr, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("invalid dial address %q: %w", address, err)
	}
	if _, ok := allowed[strings.ToLower(net.JoinHostPort(host, portStr))]; ok {
		return dialer.DialContext(ctx, network, address)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		return nil, fmt.Errorf("%w: %q", ErrBlockedPort, portStr)
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
	if _, ok := allowedPorts[port]; !ok {
		return nil, fmt.Errorf("%w: port %d is not allowed", ErrBlockedPort, port)
	}

	var lastErr error
	for _, ip := range ips {
		target := net.JoinHostPort(ip.Unmap().String(), portStr)
		conn, err := dialer.DialContext(ctx, network, target)
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	return nil, lastErr
}
