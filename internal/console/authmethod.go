package console

import (
	"log/slog"
	"net/http"
	"strings"

	"shortlink/internal/identity"
)

// AuthFlowKind 标识登录插件的前端交互形态，避免前端按提供方名称硬编码。
type AuthFlowKind string

const (
	// AuthFlowRedirect 表示浏览器跳转型授权登录（OIDC、GitHub、Google、飞书等）。
	AuthFlowRedirect AuthFlowKind = "redirect"
	// AuthFlowPhoneOTP 预留给后续手机号短信验证码登录插件使用（ADR 0029）。
	AuthFlowPhoneOTP AuthFlowKind = "phone_otp"
)

// AuthMethodDescriptor 随 GET /api/v1/capabilities 下发，供前端动态渲染登录入口。
type AuthMethodDescriptor struct {
	ID            string       `json:"id"`
	Kind          AuthFlowKind `json:"kind"`
	LoginURL      string       `json:"login_url,omitempty"`
	SendCodeURL   string       `json:"send_code_url,omitempty"`
	VerifyCodeURL string       `json:"verify_code_url,omitempty"`
}

// IssueSessionOptions 控制认证成功后的会话 Cookie 安全属性与可选站内重定向。
type IssueSessionOptions struct {
	SecureCookie bool
	ReturnTo     string
}

/*
SessionIssuer 把“凭证核验通过后的统一建会话、审计上下文与种 Cookie”收敛到认证底座。
各登录插件（OAuth 跳转、手机号验证码等）只负责前半段核验并产出 identity.ExternalIdentity。
*/
type SessionIssuer interface {
	IssueSession(w http.ResponseWriter, r *http.Request, id identity.ExternalIdentity, opts IssueSessionOptions)
}

// AuthToolkit 向 AuthMethod 插件提供受控的路由注册与通用底座依赖。
type AuthToolkit struct {
	Mux         *http.ServeMux
	IPRateLimit func(http.Handler) http.Handler
	CSRF        *http.CrossOriginProtection
	Issuer      SessionIssuer
	Logger      *slog.Logger
}

/*
AuthMethod 是所有用户登录方式的统一插件接口（ADR 0029）。
跳转型 OAuth/OIDC 通过 NewOAuthAuthMethod 适配，表单/验证码型（如手机号 OTP）直接实现本接口。
*/
type AuthMethod interface {
	Descriptor() AuthMethodDescriptor
	SecureCookies() bool
	RegisterRoutes(tk AuthToolkit)
}

func validAuthMethodID(id string) bool {
	if id == "" || len(id) > 32 || strings.TrimSpace(id) != id {
		return false
	}
	for i := range len(id) {
		c := id[i]
		isLower := c >= 'a' && c <= 'z'
		isDigit := c >= '0' && c <= '9'
		if !isLower && !isDigit && c != '-' && c != '_' {
			return false
		}
	}
	return true
}
