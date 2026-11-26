package console

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"shortlink/internal/identity"
	"shortlink/internal/sms"
)

type stubPhoneOTPStore struct {
	createCalls int
	deleteCalls int
	verifyCalls int
	deletedIDs  []int64

	nextID     int64
	activeSalt []byte
	activeHash []byte
	activeTo   string

	createErr error
	verifyErr error
}

func (s *stubPhoneOTPStore) CreatePhoneOTPChallenge(_ context.Context, p identity.PhoneOTPChallengeParams) (int64, error) {
	s.createCalls++
	if s.createErr != nil {
		return 0, s.createErr
	}
	s.nextID++
	s.activeTo = p.Phone
	s.activeSalt = p.CodeSalt
	s.activeHash = p.CodeHash
	return s.nextID, nil
}

func (s *stubPhoneOTPStore) DeleteUnsentPhoneOTPChallenge(_ context.Context, id int64) error {
	s.deleteCalls++
	s.deletedIDs = append(s.deletedIDs, id)
	if id == s.nextID {
		s.activeSalt = nil
		s.activeHash = nil
	}
	return nil
}

func (s *stubPhoneOTPStore) VerifyPhoneOTPChallenge(_ context.Context, phone, code string) error {
	s.verifyCalls++
	if s.verifyErr != nil {
		return s.verifyErr
	}
	if phone != s.activeTo || !identity.VerifyOTPCodeHash(s.activeSalt, s.activeHash, phone, code) {
		return identity.ErrOTPInvalid
	}
	s.activeSalt = nil
	s.activeHash = nil
	return nil
}

type stubSMSSender struct {
	calls int
	sent  []sms.Message
	err   error
}

func (s *stubSMSSender) SendOTP(ctx context.Context, msg sms.Message) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.calls++
	if s.err != nil {
		return s.err
	}
	s.sent = append(s.sent, msg)
	return nil
}

