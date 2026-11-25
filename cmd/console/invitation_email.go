package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"shortlink/internal/identity"
	"shortlink/internal/job"
	"shortlink/internal/mailer"
)

// invitationEmailStore 定义 invitation.email 任务处理器依赖的最小只读查询接口。
type invitationEmailStore interface {
	GetInvitationEmailTarget(ctx context.Context, invitationID int64) (mailer.InvitationEmailTarget, error)
}

type invitationEmailPayload struct {
	InvitationID int64 `json:"invitation_id"`
}

/*
newInvitationEmailHandler 返回消费 invitation.email 任务的 job.Handler（ADR 0027）：
1. 未配置 SMTP（sender == nil）时返回 job.Skip("smtp not configured")，不阻断无邮件环境；
2. 回表读取邀请最新状态、组织名与邀请人信息；已不存在、已兑现、已撤销、被替代或已过期一律 job.Skip；
3. 构造纯文本邀请邮件通过 SMTP 发送；5xx/配置错误转成 job.Permanent，4xx/网络超时按退避重试。
*/
func newInvitationEmailHandler(store invitationEmailStore, sender mailer.Sender, consolePublicURL string, logger *slog.Logger) job.Handler {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return func(ctx context.Context, j job.Job) error {
		if sender == nil {
			return job.Skip("smtp not configured")
		}
		var p invitationEmailPayload
		if err := json.Unmarshal(j.Payload, &p); err != nil {
			return job.Permanent(fmt.Errorf("decode invitation.email payload: %w", err))
		}
		if p.InvitationID <= 0 {
			return job.Permanent(errors.New("invitation.email payload missing invitation_id"))
		}

		target, err := store.GetInvitationEmailTarget(ctx, p.InvitationID)
		if err != nil {
			if errors.Is(err, identity.ErrInvitationNotFound) {
				return job.Skip("invitation deleted before email send")
			}
			return err
		}
		if !target.Pending(time.Now().UTC()) {
			return job.Skip("invitation is no longer pending")
		}

		msg := mailer.BuildInvitationMessage(target, consolePublicURL)
		if err := sender.Send(ctx, msg); err != nil {
			if mailer.IsPermanent(err) {
				return job.Permanent(err)
			}
			return err
		}
		logger.InfoContext(ctx, "sent invitation email",
			"invitation_id", target.InvitationID,
			"tenant_id", target.TenantID,
			"email_domain", emailDomainForLog(target.Email),
		)
		return nil
	}
}

func emailDomainForLog(email string) string {
	_, domain, ok := strings.Cut(email, "@")
	if !ok {
		return ""
	}
	return domain
}
