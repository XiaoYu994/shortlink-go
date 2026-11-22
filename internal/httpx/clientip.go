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
才看 X-Forwarded-For，并且只取最右一段合法 IP。
nginx 的 $proxy_add_x_forwarded_for 是把对端地址追加在末尾；
最左一段是客户端自己带来的，可以伪造。最右一段不合法时不往左找，
改回 RemoteAddr，避免用到伪造值。
这个开关只能开在可信代理后面。代理应覆盖成它看到的对端地址；
见 deploy/nginx.conf。
*/
func ClientIP(r *http.Request, trustProxy bool) string {
	if trustProxy {
		/*
			用 Values 拿到所有行再拼起来，不用 Get：Get 只返回第一行。
			有的代理不是在原来那行末尾追加，而是另起一行；这时第一行是客户端自己带的。
			多行等价于用逗号连成一行，最右一段仍是离我们最近的代理写的。
		*/
		forwarded := strings.Join(r.Header.Values("X-Forwarded-For"), ",")
		if ip, ok := rightmostForwardedIP(forwarded); ok {
			return ip
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