func TestPhoneOTPAuthMethodCapabilitiesAndFullFlow(t *testing.T) {
	otpStore := &stubPhoneOTPStore{}
	sender := &stubSMSSender{}
	sessions := &stubSessionStore{}

	method, err := NewPhoneOTPAuthMethod("phone", PhoneOTPOptions{
		Store:         otpStore,
		Sender:        sender,
		TTL:           5 * time.Minute,
		Cooldown:      60 * time.Second,
		SecureCookies: false,
	})
	if err != nil {
		t.Fatalf("NewPhoneOTPAuthMethod: %v", err)
	}

	srv := New(selfServiceLogger(), &stubStore{}, 2*time.Second, prometheus.NewRegistry(), Options{
		Authorizer:  identity.NewMapAuthorizer(),
		Sessions:    sessions,
		AuthMethods: []AuthMethod{method},
	})
	h := srv.Routes()

	// 1. /api/v1/capabilities 应动态暴露 phone_otp 描述与路径。
	capRec := httptest.NewRecorder()
	h.ServeHTTP(capRec, httptest.NewRequest(http.MethodGet, "/api/v1/capabilities", nil))
	if capRec.Code != http.StatusOK {
		t.Fatalf("GET /capabilities = %d, want 200", capRec.Code)
	}
	var capBody capabilitiesResponse
	if err := json.Unmarshal(capRec.Body.Bytes(), &capBody); err != nil {
		t.Fatalf("decode capabilities: %v", err)
	}
	if len(capBody.AuthMethods) != 1 ||
		capBody.AuthMethods[0].ID != "phone" ||
		capBody.AuthMethods[0].Kind != AuthFlowPhoneOTP ||
		capBody.AuthMethods[0].SendCodeURL != "/api/v1/auth/phone/send-code" ||
		capBody.AuthMethods[0].VerifyCodeURL != "/api/v1/auth/phone/verify" {
		t.Fatalf("capabilities auth_methods = %+v", capBody.AuthMethods)
	}

	// 2. 发送验证码成功，返回过期时间与冷却秒数，不泄露验证码。
	sendReq := httptest.NewRequest(http.MethodPost, "/api/v1/auth/phone/send-code", strings.NewReader(`{"phone":"+8613800138000"}`))
	sendRec := httptest.NewRecorder()
	h.ServeHTTP(sendRec, sendReq)
	if sendRec.Code != http.StatusOK {
		t.Fatalf("POST send-code = %d %s, want 200", sendRec.Code, sendRec.Body.String())
	}
	if sendRec.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("send-code Cache-Control = %q, want no-store", sendRec.Header().Get("Cache-Control"))
	}
	if len(sender.sent) != 1 {
		t.Fatalf("sent SMS count = %d, want 1", len(sender.sent))
	}
	code := sender.sent[0].Code
	if strings.Contains(sendRec.Body.String(), code) {
		t.Fatalf("send-code response leaked OTP code: %s", sendRec.Body.String())
	}

	// 3. 错码核验返回 401，不签发会话与 Cookie。
	wrongCode := "000000"
	if code == wrongCode {
		wrongCode = "999999"
	}
	wrongRec := httptest.NewRecorder()
	h.ServeHTTP(wrongRec, httptest.NewRequest(http.MethodPost, "/api/v1/auth/phone/verify",
		strings.NewReader(`{"phone":"+8613800138000","code":"`+wrongCode+`"}`)))
	if wrongRec.Code != http.StatusUnauthorized {
		t.Fatalf("verify wrong code = %d %s, want 401", wrongRec.Code, wrongRec.Body.String())
	}
	if sessions.calls != 0 || findCookie(wrongRec.Result().Cookies(), SessionCookieName) != nil {
		t.Fatalf("wrong OTP produced session side effects: calls=%d", sessions.calls)
	}

	// 4. 正确验证码核验通过，签发 sl_session Cookie 且 ExternalIdentity 使用 PhoneIssuer 与脱敏展示名。
	okRec := httptest.NewRecorder()
	h.ServeHTTP(okRec, httptest.NewRequest(http.MethodPost, "/api/v1/auth/phone/verify",
		strings.NewReader(`{"phone":"+8613800138000","code":"`+code+`"}`)))
	if okRec.Code != http.StatusOK {
		t.Fatalf("verify valid code = %d %s, want 200", okRec.Code, okRec.Body.String())
	}
	if findCookie(okRec.Result().Cookies(), SessionCookieName) == nil {
		t.Fatal("verify valid code missing sl_session cookie")
	}
	if sessions.calls != 1 {
		t.Fatalf("sessions.calls = %d, want 1", sessions.calls)
	}
	if sessions.lastID.Issuer != identity.PhoneIssuer ||
		sessions.lastID.Subject != "+8613800138000" ||
		sessions.lastID.Email != "" ||
		sessions.lastID.EmailVerified ||
		sessions.lastID.Name != "+86****8000" {
		t.Fatalf("issued ExternalIdentity = %+v", sessions.lastID)
	}

	// 5. 同一验证码再次重放必须返回 401 且不二次建会话。
	replayRec := httptest.NewRecorder()
	h.ServeHTTP(replayRec, httptest.NewRequest(http.MethodPost, "/api/v1/auth/phone/verify",
		strings.NewReader(`{"phone":"+8613800138000","code":"`+code+`"}`)))
	if replayRec.Code != http.StatusUnauthorized || sessions.calls != 1 {
		t.Fatalf("replayed OTP = %d (sessions=%d), want 401 (sessions=1)", replayRec.Code, sessions.calls)
	}
}

