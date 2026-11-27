package console

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"shortlink/internal/httpx"
	"shortlink/internal/identity"
	"shortlink/internal/sms"
)

const otpRollbackTimeout = 2 * time.Second

/*
PhoneOTPStore 封装手机验证码登录插件所需的挑战持久化和原子核验能力（ADR 0030）。
接口定义在使用方并控制在 3 个方法以内，生产由 postgres.Store 实现。
*/
type PhoneOTPStore interface {
	CreatePhoneOTPChallenge(ctx context.Context, p identity.PhoneOTPChallengeParams) (int64, error)
	DeleteUnsentPhoneOTPChallenge(ctx context.Context, id int64) error
	VerifyPhoneOTPChallenge(ctx context.Context, phone, code string) error
}

// PhoneOTPOptions 控制手机号验证码登录插件的存储、短信通道、有效期与防刷参数。
type PhoneOTPOptions struct {
	Store         PhoneOTPStore
	Sender        sms.Sender
	TTL           time.Duration
	Cooldown      time.Duration
	MaxPerHour    int
	MaxAttempts   int
	SecureCookies bool
}

/*
PhoneOTPAuthMethod 是基于短信一次性验证码的非跳转型登录插件（ADR 0029、ADR 0030）。
它直接实现 AuthMethod 接口，暴露 POST /api/v1/auth/{id}/send-code 与 POST /api/v1/auth/{id}/verify，
复用认证底座的 IP 限流、CrossOriginProtection 与 SessionIssuer，核心会话层零改动。
*/
type PhoneOTPAuthMethod struct {
	id             string
	sendCodePath   string
	verifyCodePath string
	store          PhoneOTPStore
	sender         sms.Sender
	ttl            time.Duration
	cooldown       time.Duration
	maxPerHour     int
	maxAttempts    int
	secureCookies  bool
}

// NewPhoneOTPAuthMethod 校验参数并构造手机号验证码登录插件。
func NewPhoneOTPAuthMethod(id string, opts PhoneOTPOptions) (*PhoneOTPAuthMethod, error) {
	if !validAuthMethodID(id) {
		return nil, fmt.Errorf("phone otp auth method: invalid id %q", id)
	}
	if opts.Store == nil {
		return nil, fmt.Errorf("phone otp auth method %q: store is nil", id)
	}
	if opts.Sender == nil {
		return nil, fmt.Errorf("phone otp auth method %q: sms sender is nil", id)
	}
	ttl := opts.TTL
	if ttl <= 0 {
		ttl = identity.DefaultOTPTTL
	}
	cooldown := opts.Cooldown
	if cooldown <= 0 {
		cooldown = identity.DefaultOTPCooldown
	}
	maxPerHour := opts.MaxPerHour
	if maxPerHour <= 0 {
		maxPerHour = identity.DefaultOTPMaxPerHour
	}
	maxAttempts := opts.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = identity.DefaultOTPMaxAttempts
	}
	base := apiV1Prefix + "/auth/" + id
	return &PhoneOTPAuthMethod{
		id:             id,
		sendCodePath:   base + "/send-code",
		verifyCodePath: base + "/verify",
		store:          opts.Store,
		sender:         opts.Sender,
		ttl:            ttl,
		cooldown:       cooldown,
		maxPerHour:     maxPerHour,
		maxAttempts:    maxAttempts,
		secureCookies:  opts.SecureCookies,
	}, nil
}

// Descriptor 返回下发给前端的能力描述，附带发码与核验接口路径。
func (m *PhoneOTPAuthMethod) Descriptor() AuthMethodDescriptor {
	return AuthMethodDescriptor{
		ID:            m.id,
		Kind:          AuthFlowPhoneOTP,
		SendCodeURL:   m.sendCodePath,
		VerifyCodeURL: m.verifyCodePath,
	}
}

// SecureCookies 返回该登录方式签发会话 Cookie 时使用的 Secure 标记。
func (m *PhoneOTPAuthMethod) SecureCookies() bool {
	return m.secureCookies
}

// RegisterRoutes 挂载发码与核验两个 POST 路由，并统一接入底座 IP 限流中间件。
func (m *PhoneOTPAuthMethod) RegisterRoutes(tk AuthToolkit) {
	tk.Mux.Handle("POST "+m.sendCodePath, tk.IPRateLimit(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.handleSendCode(w, r, tk.Logger, tk.CSRF)
	})))
	tk.Mux.Handle("POST "+m.verifyCodePath, tk.IPRateLimit(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.handleVerify(w, r, tk.Logger, tk.CSRF, tk.Issuer)
	})))
}

type sendPhoneOTPRequest struct {
	Phone string `json:"phone"`
}

type sendPhoneOTPResponse struct {
	ExpiresIn       int `json:"expires_in"`
	CooldownSeconds int `json:"cooldown_seconds"`
}

type verifyPhoneOTPRequest struct {
	Phone string `json:"phone"`
	Code  string `json:"code"`
}

