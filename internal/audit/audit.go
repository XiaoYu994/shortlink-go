/*
Package audit 定义组织操作审计日志的领域模型、动作枚举与 context 元数据透传约定（阶段 14、ADR 0023）。

本包仅依赖 Go 标准库，供 console、risk、cmd/console 和 postgres 共用，
在保持既有 Store 接口签名不变的前提下把执行者身份与 request_id 透传到存储层写事务里。
*/
package audit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

const maxRequestIDLen = 128

// ErrMissingMeta 表示写事务缺少有效的审计执行主体元数据（ADR 0025 fail-closed）。
var ErrMissingMeta = errors.New("audit metadata missing or invalid")

// ActorKind 区分审计事件的执行主体类型：人（会话用户）、机器密钥、运维 CLI 或后台系统任务。
type ActorKind string

const (
	ActorUser     ActorKind = "user"
	ActorAPIKey   ActorKind = "api_key"
	ActorOperator ActorKind = "operator"
	ActorSystem   ActorKind = "system"
)

// Valid 报告 k 是否属于系统定义的执行主体类型。
func (k ActorKind) Valid() bool {
	switch k {
	case ActorUser, ActorAPIKey, ActorOperator, ActorSystem:
		return true
	default:
		return false
	}
}

// Action 表示被审计的业务变更动作。
type Action string

const (
	ActionOrgCreate           Action = "org.create"
	ActionTenantLimitsUpdate  Action = "tenant.limits_update"
	ActionLinkCreate          Action = "link.create"
	ActionLinkUpdate          Action = "link.update"
	ActionLinkDelete          Action = "link.delete"
	ActionLinkBan             Action = "link.ban"
	ActionLinkUnban           Action = "link.unban"
	ActionMemberRoleUpdate    Action = "member.role_update"
	ActionMemberRemove        Action = "member.remove"
	ActionInvitationCreate    Action = "invitation.create"
	ActionInvitationRevoke    Action = "invitation.revoke"
	ActionInvitationRedeem    Action = "invitation.redeem"
	ActionAPIKeyIssue         Action = "apikey.issue"
	ActionAPIKeyRevoke        Action = "apikey.revoke"
	ActionWebhookCreate       Action = "webhook.create"
	ActionWebhookUpdate       Action = "webhook.update"
	ActionWebhookDelete       Action = "webhook.delete"
	ActionWebhookRotateSecret Action = "webhook.rotate_secret"
	ActionWebhookTest         Action = "webhook.test"
	ActionWebhookAutoDisable  Action = "webhook.auto_disable"
)

// AllActions 返回全部受支持的审计动作列表；每次返回新切片，避免调用方修改全局切片。
func AllActions() []Action {
	return []Action{
		ActionOrgCreate,
		ActionTenantLimitsUpdate,
		ActionLinkCreate,
		ActionLinkUpdate,
		ActionLinkDelete,
		ActionLinkBan,
		ActionLinkUnban,
		ActionMemberRoleUpdate,
		ActionMemberRemove,
		ActionInvitationCreate,
		ActionInvitationRevoke,
		ActionInvitationRedeem,
		ActionAPIKeyIssue,
		ActionAPIKeyRevoke,
		ActionWebhookCreate,
		ActionWebhookUpdate,
		ActionWebhookDelete,
		ActionWebhookRotateSecret,
		ActionWebhookTest,
		ActionWebhookAutoDisable,
	}
}

// Valid 报告 a 是否属于已定义的审计动作枚举。
func (a Action) Valid() bool {
	switch a {
	case ActionOrgCreate,
		ActionTenantLimitsUpdate,
		ActionLinkCreate,
		ActionLinkUpdate,
		ActionLinkDelete,
		ActionLinkBan,
		ActionLinkUnban,
		ActionMemberRoleUpdate,
		ActionMemberRemove,
		ActionInvitationCreate,
		ActionInvitationRevoke,
		ActionInvitationRedeem,
		ActionAPIKeyIssue,
		ActionAPIKeyRevoke,
		ActionWebhookCreate,
		ActionWebhookUpdate,
		ActionWebhookDelete,
		ActionWebhookRotateSecret,
		ActionWebhookTest,
		ActionWebhookAutoDisable:
		return true
	default:
		return false
	}
}

// TargetType 表示审计事件作用的目标资源类型。
type TargetType string

const (
	TargetOrg        TargetType = "org"
	TargetLink       TargetType = "link"
	TargetMember     TargetType = "member"
	TargetInvitation TargetType = "invitation"
	TargetAPIKey     TargetType = "api_key"
	TargetWebhook    TargetType = "webhook"
)

// Valid 报告 t 是否属于已定义的审计目标资源类型。
func (t TargetType) Valid() bool {
	switch t {
	case TargetOrg, TargetLink, TargetMember, TargetInvitation, TargetAPIKey, TargetWebhook:
		return true
	default:
		return false
	}
}

// Meta 携带从上游入口（HTTP、CLI、Worker）透传给存储写事务的审计上下文。
type Meta struct {
	ActorKind ActorKind
	ActorID   *int64
	RequestID string
}

