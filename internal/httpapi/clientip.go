package httpapi

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
)

/*
clientIP 返回用于访客哈希的客户端地址，拿不到时返回空串。

默认取 RemoteAddr 的 host。部署在反向代理后面时，TRUST_PROXY_HEADERS=true
才看 X-Forwarded-For：只取第一段并校验成 IP，防止把整串头写进哈希。
这个开关只能开在可信代理后面，否则客户端可以伪造 IP 刷高 UV。
*/
func clientIP(r *http.Request, trustProxy bool) string {
	if trustProxy {
		first, _, _ := strings.Cut(r.Header.Get("X-Forwarded-For"), ",")
		if ip, err := netip.ParseAddr(strings.TrimSpace(first)); err == nil {
			return ip.Unmap().String()
		}
	}

	host := r.RemoteAddr
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		return ip.Unmap().String()
	}
	return ""
}
