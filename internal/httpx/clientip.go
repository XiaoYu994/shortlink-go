package httpx

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
)

/*
ClientIP 返回客户端地址，拿不到时返回空串。

默认取 RemoteAddr 的 host。部署在反向代理后面时，trustProxy 为 true
才看 X-Forwarded-For，且默认按单跳代理取最右一段合法 IP。
若同时存在 CDN/网关等多跳代理，请使用 ClientIPWithTrustedProxies 传入可信代理网段列表。
*/
func ClientIP(r *http.Request, trustProxy bool) string {
	return ClientIPWithTrustedProxies(r, trustProxy, nil)
}

/*
ClientIPWithTrustedProxies 解析真实客户端 IP，兼容单跳代理与多跳可信代理链（ADR 0024）：
  - trustProxy 为 false 时始终忽略 X-Forwarded-For，只返回 RemoteAddr；
  - trustedProxies 为空时沿用单跳策略，取 X-Forwarded-For 最右一段合法 IP；
  - trustedProxies 非空时先验证直连对端（RemoteAddr）必须属于可信网段，
    再从右向左跳过所有落入可信网段的代理 IP，返回第一个非可信代理的合法客户端 IP；
    若遇到非法 IP 字符串立即放弃解析并退回 RemoteAddr，防止左侧伪造内容混入。
*/
func ClientIPWithTrustedProxies(r *http.Request, trustProxy bool, trustedProxies []netip.Prefix) string {
	peerIP, peerOK := parseRemoteIP(r.RemoteAddr)
	if !trustProxy {
		if peerOK {
			return peerIP.String()
		}
		return ""
	}

	/*
		用 Values 拿到所有行再拼起来，不用 Get：Get 只返回第一行。
		有的代理不是在原来那行末尾追加，而是另起一行；这时第一行是客户端自己带的。
		多行等价于用逗号连成一行，最右一段仍是离我们最近的代理写的。
	*/
	forwarded := strings.Join(r.Header.Values("X-Forwarded-For"), ",")
	if len(trustedProxies) == 0 {
		if ip, ok := rightmostForwardedIP(forwarded); ok {
			return ip
		}
		if peerOK {
			return peerIP.String()
		}
		return ""
	}

	if !peerOK {
		return ""
	}
	if !isTrustedProxy(peerIP, trustedProxies) {
		return peerIP.String()
	}

	parts := strings.Split(forwarded, ",")
	var leftmostValid netip.Addr
	hasValid := false
	for i := len(parts) - 1; i >= 0; i-- {
		field := strings.TrimSpace(parts[i])
		if field == "" {
			continue
		}
		ip, err := netip.ParseAddr(field)
		if err != nil {
			return peerIP.String()
		}
		ip = ip.Unmap()
		leftmostValid = ip
		hasValid = true
		if !isTrustedProxy(ip, trustedProxies) {
			return ip.String()
		}
	}
	if hasValid {
		return leftmostValid.String()
	}
	return peerIP.String()
}

func parseRemoteIP(remoteAddr string) (netip.Addr, bool) {
	host := remoteAddr
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}, false
	}
	return ip.Unmap(), true
}

func isTrustedProxy(ip netip.Addr, prefixes []netip.Prefix) bool {
	ip = ip.Unmap()
	for _, p := range prefixes {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

/*
rightmostForwardedIP 取最右一段非空字段。
空段可以跳过（末尾多一个逗号）。遇到非 IP 立刻放弃，
不再使用它左边的值。
*/
func rightmostForwardedIP(header string) (string, bool) {
	parts := strings.Split(header, ",")
	for i := len(parts) - 1; i >= 0; i-- {
		field := strings.TrimSpace(parts[i])
		if field == "" {
			continue
		}
		ip, err := netip.ParseAddr(field)
		if err != nil {
			return "", false
		}
		return ip.Unmap().String(), true
	}
	return "", false
}
