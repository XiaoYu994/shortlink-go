package mailer

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"net"
	"net/smtp"
	"net/textproto"
	"strconv"
	"strings"
	"time"
)

const (
	DefaultSMTPPort    = 587
	DefaultSMTPTimeout = 10 * time.Second
)

// SMTPConfig 控制 SMTP 出站连接、TLS 模式、可选认证与单次超时。
type SMTPConfig struct {
	Host     string
	Port     int
	Username string
	Password string
	From     string
	TLSMode  string
	Timeout  time.Duration
}

// Enabled 报告是否启用了 SMTP 发信。
func (c SMTPConfig) Enabled() bool {
	return strings.TrimSpace(c.Host) != ""
}

func redactSecret(s string) string {
	if s == "" {
		return ""
	}
	return "[REDACTED]"
}

// LogValue 掩码 SMTP_PASSWORD，避免日志泄露发信凭据。
func (c SMTPConfig) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("host", c.Host),
		slog.Int("port", c.Port),
		slog.String("username", c.Username),
		slog.String("password", redactSecret(c.Password)),
		slog.String("from", c.From),
		slog.String("tls_mode", c.TLSMode),
		slog.Duration("timeout", c.Timeout),
	)
}

// String 实现 fmt.Stringer，防止 %+v/%v 意外打印出密码。
func (c SMTPConfig) String() string {
	return c.LogValue().String()
}

// ValidTLSMode 报告 mode 是否为受支持的 TLS 模式。
func ValidTLSMode(mode string) bool {
	switch mode {
	case TLSModeStartTLS, TLSModeSMTPS, TLSModeNone:
		return true
	default:
		return false
	}
}

// SMTPSender 基于 Go 标准库 net/smtp 实现支持 context 取消与截止时间的邮件发送器。
type SMTPSender struct {
	cfg        SMTPConfig
	fromHeader string
	fromAddr   string
	tlsConfig  *tls.Config
}

// NewSMTPSender 校验 SMTPConfig 并构造 SMTPSender；可选传入自定义 *tls.Config 方便测试或内网 CA。
func NewSMTPSender(cfg SMTPConfig, tlsConfig ...*tls.Config) (*SMTPSender, error) {
	cfg.Host = strings.TrimSpace(cfg.Host)
	if cfg.Host == "" {
		return nil, fmt.Errorf("%w: host is required", ErrInvalidConfig)
	}
	if cfg.Port <= 0 {
		cfg.Port = DefaultSMTPPort
	}
	if cfg.Port < 1 || cfg.Port > 65535 {
		return nil, fmt.Errorf("%w: invalid port %d", ErrInvalidConfig, cfg.Port)
	}
	if cfg.TLSMode == "" {
		cfg.TLSMode = TLSModeStartTLS
	}
	if !ValidTLSMode(cfg.TLSMode) {
		return nil, fmt.Errorf("%w: invalid tls mode %q", ErrInvalidConfig, cfg.TLSMode)
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultSMTPTimeout
	}
	if (cfg.Username == "") != (cfg.Password == "") {
		return nil, fmt.Errorf("%w: username and password must both be set or both be empty", ErrInvalidConfig)
	}

	fromHeader, fromAddr, err := parseEnvelopeAddr(cfg.From, "from")
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidConfig, err)
	}

	var customTLS *tls.Config
	if len(tlsConfig) > 0 && tlsConfig[0] != nil {
		customTLS = tlsConfig[0].Clone()
	} else {
		customTLS = &tls.Config{
			ServerName: cfg.Host,
			MinVersion: tls.VersionTLS12,
		}
	}
	if customTLS.ServerName == "" {
		customTLS.ServerName = cfg.Host
	}

	return &SMTPSender{
		cfg:        cfg,
		fromHeader: fromHeader,
		fromAddr:   fromAddr,
		tlsConfig:  customTLS,
	}, nil
}