// UserMeta 构造用户会话执行的审计元数据。
func UserMeta(userID int64, requestID string) Meta {
	id := userID
	return Meta{
		ActorKind: ActorUser,
		ActorID:   &id,
		RequestID: requestID,
	}
}

// APIKeyMeta 构造 API Key 执行的审计元数据。
func APIKeyMeta(keyID int64, requestID string) Meta {
	id := keyID
	return Meta{
		ActorKind: ActorAPIKey,
		ActorID:   &id,
		RequestID: requestID,
	}
}

// OperatorMeta 构造运维 CLI 执行的审计元数据。
func OperatorMeta(requestID string) Meta {
	return Meta{
		ActorKind: ActorOperator,
		RequestID: requestID,
	}
}

// SystemMeta 构造后台系统或异步风控任务执行的审计元数据。
func SystemMeta(requestID string) Meta {
	return Meta{
		ActorKind: ActorSystem,
		RequestID: requestID,
	}
}

type metaContextKey struct{}

// WithMeta 将审计元数据放进 context，克隆 ActorID 指针以防调用方后续修改。
func WithMeta(ctx context.Context, m Meta) context.Context {
	return context.WithValue(ctx, metaContextKey{}, cloneMeta(m))
}

// MetaFrom 从 context 读取审计元数据，并返回克隆副本防止外部篡改。
func MetaFrom(ctx context.Context) (Meta, bool) {
	if ctx == nil {
		return Meta{}, false
	}
	m, ok := ctx.Value(metaContextKey{}).(Meta)
	if !ok {
		return Meta{}, false
	}
	return cloneMeta(m), true
}

/*
ValidateMeta 校验审计元数据是否满足执行主体一致性约束（fail-closed）：
  - ActorKind 必须是已定义枚举；
  - user / api_key 必须带有正整数 ActorID；
  - operator / system 禁止携带 ActorID。
*/
func ValidateMeta(m Meta) error {
	if !m.ActorKind.Valid() {
		return fmt.Errorf("%w: invalid actor kind %q", ErrMissingMeta, m.ActorKind)
	}
	switch m.ActorKind {
	case ActorUser, ActorAPIKey:
		if m.ActorID == nil || *m.ActorID <= 0 {
			return fmt.Errorf("%w: %s requires positive actor id", ErrMissingMeta, m.ActorKind)
		}
	case ActorOperator, ActorSystem:
		if m.ActorID != nil {
			return fmt.Errorf("%w: %s must not set actor id", ErrMissingMeta, m.ActorKind)
		}
	}
	return nil
}

/*
NormalizeMeta 强制满足数据库约束 ck_audit_logs_actor_consistency 与长度限制：
  - user / api_key 必须携带正整数 ActorID，否则退回为 system + nil ActorID；
  - operator / system 一律清掉 ActorID；
  - 未知执行主体退回为 system + nil ActorID；
  - RequestID 清洗非法字节并按 UTF-8 rune 边界截断到最多 128 字节，避免半个多字节字符触发 PG 22021 错误。
*/
func NormalizeMeta(m Meta) Meta {
	reqID := truncateUTF8(m.RequestID, maxRequestIDLen)

	switch m.ActorKind {
	case ActorUser, ActorAPIKey:
		if m.ActorID == nil || *m.ActorID <= 0 {
			return Meta{
				ActorKind: ActorSystem,
				RequestID: reqID,
			}
		}
		id := *m.ActorID
		return Meta{
			ActorKind: m.ActorKind,
			ActorID:   &id,
			RequestID: reqID,
		}
	case ActorOperator, ActorSystem:
		return Meta{
			ActorKind: m.ActorKind,
			RequestID: reqID,
		}
	default:
		return Meta{
			ActorKind: ActorSystem,
			RequestID: reqID,
		}
	}
}

func truncateUTF8(s string, maxBytes int) string {
	s = strings.ToValidUTF8(s, "")
	if len(s) <= maxBytes {
		return s
	}
	cut := maxBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

func cloneMeta(m Meta) Meta {
	out := Meta{
		ActorKind: m.ActorKind,
		RequestID: m.RequestID,
	}
	if m.ActorID != nil {
		id := *m.ActorID
		out.ActorID = &id
	}
	return out
}

// Event 是一条持久化审计日志事件，在查询 user 执行者时会关联补全邮箱与姓名。
type Event struct {
	ID         int64
	TenantID   int64
	ActorKind  ActorKind
	ActorID    *int64
	ActorEmail string
	ActorName  string
	Action     Action
	TargetType TargetType
	TargetID   string
	RequestID  string
	Metadata   json.RawMessage
	CreatedAt  time.Time
}

// ListCursor 是审计日志列表的 Keyset 游标（仅按 id 严格单调递减）。
type ListCursor struct {
	ID int64
}

// ListOptions 指定按租户查询审计日志的过滤和分页参数。
type ListOptions struct {
	TenantID int64
	Action   Action
	Limit    int
	Cursor   *ListCursor
}
