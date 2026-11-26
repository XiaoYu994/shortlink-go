package logx

import (
	"log/slog"
	"net/url"
	"strings"
)

// Redacted 是敏感字段与凭据脱敏后的统一占位符。
const Redacted = "[REDACTED]"

// RedactSecret 对非空敏感字符串返回固定占位符，保留空值以便区分“未配置”与“已配置”。
func RedactSecret(s string) string {
	if s == "" {
		return ""
	}
	return Redacted
}

/*
RedactPhone 脱敏手机号中间位（如 +8613800138000 -> +86****8000），
供日志与用户默认展示名共用，作为全仓库手机号脱敏的单一事实来源（ADR 0030）。
*/
func RedactPhone(phone string) string {
	n := len(phone)
	switch {
	case n <= 5:
		return "****"
	case n <= 9:
		return phone[:2] + "****" + phone[n-2:]
	default:
		return phone[:3] + "****" + phone[n-4:]
	}
}

/*
RedactURL 对数据库连接串、Redis URL 或带查询凭据的 URL 做脱敏：
 1. 借 url.URL.Redacted() 将 userinfo 中的密码掩码为 xxxxx；
 2. 对常见敏感查询参数（如 password、token、secret、api_key）将值替换为 REDACTED，
    仅在确实命中敏感参数时重写 RawQuery，保证 sslmode=disable 等安全参数原样保留。
 3. 解析失败时返回 Redacted，防止畸形串携带未转义密码泄漏。
*/
func RedactURL(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return Redacted
	}
	if u.RawQuery != "" {
		q := u.Query()
		changed := false
		for k := range q {
			if isSensitiveQueryParam(k) {
				q.Set(k, "REDACTED")
				changed = true
			}
		}
		if changed {
			u.RawQuery = q.Encode()
		}
	}
	return u.Redacted()
}

func isSensitiveQueryParam(k string) bool {
	switch strings.ToLower(strings.TrimSpace(k)) {
	case "password", "passwd", "secret", "token", "access_token", "refresh_token",
		"api_key", "apikey", "key", "auth":
		return true
	default:
		return false
	}
}

/*
replaceAttr 作为 slog.HandlerOptions.ReplaceAttr 的全局脱敏防线。
注意：刻意不拦截 "key"（internal/cache/store.go 用 "key" 记录 Redis 缓存键名）。
*/
func replaceAttr(_ []string, a slog.Attr) slog.Attr {
	if a.Value.Kind() == slog.KindGroup {
		return a
	}
	switch strings.ToLower(strings.TrimSpace(a.Key)) {
	case "database_url", "redis_url", "dsn":
		if a.Value.Kind() == slog.KindString && a.Value.String() != "" {
			return slog.String(a.Key, RedactURL(a.Value.String()))
		}
	case "authorization", "cookie", "set-cookie",
		"password", "passwd", "secret", "client_secret",
		"token", "access_token", "refresh_token", "id_token",
		"api_key", "apikey", "visitor_salt", "sl_session":
		if a.Value.String() != "" {
			return slog.String(a.Key, Redacted)
		}
	}
	return a
}
