package identity

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
)

const (
	flowRandomBytes = 32
	// GitHubIssuer 是放进 user_identities.issuer 的固定标识，与 OIDC 提供方的 issuer 共同组成唯一键。
	GitHubIssuer = "https://github.com"
)

var (
	// ErrAuthFailed 表示授权码兑换被拒绝、访问令牌无效、或 ID Token 校验/身份声明非法。
	ErrAuthFailed = errors.New("authentication failed")
	// ErrProviderUnavailable 表示身份提供方不可达、返回 5xx 或客户端凭据配置异常。
	ErrProviderUnavailable = errors.New("identity provider unavailable")
	// ErrNonceMismatch 表示 ID Token 里的 nonce 与登录流程 Cookie 中的值不一致。
	ErrNonceMismatch = errors.New("oidc nonce mismatch")
)

// OAuthFlow 保存纯 OAuth 2.0 授权码登录生成的随机 state 和 PKCE code_verifier（没有 ID Token，所以不需要 nonce）。
type OAuthFlow struct {
	State        string
	CodeVerifier string
}

// OIDCFlow 保存单次 OIDC 授权码登录生成的三个随机值，由 HTTP 层写入短期 HttpOnly Cookie。
type OIDCFlow struct {
	State        string
	CodeVerifier string
	Nonce        string
}

/*
NewOAuthFlow 生成纯 OAuth 2.0 授权码流程所需的随机 state 和 PKCE code_verifier。
两项均使用 32 字节密码学随机数做 base64url（无填充）编码，满足 RFC 7636 PKCE 长度与字符集要求。
*/
func NewOAuthFlow() (OAuthFlow, error) {
	state, err := randomURLToken(flowRandomBytes)
	if err != nil {
		return OAuthFlow{}, fmt.Errorf("generate oauth state: %w", err)
	}
	verifier, err := randomURLToken(flowRandomBytes)
	if err != nil {
		return OAuthFlow{}, fmt.Errorf("generate oauth code verifier: %w", err)
	}
	return OAuthFlow{
		State:        state,
		CodeVerifier: verifier,
	}, nil
}

// NewOIDCFlow 生成单次授权码流程所需的随机 state、PKCE code_verifier 和 nonce。
func NewOIDCFlow() (OIDCFlow, error) {
	oauthFlow, err := NewOAuthFlow()
	if err != nil {
		return OIDCFlow{}, err
	}
	nonce, err := randomURLToken(flowRandomBytes)
	if err != nil {
		return OIDCFlow{}, fmt.Errorf("generate oidc nonce: %w", err)
	}
	return OIDCFlow{
		State:        oauthFlow.State,
		CodeVerifier: oauthFlow.CodeVerifier,
		Nonce:        nonce,
	}, nil
}

func randomURLToken(nBytes int) (string, error) {
	buf := make([]byte, nBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