func TestPhoneOTPAuthMethodBoundariesAndNegativeSideEffects(t *testing.T) {
	otpStore := &stubPhoneOTPStore{}
	sender := &stubSMSSender{}
	sessions := &stubSessionStore{}

	method, err := NewPhoneOTPAuthMethod("phone", PhoneOTPOptions{
		Store:  otpStore,
		Sender: sender,
	})
	if err != nil {
		t.Fatalf("NewPhoneOTPAuthMethod: %v", err)
	}
	srv := New(selfServiceLogger(), &stubStore{}, 2*time.Second, prometheus.NewRegistry(), Options{
		Authorizer:  identity.NewMapAuthorizer(),
		Sessions:    sessions,
		AuthMethods: []AuthMethod{method},
	})
	h := srv.Routes()

	// 1. 跨站请求被 CrossOriginProtection 拦截，不写挑战、不发短信。
	csrfReq := httptest.NewRequest(http.MethodPost, "/api/v1/auth/phone/send-code", strings.NewReader(`{"phone":"+8613800138000"}`))
	csrfReq.Header.Set("Sec-Fetch-Site", "cross-site")
	csrfRec := httptest.NewRecorder()
	h.ServeHTTP(csrfRec, csrfReq)
	if csrfRec.Code != http.StatusForbidden {
		t.Fatalf("cross-site send-code = %d, want 403", csrfRec.Code)
	}
	if otpStore.createCalls != 0 || sender.calls != 0 {
		t.Fatalf("cross-site request caused side effects: create=%d sms=%d", otpStore.createCalls, sender.calls)
	}

	// 2. 非法手机号与畸形验证码在入库前直接返回 400。
	badPhoneRec := httptest.NewRecorder()
	h.ServeHTTP(badPhoneRec, httptest.NewRequest(http.MethodPost, "/api/v1/auth/phone/send-code", strings.NewReader(`{"phone":"13800138000"}`)))
	if badPhoneRec.Code != http.StatusBadRequest || otpStore.createCalls != 0 || sender.calls != 0 {
		t.Fatalf("bad phone = %d (create=%d sms=%d), want 400 (0/0)", badPhoneRec.Code, otpStore.createCalls, sender.calls)
	}

	badCodeRec := httptest.NewRecorder()
	h.ServeHTTP(badCodeRec, httptest.NewRequest(http.MethodPost, "/api/v1/auth/phone/verify", strings.NewReader(`{"phone":"+8613800138000","code":"12ab"}`)))
	if badCodeRec.Code != http.StatusBadRequest || otpStore.verifyCalls != 0 {
		t.Fatalf("bad code format = %d (verify=%d), want 400 (0)", badCodeRec.Code, otpStore.verifyCalls)
	}

	// 3. 冷却期返回 429 并带 Retry-After，且不调用短信通道。
	otpStore.createErr = identity.OTPRetryAfterError{Err: identity.ErrOTPCooldown, RetryAfter: 45 * time.Second}
	cdRec := httptest.NewRecorder()
	h.ServeHTTP(cdRec, httptest.NewRequest(http.MethodPost, "/api/v1/auth/phone/send-code", strings.NewReader(`{"phone":"+8613800138000"}`)))
	if cdRec.Code != http.StatusTooManyRequests || cdRec.Header().Get("Retry-After") != "45" || sender.calls != 0 {
		t.Fatalf("cooldown = %d Retry-After=%q sms=%d, want 429 45 0", cdRec.Code, cdRec.Header().Get("Retry-After"), sender.calls)
	}
	otpStore.createErr = nil

	// 4. 短信网关失败时返回 502 并回滚删除刚写入的挑战，不遗留活跃挑战。
	sender.err = errors.Join(sms.ErrProviderUnavailable, errors.New("503"))
	failRec := httptest.NewRecorder()
	h.ServeHTTP(failRec, httptest.NewRequest(http.MethodPost, "/api/v1/auth/phone/send-code", strings.NewReader(`{"phone":"+8613800138000"}`)))
	if failRec.Code != http.StatusBadGateway {
		t.Fatalf("sms failure = %d %s, want 502", failRec.Code, failRec.Body.String())
	}
	if otpStore.deleteCalls != 1 || len(otpStore.deletedIDs) != 1 || otpStore.deletedIDs[0] != 1 {
		t.Fatalf("sms failure did not rollback challenge: deleteCalls=%d ids=%v", otpStore.deleteCalls, otpStore.deletedIDs)
	}
	sender.err = nil

	// 5. 请求 ctx 已取消时零 DB 调用、零 SMS 调用。
	prevCreate, prevSMS := otpStore.createCalls, sender.calls
	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	canceledReq := httptest.NewRequest(http.MethodPost, "/api/v1/auth/phone/send-code", strings.NewReader(`{"phone":"+8613800138000"}`)).WithContext(canceledCtx)
	h.ServeHTTP(httptest.NewRecorder(), canceledReq)
	if otpStore.createCalls != prevCreate || sender.calls != prevSMS {
		t.Fatalf("canceled send-code ran downstream calls: create %d->%d sms %d->%d", prevCreate, otpStore.createCalls, prevSMS, sender.calls)
	}
}