/*
Send 通过 SMTP 发送一封纯文本 UTF-8 邮件，并把 ctx 截止时间/取消同步映射到底层 net.Conn Deadline，
补齐标准库 smtp.SendMail 不接受 context.Context 的缺口。SMTP 5xx 响应标记为永久错误。
*/
func (s *SMTPSender) Send(ctx context.Context, msg Message) error {
	if s == nil {
		return MarkPermanent(fmt.Errorf("%w: nil smtp sender", ErrInvalidConfig))
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	toHeader, toAddr, err := parseEnvelopeAddr(msg.To, "to")
	if err != nil {
		return err
	}
	subject := strings.TrimSpace(msg.Subject)
	if subject == "" {
		return MarkPermanent(fmt.Errorf("%w: subject is required", ErrInvalidMessage))
	}
	if containsHeaderControl(subject) {
		return MarkPermanent(fmt.Errorf("%w: subject", ErrHeaderInjection))
	}
	if strings.ContainsRune(msg.Body, '\x00') {
		return MarkPermanent(fmt.Errorf("%w: body contains null byte", ErrInvalidMessage))
	}

	deadline := time.Now().Add(s.cfg.Timeout)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}
	dialCtx, cancelDial := context.WithDeadline(ctx, deadline)
	defer cancelDial()

	addr := net.JoinHostPort(s.cfg.Host, strconv.Itoa(s.cfg.Port))
	conn, err := s.dial(dialCtx, addr)
	if err != nil {
		return fmt.Errorf("smtp dial %s: %w", addr, err)
	}

	// 将 ctx 取消映射到 conn deadline；Send 返回时 close(done) 确保 watcher goroutine 立即退出。
	_ = conn.SetDeadline(deadline)
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.SetDeadline(time.Now())
		case <-done:
		}
	}()

	client, err := smtp.NewClient(conn, s.cfg.Host)
	if err != nil {
		_ = conn.Close()
		return classifySMTPError("smtp greeting", err)
	}
	defer func() { _ = client.Close() }()

	if s.cfg.TLSMode == TLSModeStartTLS {
		if ok, _ := client.Extension("STARTTLS"); !ok {
			return fmt.Errorf("smtp starttls: server does not advertise STARTTLS")
		}
		if err := client.StartTLS(s.tlsConfig.Clone()); err != nil {
			return classifySMTPError("smtp starttls", err)
		}
	}

	if s.cfg.Username != "" {
		auth := smtp.PlainAuth("", s.cfg.Username, s.cfg.Password, s.cfg.Host)
		if err := client.Auth(auth); err != nil {
			return classifySMTPError("smtp auth", err)
		}
	}

	if err := client.Mail(s.fromAddr); err != nil {
		return classifySMTPError("smtp MAIL FROM", err)
	}
	if err := client.Rcpt(toAddr); err != nil {
		return classifySMTPError("smtp RCPT TO", err)
	}
	w, err := client.Data()
	if err != nil {
		return classifySMTPError("smtp DATA", err)
	}

	payload := buildRFC5322PlainText(s.fromHeader, toHeader, subject, msg.Body, time.Now().UTC())
	if _, err := w.Write(payload); err != nil {
		_ = w.Close()
		return classifySMTPError("smtp write body", err)
	}
	if err := w.Close(); err != nil {
		return classifySMTPError("smtp commit body", err)
	}
	// DATA 已经拿到 250 确认，邮件已入对端投递队列；QUIT 只是收尾礼貌挥手，
	// 对端先断开或 deadline 刚好到点不应报错导致任务重试重复发信。
	_ = client.Quit()
	return nil
}

func (s *SMTPSender) dial(ctx context.Context, addr string) (net.Conn, error) {
	netDialer := &net.Dialer{Timeout: s.cfg.Timeout}
	if s.cfg.TLSMode == TLSModeSMTPS {
		dialer := &tls.Dialer{
			NetDialer: netDialer,
			Config:    s.tlsConfig.Clone(),
		}
		return dialer.DialContext(ctx, "tcp", addr)
	}
	return netDialer.DialContext(ctx, "tcp", addr)
}

func buildRFC5322PlainText(fromHeader, toHeader, subject, body string, now time.Time) []byte {
	encodedSubject := mime.QEncoding.Encode("utf-8", subject)
	normalizedBody := strings.ReplaceAll(strings.ReplaceAll(body, "\r\n", "\n"), "\n", "\r\n")
	if !strings.HasSuffix(normalizedBody, "\r\n") {
		normalizedBody += "\r\n"
	}

	var b strings.Builder
	b.WriteString("From: ")
	b.WriteString(fromHeader)
	b.WriteString("\r\n")
	b.WriteString("To: ")
	b.WriteString(toHeader)
	b.WriteString("\r\n")
	b.WriteString("Subject: ")
	b.WriteString(encodedSubject)
	b.WriteString("\r\n")
	b.WriteString("Date: ")
	b.WriteString(now.Format(time.RFC1123Z))
	b.WriteString("\r\n")
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=UTF-8\r\n")
	b.WriteString("Content-Transfer-Encoding: 8bit\r\n")
	b.WriteString("\r\n")
	b.WriteString(normalizedBody)
	return []byte(b.String())
}

func classifySMTPError(op string, err error) error {
	wrapped := fmt.Errorf("%s: %w", op, err)
	if tpErr, ok := errors.AsType[*textproto.Error](err); ok {
		if tpErr.Code >= 500 && tpErr.Code < 600 {
			return MarkPermanent(wrapped)
		}
	}
	return wrapped
}
