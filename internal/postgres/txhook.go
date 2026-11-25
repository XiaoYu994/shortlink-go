package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"shortlink/internal/identity"
	"shortlink/internal/job"
	"shortlink/internal/link"
	"shortlink/internal/webhook"
)

const enqueueInvitationEmailSQL = `INSERT INTO jobs (kind, tenant_id, payload, dedup_key, max_attempts)
VALUES ($1, $2, $3::jsonb, $4, $5)
ON CONFLICT (kind, dedup_key) WHERE dedup_key IS NOT NULL AND status IN ('pending', 'running')
DO NOTHING`

/*
StoreOption 配置 PostgreSQL Store 的可选行为与事务内扩展钩子（ADR 0029）。
默认 NewStore(db) 不自动挂载可选外围钩子，由各进程组合根按启用模块显式传入。
*/
type StoreOption func(*Store)

// WithTxHooks 注册事务内领域事件钩子，按传入顺序在同一数据库事务内执行；nil 钩子会被忽略。
func WithTxHooks(hooks ...TxHook) StoreOption {
	return func(s *Store) {
		for _, h := range hooks {
			if h != nil {
				s.txHooks = append(s.txHooks, h)
			}
		}
	}
}

// WithAnalytics 控制读路径与更新返回是否关联 link_clicks_daily 统计表（默认 true，ADR 0029）。
func WithAnalytics(enabled bool) StoreOption {
	return func(s *Store) {
		s.analyticsEnabled = enabled
	}
}

/*
TxEvent 表示核心写事务已落盘主表后发出的领域事件（ADR 0029）。
仅允许本包内定义的具体事件值类型实现，确保类型断言边界清晰。
*/
type TxEvent interface {
	isTxEvent()
}

// TxHook 在主业务写与必选审计日志写入完成后、同一数据库事务提交前执行。
// 任意 Hook 返回错误都会触发整笔事务原子回滚；Hook 内不得执行外网 I/O。
type TxHook func(ctx context.Context, tx *sql.Tx, ev TxEvent) error

// LinkCreatedEvent 在新建短链主行成功插入后触发。
type LinkCreatedEvent struct {
	Link link.Link
}

func (LinkCreatedEvent) isTxEvent() {}

// LinkUpdatedEvent 在短链修改成功落库后触发。
type LinkUpdatedEvent struct {
	Link          link.Link
	URLChanged    bool
	ChangedFields []string
	UpdatedAt     time.Time
}

func (LinkUpdatedEvent) isTxEvent() {}

// LinkDeletedEvent 在短链主行成功删除后触发。
type LinkDeletedEvent struct {
	TenantID  int64
	LinkID    int64
	Code      string
	URL       string
	DeletedAt time.Time
}

func (LinkDeletedEvent) isTxEvent() {}

// LinkBannedEvent 在平台封禁执行完成后触发。
// 幂等重跑已封禁链接时 NewlyBanned 为 false，缓存对账仍可执行，Webhook 只在首次封禁时扇出。
type LinkBannedEvent struct {
	Link        link.Link
	NewlyBanned bool
}

func (LinkBannedEvent) isTxEvent() {}

// LinkUnbannedEvent 在平台解封执行完成后触发。
// 幂等重跑未封禁链接时 WasBanned 为 false，缓存对账仍可执行，Webhook 只在实际解封时扇出。
type LinkUnbannedEvent struct {
	Link           link.Link
	WasBanned      bool
	PreviousReason link.BanReason
	PreviousSource link.BanSource
	UnbannedAt     time.Time
}

func (LinkUnbannedEvent) isTxEvent() {}

// InvitationCreatedEvent 在新建组织成员邀请成功后触发。
type InvitationCreatedEvent struct {
	Invitation identity.Invitation
}

func (InvitationCreatedEvent) isTxEvent() {}

type linkRiskScanPayload struct {
	LinkID int64  `json:"link_id"`
	Code   string `json:"code"`
	URL    string `json:"url"`
}

type cacheReconcilePayload struct {
	Code string `json:"code"`
}

type linkPurgeStatsPayload struct {
	LinkID int64 `json:"link_id"`
}

