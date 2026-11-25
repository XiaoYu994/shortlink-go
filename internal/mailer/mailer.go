/*
Package mailer 提供成员邀请通知等管理域异步邮件发送能力（阶段 15、ADR 0027）。

本包仅依赖 Go 标准库，定义极简的 Sender 接口与支持 context 超时控制的 SMTP 实现，
不包含数据库或任务队列依赖。
*/
package mailer

import (
	"context"
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"unicode"
)

const (
	TLSModeStartTLS = "starttls"
	TLSModeSMTPS    = "smtps"
	TLSModeNone     = "none"
)

var (
	// ErrInvalidConfig 表示 SMTP 主机、端口、发件人或 TLS 模式配置无效。
	ErrInvalidConfig = errors.New("mailer: invalid smtp config")
	// ErrInvalidMessage 表示收件人、主题或正文缺少必要内容。
	ErrInvalidMessage = errors.New("mailer: invalid email message")
	// ErrHeaderInjection 表示邮件头部包含回车换行或控制字符。
	ErrHeaderInjection = errors.New("mailer: header contains disallowed control characters")
)

// Message 描述一封纯文本 UTF-8 邮件。
type Message struct {
	To      string
	Subject string
	Body    string
}

// Sender 抽象最小邮件发送能力（类比 JavaMailSender，但按使用方收敛成 1 个方法）。
type Sender interface {
	Send(ctx context.Context, msg Message) error
}

type permanentError struct {
	err error
}

func (e permanentError) Error() string { return e.err.Error() }
func (e permanentError) Unwrap() error { return e.err }

// MarkPermanent 将底层错误标为不可重试的邮件投递错误（如 SMTP 5xx 永久拒绝）。
func MarkPermanent(err error) error {
	if err == nil {
		return nil
	}
	return permanentError{err: err}
}

// IsPermanent 报告邮件发送失败是否属于不可重试错误（如配置/头部注入或 SMTP 5xx）。
func IsPermanent(err error) bool {
	_, ok := errors.AsType[permanentError](err)
	return ok
}

func containsHeaderControl(s string) bool {
	return strings.ContainsFunc(s, unicode.IsControl)
}

func parseEnvelopeAddr(raw, field string) (headerVal string, addrSpec string, err error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", "", MarkPermanent(fmt.Errorf("%w: %s is required", ErrInvalidMessage, field))
	}
	if containsHeaderControl(trimmed) {
		return "", "", MarkPermanent(fmt.Errorf("%w: %s", ErrHeaderInjection, field))
	}
	parsed, parseErr := mail.ParseAddress(trimmed)
	if parseErr != nil || parsed.Address == "" {
		return "", "", MarkPermanent(fmt.Errorf("%w: invalid %s %q", ErrInvalidMessage, field, trimmed))
	}
	if containsHeaderControl(parsed.Address) {
		return "", "", MarkPermanent(fmt.Errorf("%w: %s", ErrHeaderInjection, field))
	}
	return parsed.String(), parsed.Address, nil
}
