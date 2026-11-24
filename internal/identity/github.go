package identity

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"golang.org/x/oauth2"

	"shortlink/internal/logx"
)

const (
	// GitHubIssuer 是放进 user_identities.issuer 的固定标识，与 OIDC 提供方的 issuer 共同组成唯一键。
	GitHubIssuer = "https://github.com"

	defaultGitHubAuthURL    = "https://github.com/login/oauth/authorize"
	defaultGitHubTokenURL   = "https://github.com/login/oauth/access_token"
	defaultGitHubAPIBaseURL = "https://api.github.com"

	githubAPIVersion       = "2022-11-28"
	githubUserAgent        = "shortlink"
	maxGitHubResponseBytes = 1 << 20
)

/*
GitHubConfig 是构造 GitHubProvider 所需的参数。
ClientID、ClientSecret 和 RedirectURL 必填；AuthURL、TokenURL、APIBaseURL 与 HTTPClient
留空时使用 GitHub 官方地址和带超时的默认客户端，只在测试里替换为本地假服务。
*/
type GitHubConfig struct {
	ClientID     string
	ClientSecret string
	RedirectURL  string

	AuthURL    string
	TokenURL   string
	APIBaseURL string
	HTTPClient *http.Client
}

// LogValue 掩码 ClientSecret，防止日志直接打印 GitHubConfig 时泄漏密钥。
func (c GitHubConfig) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("client_id", c.ClientID),
		slog.String("client_secret", logx.RedactSecret(c.ClientSecret)),
		slog.String("redirect_url", c.RedirectURL),
	)
}

// String 实现 fmt.Stringer，防止 %+v/%v 意外打印出 ClientSecret。
func (c GitHubConfig) String() string {
	return c.LogValue().String()
}

/*
GitHubProvider 封装纯 OAuth 2.0 的 GitHub 登录流程（阶段 11、ADR 0018）。
与 OIDCProvider 不同：GitHub 没有发现文档，也不签发 ID Token（因此没有 nonce 和离线 JWKS 验签）；
换回 access_token 后必须再在线请求 /user 与 /user/emails 才能拿到稳定数字用户 ID 和主邮箱验证状态。
*/
type GitHubProvider struct {
	oauth         oauth2.Config
	apiBaseURL    string
	httpClient    *http.Client
	secureCookies bool
	flowCookieKey []byte
}

type githubUserResponse struct {
	ID    int64  `json:"id"`
	Login string `json:"login"`
	Name  string `json:"name"`
}

type githubEmailResponse struct {
	Email    string `json:"email"`
	Primary  bool   `json:"primary"`
	Verified bool   `json:"verified"`
}

/*
NewGitHubProvider 校验配置并构造 GitHub OAuth 2.0 客户端。
因为没有发现文档，启动阶段不发网络请求；OAuth 端点认证风格固定为 AuthStyleInParams，匹配 GitHub 的表单参数约定。
*/
func NewGitHubProvider(cfg GitHubConfig) (*GitHubProvider, error) {
	if cfg.ClientID == "" || cfg.ClientSecret == "" || cfg.RedirectURL == "" {
		return nil, errors.New("github config: client id, client secret, and redirect url are required")
	}
	redirectURL, err := parseRedirectURL(cfg.RedirectURL, "github config", "redirect url")
	if err != nil {
		return nil, err
	}
	authURL, err := optionalEndpointURL(cfg.AuthURL, defaultGitHubAuthURL, "auth url")
	if err != nil {
		return nil, err
	}
	tokenURL, err := optionalEndpointURL(cfg.TokenURL, defaultGitHubTokenURL, "token url")
	if err != nil {
		return nil, err
	}
	apiBaseURL, err := optionalEndpointURL(cfg.APIBaseURL, defaultGitHubAPIBaseURL, "api base url")
	if err != nil {
		return nil, err
	}
	apiBaseURL = strings.TrimRight(apiBaseURL, "/")

	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: defaultProviderHTTPTimeout}
	}

	return &GitHubProvider{
		oauth: oauth2.Config{
			ClientID:     cfg.ClientID,
			ClientSecret: cfg.ClientSecret,
			Endpoint: oauth2.Endpoint{
				AuthURL:   authURL,
				TokenURL:  tokenURL,
				AuthStyle: oauth2.AuthStyleInParams,
			},
			RedirectURL: cfg.RedirectURL,
			Scopes:      []string{"read:user", "user:email"},
		},
		apiBaseURL:    apiBaseURL,
		httpClient:    httpClient,
		secureCookies: strings.EqualFold(redirectURL.Scheme, "https"),
		flowCookieKey: deriveFlowCookieKey("shortlink-github-flow-v1", cfg.ClientSecret),
	}, nil
}

func optionalEndpointURL(raw, fallback, field string) (string, error) {
	if raw == "" {
		return fallback, nil
	}
	if _, err := parseEndpointURL(raw, "github config", field); err != nil {
		return "", err
	}
	return raw, nil
}

// SecureCookies 报告登录流程 Cookie 和会话 Cookie 是否应带 Secure 属性（按 GITHUB_REDIRECT_URL 的协议决定）。
func (p *GitHubProvider) SecureCookies() bool {
	return p.secureCookies
}

// FlowCookieKey 返回 GitHub 流程 Cookie 的 HMAC 密钥副本。
func (p *GitHubProvider) FlowCookieKey() []byte {
	return bytes.Clone(p.flowCookieKey)
}

// AuthCodeURL 构造带 state 和 PKCE S256 challenge 的 GitHub 授权跳转地址（纯 OAuth 2.0，不传 nonce）。
func (p *GitHubProvider) AuthCodeURL(state, codeVerifier string) string {
	return p.oauth.AuthCodeURL(state, oauth2.S256ChallengeOption(codeVerifier))
}