type linkCreatedWebhookData struct {
	LinkID    int64      `json:"link_id"`
	Code      string     `json:"code"`
	URL       string     `json:"url"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
}

type linkUpdatedWebhookData struct {
	LinkID        int64      `json:"link_id"`
	Code          string     `json:"code"`
	URL           string     `json:"url"`
	ExpiresAt     *time.Time `json:"expires_at,omitempty"`
	DisabledAt    *time.Time `json:"disabled_at,omitempty"`
	ChangedFields []string   `json:"changed_fields"`
	UpdatedAt     time.Time  `json:"updated_at"`
}

type linkDeletedWebhookData struct {
	LinkID    int64     `json:"link_id"`
	Code      string    `json:"code"`
	URL       string    `json:"url"`
	DeletedAt time.Time `json:"deleted_at"`
}

type linkBannedWebhookData struct {
	LinkID    int64          `json:"link_id"`
	Code      string         `json:"code"`
	URL       string         `json:"url"`
	BannedAt  time.Time      `json:"banned_at"`
	BanReason link.BanReason `json:"ban_reason"`
	BanSource link.BanSource `json:"ban_source"`
}

type linkUnbannedWebhookData struct {
	LinkID         int64          `json:"link_id"`
	Code           string         `json:"code"`
	URL            string         `json:"url"`
	ExpiresAt      *time.Time     `json:"expires_at,omitempty"`
	DisabledAt     *time.Time     `json:"disabled_at,omitempty"`
	PreviousReason link.BanReason `json:"previous_reason,omitempty"`
	PreviousSource link.BanSource `json:"previous_source,omitempty"`
	UnbannedAt     time.Time      `json:"unbanned_at"`
}

// RiskScanHook 返回异步 URL 风控入队钩子：响应短链创建与修改目标 URL。
func RiskScanHook() TxHook {
	return func(ctx context.Context, tx *sql.Tx, ev TxEvent) error {
		switch e := ev.(type) {
		case LinkCreatedEvent:
			return insertJobTx(ctx, tx, job.KindLinkRiskScan, e.Link.TenantID, linkRiskScanPayload{
				LinkID: e.Link.ID,
				Code:   e.Link.Code,
				URL:    e.Link.URL,
			})
		case LinkUpdatedEvent:
			if !e.URLChanged {
				return nil
			}
			return insertJobTx(ctx, tx, job.KindLinkRiskScan, e.Link.TenantID, linkRiskScanPayload{
				LinkID: e.Link.ID,
				Code:   e.Link.Code,
				URL:    e.Link.URL,
			})
		default:
			return nil
		}
	}
}

// WebhookFanoutHook 返回租户 Webhook 事务内事件扇出钩子：覆盖短链 5 类生命周期事件。
func WebhookFanoutHook() TxHook {
	return func(ctx context.Context, tx *sql.Tx, ev TxEvent) error {
		switch e := ev.(type) {
		case LinkCreatedEvent:
			return enqueueWebhookEventTx(ctx, tx, e.Link.TenantID, webhook.EventLinkCreated, e.Link.CreatedAt, linkCreatedWebhookData{
				LinkID:    e.Link.ID,
				Code:      e.Link.Code,
				URL:       e.Link.URL,
				ExpiresAt: e.Link.ExpiresAt,
				CreatedAt: e.Link.CreatedAt,
			})
		case LinkUpdatedEvent:
			return enqueueWebhookEventTx(ctx, tx, e.Link.TenantID, webhook.EventLinkUpdated, e.UpdatedAt, linkUpdatedWebhookData{
				LinkID:        e.Link.ID,
				Code:          e.Link.Code,
				URL:           e.Link.URL,
				ExpiresAt:     e.Link.ExpiresAt,
				DisabledAt:    e.Link.DisabledAt,
				ChangedFields: e.ChangedFields,
				UpdatedAt:     e.UpdatedAt,
			})
		case LinkDeletedEvent:
			return enqueueWebhookEventTx(ctx, tx, e.TenantID, webhook.EventLinkDeleted, e.DeletedAt, linkDeletedWebhookData{
				LinkID:    e.LinkID,
				Code:      e.Code,
				URL:       e.URL,
				DeletedAt: e.DeletedAt,
			})
		case LinkBannedEvent:
			if !e.NewlyBanned || e.Link.Ban == nil {
				return nil
			}
			return enqueueWebhookEventTx(ctx, tx, e.Link.TenantID, webhook.EventLinkBanned, e.Link.Ban.At, linkBannedWebhookData{
				LinkID:    e.Link.ID,
				Code:      e.Link.Code,
				URL:       e.Link.URL,
				BannedAt:  e.Link.Ban.At,
				BanReason: e.Link.Ban.Reason,
				BanSource: e.Link.Ban.Source,
			})
		case LinkUnbannedEvent:
			if !e.WasBanned {
				return nil
			}
			return enqueueWebhookEventTx(ctx, tx, e.Link.TenantID, webhook.EventLinkUnbanned, e.UnbannedAt, linkUnbannedWebhookData{
				LinkID:         e.Link.ID,
				Code:           e.Link.Code,
				URL:            e.Link.URL,
				ExpiresAt:      e.Link.ExpiresAt,
				DisabledAt:     e.Link.DisabledAt,
				PreviousReason: e.PreviousReason,
				PreviousSource: e.PreviousSource,
				UnbannedAt:     e.UnbannedAt,
			})
		default:
			return nil
		}
	}
}

// InvitationEmailHook 返回成员邀请异步发信入队钩子。
func InvitationEmailHook() TxHook {
	return func(ctx context.Context, tx *sql.Tx, ev TxEvent) error {
		e, ok := ev.(InvitationCreatedEvent)
		if !ok {
			return nil
		}
		emailPayload, err := json.Marshal(struct {
			InvitationID int64 `json:"invitation_id"`
		}{InvitationID: e.Invitation.ID})
		if err != nil {
			return fmt.Errorf("enqueue email: marshal payload: %w", err)
		}
		if _, err := tx.ExecContext(ctx, enqueueInvitationEmailSQL,
			job.KindInvitationEmail,
			e.Invitation.TenantID,
			emailPayload,
			"invitation:"+strconv.FormatInt(e.Invitation.ID, 10),
			job.DefaultMaxAttempts,
		); err != nil {
			return fmt.Errorf("enqueue email: %w", err)
		}
		return nil
	}
}

// CacheReconcileHook 返回 Redis 缓存电平对账入队钩子（ADR 0025）。
func CacheReconcileHook() TxHook {
	return func(ctx context.Context, tx *sql.Tx, ev TxEvent) error {
		switch e := ev.(type) {
		case LinkUpdatedEvent:
			return insertJobTx(ctx, tx, job.KindCacheReconcile, e.Link.TenantID, cacheReconcilePayload{Code: e.Link.Code})
		case LinkDeletedEvent:
			return insertJobTx(ctx, tx, job.KindCacheReconcile, e.TenantID, cacheReconcilePayload{Code: e.Code})
		case LinkBannedEvent:
			return insertJobTx(ctx, tx, job.KindCacheReconcile, e.Link.TenantID, cacheReconcilePayload{Code: e.Link.Code})
		case LinkUnbannedEvent:
			return insertJobTx(ctx, tx, job.KindCacheReconcile, e.Link.TenantID, cacheReconcilePayload{Code: e.Link.Code})
		default:
			return nil
		}
	}
}

// StatsPurgeHook 返回删除短链后异步分批回收日统计表残留行的钩子（ADR 0025）。
func StatsPurgeHook() TxHook {
	return func(ctx context.Context, tx *sql.Tx, ev TxEvent) error {
		e, ok := ev.(LinkDeletedEvent)
		if !ok {
			return nil
		}
		return insertJobTx(ctx, tx, job.KindLinkPurgeStats, e.TenantID, linkPurgeStatsPayload{LinkID: e.LinkID})
	}
}

func (s *Store) dispatchTxHooks(ctx context.Context, tx *sql.Tx, ev TxEvent) error {
	for _, hook := range s.txHooks {
		if err := hook(ctx, tx, ev); err != nil {
			return err
		}
	}
	return nil
}
