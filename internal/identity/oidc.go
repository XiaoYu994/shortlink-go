package identity

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

const (
	defaultOIDCHTTPTimeout = 10 * time.Second
	flowRandomBytes        = 32
)

var (
	// ErrOIDCAuthFailed 表示授权码兑换被拒绝或 ID Token 校验失败（签名、iss、aud、exp、nonce 或声明非法）。
	ErrOIDCAuthFailed = errors.New("oidc authentication failed")
	// ErrOIDCUnavailable 表示身份提供方不可达、返回 5xx 或客户端凭据配置异常。
	ErrOIDCUnavailable = errors.New("oidc provider unavailable")
	// ErrNonceMismatch 表示 ID Token 里的 nonce 与登录流程 Cookie 中的值不一致。
	ErrNonceMismatch = errors.New("oidc nonce mismatch")
)

// OIDCConfig 是构造 OIDCProvider 所需的参数。
type OIDCConfig struct {
	Issuer       string
	ClientID     string
	ClientSecret string
	RedirectURL  string
	// HTTPClient 可选，用于发现文档、令牌交换和 JWKS 拉取；为空时使用带超时的默认客户端。
	HTTPClient *http.Client
}

// OIDCFlow 保存单次授权码登录生成的三个随机值，由 HTTP 层写入短期 HttpOnly Cookie。
type OIDCFlow struct {
	State        string
	CodeVerifier string
	Nonce        string
}

/*
NewOIDCFlow 生成单次授权码流程所需的随机 state、PKCE code_verifier 和 nonce。
state 和 nonce 取 32 字节随机数做 base64url 编码；code_verifier 使用 oauth2.GenerateVerifier。
*/
func NewOIDCFlow() (OIDCFlow, error) {
	state, err := randomURLToken(flowRandomBytes)
	if err != nil {
		return OIDCFlow{}, fmt.Errorf("generate oidc state: %w", err)
	}
	nonce, err := randomURLToken(flowRandomBytes)
	if err != nil {
		return OIDCFlow{}, fmt.Errorf("generate oidc nonce: %w", err)
	}
	return OIDCFlow{
		State:        state,
		CodeVerifier: oauth2.GenerateVerifier(),
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

/*
OIDCProvider 封装标准 OIDC 发现、授权码跳转、令牌兑换和 ID Token 校验（ADR 0018）。
密码学验签与 JWKS 缓存由 coreos/go-oidc/v3 负责，授权码兑换由 golang.org/x/oauth2 负责；
go-oidc 不校验 nonce，由 ExchangeAndVerify 与调用方传入的预期值做常量时间比对。
*/
type OIDCProvider struct {
	oauth         oauth2.Config
	verifier      *oidc.IDTokenVerifier
	httpClient    *http.Client
	secureCookies bool
	// flowCookieKey 给登录流程 Cookie 做 HMAC。由客户端密钥派生，同一套配置的副本算出同一把密钥。
	flowCookieKey []byte
}

/*
NewOIDCProvider 通过发现文档初始化 OIDC 提供方。
ctx 控制启动期拉取发现文档的时限；JWKS 验证器使用同一个 HTTPClient，避免退回无超时的 DefaultClient。
*/
func NewOIDCProvider(ctx context.Context, cfg OIDCConfig) (*OIDCProvider, error) {
	if cfg.Issuer == "" || cfg.ClientID == "" || cfg.ClientSecret == "" || cfg.RedirectURL == "" {
		return nil, errors.New("oidc config: issuer, client id, client secret, and redirect url are required")
	}
	issuerURL, err := url.Parse(cfg.Issuer)
	if err != nil || (issuerURL.Scheme != "http" && issuerURL.Scheme != "https") || issuerURL.Host == "" || issuerURL.RawQuery != "" || issuerURL.Fragment != "" {
		return nil, fmt.Errorf("oidc config: invalid issuer %q", cfg.Issuer)
	}
	redirectURL, err := url.Parse(cfg.RedirectURL)
	if err != nil || (redirectURL.Scheme != "http" && redirectURL.Scheme != "https") || redirectURL.Host == "" || redirectURL.Fragment != "" {
		return nil, fmt.Errorf("oidc config: invalid redirect url %q", cfg.RedirectURL)
	}

	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: defaultOIDCHTTPTimeout}
	}

	provider, err := oidc.NewProvider(oidc.ClientContext(ctx, httpClient), cfg.Issuer)
	if err != nil {
		return nil, fmt.Errorf("oidc discovery %q: %w", cfg.Issuer, err)
	}

	/*
		VerifierContext 把自定义 HTTPClient 传给上游 JWKS 拉取（取消信号由单次 Verify 的 ctx 控制），
		避免使用无超时的 http.DefaultClient。
	*/
	verifier := provider.VerifierContext(
		oidc.ClientContext(context.Background(), httpClient),
		&oidc.Config{ClientID: cfg.ClientID},
	)

	return &OIDCProvider{
		oauth: oauth2.Config{
			ClientID:     cfg.ClientID,
			ClientSecret: cfg.ClientSecret,
			Endpoint:     provider.Endpoint(),
			RedirectURL:  cfg.RedirectURL,
			Scopes:       []string{oidc.ScopeOpenID, "email", "profile"},
		},
		verifier:      verifier,
		httpClient:    httpClient,
		secureCookies: strings.EqualFold(redirectURL.Scheme, "https"),
		flowCookieKey: deriveFlowCookieKey(cfg.ClientSecret),
	}, nil
}

