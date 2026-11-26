package main

import (
	"errors"
	"fmt"
	"log/slog"

	"shortlink/internal/job"
	"shortlink/internal/mailer"
)

/*
emailWorkerModule 自包含 SMTP 发送器构造与 invitation.email 任务注册（ADR 0030）。
SMTP_HOST 为空时仅注册 invitation.email 的 disabledSkipHandler。
*/
type emailWorkerModule struct {
	smtp             mailer.SMTPConfig
	store            invitationEmailStore
	consolePublicURL string
	logger           *slog.Logger
}

func (m emailWorkerModule) Name() string { return "email" }

func (m emailWorkerModule) RegisterWorker(r WorkerRegistrar) error {
	if !m.smtp.Enabled() {
		if m.logger != nil {
			m.logger.Info("invitation email sender disabled: SMTP_HOST is not set")
		}
		r.HandleDisabled(job.KindInvitationEmail, m.Name())
		return nil
	}
	if m.store == nil {
		return errors.New("invitation email store is nil")
	}
	sender, err := mailer.NewSMTPSender(m.smtp)
	if err != nil {
		return fmt.Errorf("init smtp sender: %w", err)
	}
	if m.logger != nil {
		m.logger.Info("invitation email sender enabled", "host", m.smtp.Host, "port", m.smtp.Port, "tls_mode", m.smtp.TLSMode)
	}
	r.Handle(job.KindInvitationEmail, newInvitationEmailHandler(m.store, sender, m.consolePublicURL, m.logger))
	return nil
}