/*
ExchangeAndFetchUser 用授权码和 PKCE code_verifier 向 GitHub 换取 access_token，
再在线请求 /user 与 /user/emails 构造规范化的 ExternalIdentity（Subject 取不可变的十进制数字用户 ID）。
access_token 只在本次请求里使用，不持久化也不写入日志。
*/
func (p *GitHubProvider) ExchangeAndFetchUser(ctx context.Context, code, codeVerifier string) (ExternalIdentity, error) {
	if code == "" || codeVerifier == "" {
		return ExternalIdentity{}, fmt.Errorf("%w: code and code_verifier are required", ErrAuthFailed)
	}

	callCtx := context.WithValue(ctx, oauth2.HTTPClient, p.httpClient)
	token, err := p.oauth.Exchange(callCtx, code, oauth2.VerifierOption(codeVerifier))
	if err != nil {
		return ExternalIdentity{}, classifyGitHubExchangeError(ctx, err)
	}
	if token.AccessToken == "" {
		return ExternalIdentity{}, fmt.Errorf("%w: empty access token", ErrProviderUnavailable)
	}

	var user githubUserResponse
	if err := p.getJSON(ctx, token.AccessToken, p.apiBaseURL+"/user", &user); err != nil {
		return ExternalIdentity{}, err
	}
	if user.ID <= 0 {
		return ExternalIdentity{}, fmt.Errorf("%w: invalid github user id %d", ErrAuthFailed, user.ID)
	}

	/*
		必须单独查询 /user/emails：/user 返回的 email 对隐藏私密邮箱的用户为空，且不包含 verified 标记；
		邀请兑现只能认已经 verified 的 primary 邮箱，不能用未核实邮箱冒领组织成员资格。
	*/
	var emails []githubEmailResponse
	if err := p.getJSON(ctx, token.AccessToken, p.apiBaseURL+"/user/emails", &emails); err != nil {
		return ExternalIdentity{}, err
	}

	email, verified := pickGitHubEmail(emails)
	displayName := strings.TrimSpace(user.Name)
	if displayName == "" {
		displayName = strings.TrimSpace(user.Login)
	}

	id, err := (ExternalIdentity{
		Issuer:        GitHubIssuer,
		Subject:       strconv.FormatInt(user.ID, 10),
		Email:         email,
		EmailVerified: verified,
		Name:          displayName,
	}).Normalize()
	if err != nil {
		return ExternalIdentity{}, fmt.Errorf("%w: %w", ErrAuthFailed, err)
	}
	return id, nil
}

func pickGitHubEmail(emails []githubEmailResponse) (string, bool) {
	for _, item := range emails {
		if !item.Primary {
			continue
		}
		email := strings.TrimSpace(item.Email)
		if email == "" {
			return "", false
		}
		return email, item.Verified
	}
	return "", false
}

func (p *GitHubProvider) getJSON(ctx context.Context, accessToken, endpoint string, dst any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return fmt.Errorf("github api request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", githubAPIVersion)
	req.Header.Set("User-Agent", githubUserAgent)

	resp, err := p.httpClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("github api request: %w", ctx.Err())
		}
		return fmt.Errorf("%w: %w", ErrProviderUnavailable, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
	}()

	if resp.StatusCode == http.StatusUnauthorized {
		return fmt.Errorf("%w: github api status %d", ErrAuthFailed, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: github api status %d", ErrProviderUnavailable, resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxGitHubResponseBytes+1))
	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("read github response: %w", ctx.Err())
		}
		return fmt.Errorf("%w: read github response: %w", ErrProviderUnavailable, err)
	}
	if len(body) > maxGitHubResponseBytes {
		return fmt.Errorf("%w: github response exceeds %d bytes", ErrProviderUnavailable, maxGitHubResponseBytes)
	}
	if err := json.Unmarshal(body, dst); err != nil {
		return fmt.Errorf("%w: decode github response: %w", ErrProviderUnavailable, err)
	}
	return nil
}

/*
classifyGitHubExchangeError 按 GitHub /login/oauth/access_token 的 quirks 分类失败原因：
GitHub 在授权码错、客户凭据错时经常仍返回 HTTP 200（表单或 JSON 体带 error=...），
这和标准 OAuth/OIDC 的 HTTP 400 不同；x/oauth2 遇到 ErrorCode 非空时会返回 *oauth2.RetrieveError。
bad_verification_code / access_denied 等用户/一次性码错误归为 ErrAuthFailed（401），
incorrect_client_credentials / redirect_uri_mismatch / 5xx / 网络故障归为 ErrProviderUnavailable（502）。
*/
func classifyGitHubExchangeError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return fmt.Errorf("exchange github code: %w", ctx.Err())
	}
	if retrieveErr, ok := errors.AsType[*oauth2.RetrieveError](err); ok && retrieveErr.Response != nil &&
		(retrieveErr.Response.StatusCode == http.StatusOK || retrieveErr.Response.StatusCode == http.StatusBadRequest) {
		switch retrieveErr.ErrorCode {
		case "bad_verification_code", "invalid_grant", "access_denied", "expired_token", "unverified_user_email", "invalid_request":
			return fmt.Errorf("%w: %w", ErrAuthFailed, err)
		default:
			// unsupported_grant_type、incorrect_client_credentials、redirect_uri_mismatch、
			// application_suspended、server_error、temporarily_unavailable 等配置或服务端异常统一归为提供方不可用。
			return fmt.Errorf("%w: %w", ErrProviderUnavailable, err)
		}
	}
	return fmt.Errorf("%w: %w", ErrProviderUnavailable, err)
}