func (m *PhoneOTPAuthMethod) handleSendCode(w http.ResponseWriter, r *http.Request, logger *slog.Logger, csrf *http.CrossOriginProtection) {
	if httpx.IsClientGone(logger, r) {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if !checkCrossOrigin(logger, csrf, w, r) {
		return
	}

	body, ok := readSmallRequestBody(logger, w, r)
	if !ok {
		return
	}
	var req sendPhoneOTPRequest
	if err := decodeStrict(body, &req); err != nil {
		httpx.WriteError(logger, r.Context(), w, http.StatusBadRequest, phoneOTPDecodeMessage(err))
		return
	}

	phone, err := identity.NormalizePhone(req.Phone)
	if err != nil {
		httpx.WriteError(logger, r.Context(), w, http.StatusBadRequest, identity.ErrInvalidPhone.Error())
		return
	}

	code := identity.NewOTPCode()
	salt := identity.NewOTPSalt()

	challengeID, err := m.store.CreatePhoneOTPChallenge(r.Context(), identity.PhoneOTPChallengeParams{
		Phone:       phone,
		CodeSalt:    salt,
		CodeHash:    identity.HashOTPCode(salt, phone, code),
		TTL:         m.ttl,
		Cooldown:    m.cooldown,
		MaxPerHour:  m.maxPerHour,
		MaxAttempts: m.maxAttempts,
	})
	if err != nil {
		if errors.Is(err, identity.ErrOTPCooldown) || errors.Is(err, identity.ErrOTPRateLimited) {
			if retryAfter, ok := identity.OTPRetryAfter(err); ok {
				w.Header().Set("Retry-After", strconv.Itoa(retryAfterSeconds(retryAfter)))
			}
			httpx.WriteError(logger, r.Context(), w, http.StatusTooManyRequests, err.Error())
			return
		}
		httpx.HandleStoreError(logger, w, r, "create phone otp challenge", err)
		return
	}

	if err := m.sender.SendOTP(r.Context(), sms.Message{
		To:   phone,
		Code: code,
		TTL:  m.ttl,
	}); err != nil {
		// 短信未成功送达时回滚刚写入的挑战，避免白白占用用户冷却期与小时额度。
		rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), otpRollbackTimeout)
		if delErr := m.store.DeleteUnsentPhoneOTPChallenge(rollbackCtx, challengeID); delErr != nil {
			logger.ErrorContext(r.Context(), "rollback unsent phone otp challenge", "phone", identity.MaskPhone(phone), "err", delErr)
		}
		cancel()

		if httpx.IsClientGone(logger, r) {
			return
		}
		if errors.Is(err, context.DeadlineExceeded) {
			logger.WarnContext(r.Context(), "send phone otp timed out", "phone", identity.MaskPhone(phone), "err", err)
			httpx.WriteError(logger, r.Context(), w, http.StatusGatewayTimeout, "request timed out")
			return
		}
		logger.ErrorContext(r.Context(), "send phone otp", "phone", identity.MaskPhone(phone), "err", err)
		httpx.WriteError(logger, r.Context(), w, http.StatusBadGateway, "sms provider unavailable")
		return
	}

	httpx.WriteJSON(logger, r.Context(), w, http.StatusOK, sendPhoneOTPResponse{
		ExpiresIn:       max(int(m.ttl/time.Second), 1),
		CooldownSeconds: max(int(m.cooldown/time.Second), 1),
	})
}

func (m *PhoneOTPAuthMethod) handleVerify(w http.ResponseWriter, r *http.Request, logger *slog.Logger, csrf *http.CrossOriginProtection, issuer SessionIssuer) {
	if httpx.IsClientGone(logger, r) {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if !checkCrossOrigin(logger, csrf, w, r) {
		return
	}

	body, ok := readSmallRequestBody(logger, w, r)
	if !ok {
		return
	}
	var req verifyPhoneOTPRequest
	if err := decodeStrict(body, &req); err != nil {
		httpx.WriteError(logger, r.Context(), w, http.StatusBadRequest, phoneOTPDecodeMessage(err))
		return
	}

	phone, err := identity.NormalizePhone(req.Phone)
	if err != nil {
		httpx.WriteError(logger, r.Context(), w, http.StatusBadRequest, identity.ErrInvalidPhone.Error())
		return
	}
	if !identity.ValidOTPCode(req.Code) {
		httpx.WriteError(logger, r.Context(), w, http.StatusBadRequest, identity.ErrOTPInvalid.Error())
		return
	}

	if err := m.store.VerifyPhoneOTPChallenge(r.Context(), phone, req.Code); err != nil {
		if errors.Is(err, identity.ErrOTPInvalid) {
			httpx.WriteError(logger, r.Context(), w, http.StatusUnauthorized, identity.ErrOTPInvalid.Error())
			return
		}
		httpx.HandleStoreError(logger, w, r, "verify phone otp challenge", err)
		return
	}

	issuer.IssueSession(w, r, identity.ExternalIdentity{
		Issuer:        identity.PhoneIssuer,
		Subject:       phone,
		Email:         "",
		EmailVerified: false,
		Name:          identity.MaskPhone(phone),
	}, IssueSessionOptions{
		SecureCookie: m.secureCookies,
	})
}

func phoneOTPDecodeMessage(err error) string {
	switch {
	case isJSONFieldError(err, "phone"):
		return identity.ErrInvalidPhone.Error()
	case isJSONFieldError(err, "code"):
		return identity.ErrOTPInvalid.Error()
	default:
		return decodeErrorMessage(err)
	}
}

func retryAfterSeconds(d time.Duration) int {
	if d <= 0 {
		return 1
	}
	return max(int((d+time.Second-1)/time.Second), 1)
}
