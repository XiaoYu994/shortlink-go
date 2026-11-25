package mailer

import (
	"fmt"
	"strings"
	"time"
)

// InvitationEmailTarget 描述后台发送成员邀请邮件时从数据库回表读取的实时邀请信息。
type InvitationEmailTarget struct {
	InvitationID int64
	TenantID     int64
	TenantName   string
	Email        string
	Role         string
	InviterName  string
	InviterEmail string
	CreatedAt    time.Time
	ExpiresAt    *time.Time
	AcceptedAt   *time.Time
	RevokedAt    *time.Time
	SupersededAt *time.Time
}

// Pending 报告该邀请在 now 时刻是否仍处于待兑现有效状态。
func (t InvitationEmailTarget) Pending(now time.Time) bool {
	if t.InvitationID <= 0 || t.Email == "" {
		return false
	}
	if t.AcceptedAt != nil || t.RevokedAt != nil || t.SupersededAt != nil {
		return false
	}
	if t.ExpiresAt != nil && !t.ExpiresAt.After(now) {
		return false
	}
	return true
}

// BuildInvitationMessage 构造成员邀请通知纯文本邮件，根据 consolePublicURL 是否配置生成登录入口或降级提示。
func BuildInvitationMessage(target InvitationEmailTarget, consolePublicURL string) Message {
	orgName := strings.TrimSpace(target.TenantName)
	if orgName == "" {
		orgName = fmt.Sprintf("组织 #%d", target.TenantID)
	}

	inviter := strings.TrimSpace(target.InviterName)
	switch {
	case inviter != "" && strings.TrimSpace(target.InviterEmail) != "":
		inviter = fmt.Sprintf("%s (%s)", inviter, strings.TrimSpace(target.InviterEmail))
	case inviter == "" && strings.TrimSpace(target.InviterEmail) != "":
		inviter = strings.TrimSpace(target.InviterEmail)
	case inviter == "":
		inviter = "组织管理员"
	}

	var b strings.Builder
	fmt.Fprintf(&b, "你好，\n\n")
	fmt.Fprintf(&b, "%s 邀请你加入 Shortlink 组织「%s」，分配角色为 %s。\n\n", inviter, orgName, target.Role)
	fmt.Fprintf(&b, "受邀邮箱：%s\n", target.Email)
	if target.ExpiresAt != nil {
		fmt.Fprintf(&b, "邀请有效期至：%s\n", target.ExpiresAt.UTC().Format(time.RFC3339))
	} else {
		fmt.Fprintf(&b, "邀请有效期：长期有效（兑现前可随时撤销）\n")
	}

	baseURL := strings.TrimRight(strings.TrimSpace(consolePublicURL), "/")
	if baseURL != "" {
		fmt.Fprintf(&b, "\n请使用邮箱 %s 登录控制台即可自动加入组织：\n%s/login\n", target.Email, baseURL)
	} else {
		fmt.Fprintf(&b, "\n请向管理员获取 Shortlink 控制台地址，并使用邮箱 %s 登录，系统将自动兑现该邀请。\n", target.Email)
	}

	return Message{
		To:      target.Email,
		Subject: fmt.Sprintf("Shortlink 邀请：加入组织「%s」", orgName),
		Body:    b.String(),
	}
}