/*
deriveFlowCookieKey 从客户端密钥派生流程 Cookie 的 HMAC 密钥。
不直接拿客户端密钥当 HMAC 密钥，避免两处用途混在同一个字节串上。
密钥只跟配置走，console 多副本不需要共享内存。
*/
func deriveFlowCookieKey(clientSecret string) []byte {
	sum := sha256.Sum256([]byte("shortlink-oidc-flow-v1\x00" + clientSecret))
	return sum[:]
}

/*
SecureCookies 报告登录流程 Cookie 和会话 Cookie 是否应带 Secure 属性。
按 OIDC_REDIRECT_URL 的协议决定：生产环境回调地址是 https://，即使 TLS 在反向代理终结也会设 Secure；
本地开发和冒烟测试走 http://127.0.0.1，curl 和 http/cookiejar 不会在 http 下发送 Secure Cookie。
*/
func (p *OIDCProvider) SecureCookies() bool {
	return p.secureCookies
}

// FlowCookieKey 返回流程 Cookie 的 HMAC 密钥副本，调用方拿去签名，改不到提供方自己的那份。
func (p *OIDCProvider) FlowCookieKey() []byte {
	return bytes.Clone(p.flowCookieKey)
}

// AuthCodeURL 构造带 state、PKCE S256 challenge 和 nonce 的授权跳转地址。
func (p *OIDCProvider) AuthCodeURL(state, codeVerifier, nonce string) string {
	return p.oauth.AuthCodeURL(
		state,
		oauth2.S256ChallengeOption(codeVerifier),
		oidc.Nonce(nonce),
	)
}

/*
ExchangeAndVerify 用授权码和 PKCE code_verifier 向令牌端点换取令牌，
再校验 ID Token 的签名、iss、aud、exp 和 expectedNonce，返回规范化后的 OIDCIdentity。
ID Token 只在这里消费一次，不持久化（ADR 0018）。
*/
func (p *OIDCProvider) ExchangeAndVerify(ctx context.Context, code, codeVerifier, expectedNonce string) (OIDCIdentity, error) {
	if code == "" || codeVerifier == "" || expectedNonce == "" {
		return OIDCIdentity{}, fmt.Errorf("%w: code, code_verifier, and nonce are required", ErrOIDCAuthFailed)
	}

	callCtx := oidc.ClientContext(ctx, p.httpClient)
	token, err := p.oauth.Exchange(callCtx, code, oauth2.VerifierOption(codeVerifier))
	if err != nil {
		return OIDCIdentity{}, classifyExchangeError(ctx, err)
	}

	rawIDToken, ok := token.Extra("id_token").(string)
	if !ok || rawIDToken == "" {
		return OIDCIdentity{}, fmt.Errorf("%w: missing id_token", ErrOIDCAuthFailed)
	}

	idToken, err := p.verifier.Verify(callCtx, rawIDToken)
	if err != nil {
		return OIDCIdentity{}, classifyVerifyError(ctx, err)
	}

	// go-oidc 只把 nonce 解析到 IDToken.Nonce，不自动比对，这里显式做常量时间比较。
	if idToken.Nonce == "" || subtle.ConstantTimeCompare([]byte(idToken.Nonce), []byte(expectedNonce)) != 1 {
		return OIDCIdentity{}, fmt.Errorf("%w: %w", ErrOIDCAuthFailed, ErrNonceMismatch)
	}

	var claims struct {
		Email         string `json:"email"`
		EmailVerified bool   `json:"email_verified"`
		Name          string `json:"name"`
	}
	if err := idToken.Claims(&claims); err != nil {
		return OIDCIdentity{}, fmt.Errorf("%w: decode id token claims: %w", ErrOIDCAuthFailed, err)
	}

	id, err := (OIDCIdentity{
		Issuer:        idToken.Issuer,
		Subject:       idToken.Subject,
		Email:         claims.Email,
		EmailVerified: claims.EmailVerified,
		Name:          claims.Name,
	}).Normalize()
	if err != nil {
		return OIDCIdentity{}, fmt.Errorf("%w: %w", ErrOIDCAuthFailed, err)
	}
	return id, nil
}

/*
classifyExchangeError 把令牌端点的失败分成两类：
请求 context 取消/超时保留原上下文错误；
400 里的 invalid_grant 等客户端错误归为 ErrOIDCAuthFailed（401）；
400 的 server_error、temporarily_unavailable，以及 401 invalid_client、5xx、网络故障归为 ErrOIDCUnavailable（502）。
*/
func classifyExchangeError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return fmt.Errorf("exchange authorization code: %w", ctx.Err())
	}
	var retrieveErr *oauth2.RetrieveError
	if errors.As(err, &retrieveErr) && retrieveErr.Response != nil && retrieveErr.Response.StatusCode == http.StatusBadRequest {
		switch retrieveErr.ErrorCode {
		case "server_error", "temporarily_unavailable":
			return fmt.Errorf("%w: %w", ErrOIDCUnavailable, err)
		default:
			return fmt.Errorf("%w: %w", ErrOIDCAuthFailed, err)
		}
	}
	return fmt.Errorf("%w: %w", ErrOIDCUnavailable, err)
}

/*
classifyVerifyError 把 ID Token 校验失败分成两类。
go-oidc 没有给 JWKS 拉取失败单独的哨兵错误，远程失败的包装文本稳定地带有 "fetching keys"
（v3.21 的 RemoteKeySet.verify）。签名、iss、aud、exp 不带这段文本，仍算认证失败。
*/
func classifyVerifyError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return fmt.Errorf("verify id token: %w", ctx.Err())
	}
	if strings.Contains(err.Error(), "fetching keys") {
		return fmt.Errorf("%w: verify id token: %w", ErrOIDCUnavailable, err)
	}
	return fmt.Errorf("%w: verify id token: %w", ErrOIDCAuthFailed, err)
}
