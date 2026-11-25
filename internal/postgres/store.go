/*
Package postgres 是存储层的 PostgreSQL 实现。

使用 database/sql 加 pgx 驱动，表名按 PostgreSQL 习惯用复数 links。
公开跳转与点击写出由 pgredirect 实现并嵌入 Store，管理写、鉴权、任务与审计收敛在本包（ADR 0024）。
id 由数据库自增，只作内部主键；对外短码由 idgen.GenerateCode 随机生成，遇到唯一约束冲突时重试。
*/
package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"shortlink/internal/audit"
	"shortlink/internal/idgen"
	"shortlink/internal/job"
	"shortlink/internal/link"
	"shortlink/internal/pgdb"
	"shortlink/internal/pgredirect"
	"shortlink/internal/webhook"
)

const (
	linksTable = "links"

	/*
		当前月取数据库时钟的 UTC 月界。console 多副本时各自的系统时钟可能不同，
		而跳转判断过期用的也是数据库时钟，所以月份也交给数据库算。
		AT TIME ZONE 'UTC' 先把时刻换成 UTC 的墙上时间再截断，结果不依赖会话时区。
	*/
	currentMonthSQL = `date_trunc('month', now() AT TIME ZONE 'UTC')::date`

	/*
		effectiveUsageSQL 是「有效已用量」：记录的月份早于当前月时按 0 算。
		跨月重置不依赖定时任务，读和写都必须用这个表达式；直接读 link_usage
		会在跨月后、当月第一次创建前看到上个月的数。
		月份在创建语句里用 GREATEST 只前进不后退，时钟回拨时不会把已经
		记到新月份的用量再当成旧月份清一次。
	*/
	effectiveUsageSQL = `CASE WHEN link_usage_month < ` + currentMonthSQL + `
    THEN 0 ELSE link_usage END`

	/*
		claimLinkQuotaSQL 在租户事务内原子占一个本月配额，并同步推进用量月份。
		租户不存在或本月配额已满时返回 0 行，由 checkCreateQuotaTx 区分原因，
		守住 ADR 0010 的应用层完整性。
	*/
	claimLinkQuotaSQL = `UPDATE tenants
SET link_usage = (` + effectiveUsageSQL + `) + 1,
    link_usage_month = GREATEST(link_usage_month, ` + currentMonthSQL + `)
WHERE id = $1
  AND (` + effectiveUsageSQL + `) < link_quota`

	insertLinkRowSQL = `INSERT INTO links (code, url, expires_at, tenant_id)
VALUES ($1, $2, $3, $4)
RETURNING id, created_at`

	// quotaExceededSQL 只在扣配额返回 0 行时执行，用同一有效已用量表达式区分配额不足与租户不存在。
	quotaExceededSQL = `SELECT (` + effectiveUsageSQL + `) >= link_quota FROM tenants WHERE id = $1`

	/*
		maxCodeAttempts 是短码冲突或跨月竞态时的最大重试次数。
		8 位 Base62 约有 2.18e14 种组合；已有 100 万条链接时，单次碰撞概率约为 4.6e-9，
		连续 5 次都碰撞的概率约为 2e-42，可以忽略。
	*/
	maxCodeAttempts = 5
)

var (
	errCreateCodeCollision = errors.New("code collision")
	errCreateQuotaRaced    = errors.New("quota check raced")
)

type linkRiskScanPayload struct {
	LinkID int64  `json:"link_id"`
	Code   string `json:"code"`
	URL    string `json:"url"`
}

type linkCreatedWebhookData struct {
	LinkID    int64      `json:"link_id"`
	Code      string     `json:"code"`
	URL       string     `json:"url"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
}

// PoolConfig 是数据库连接池参数，别名自 pgdb 保持向后兼容。
type PoolConfig = pgdb.PoolConfig

var (
	// DefaultPoolConfig 返回本地开发的默认连接池参数。
	DefaultPoolConfig = pgdb.DefaultPoolConfig
	// Open 打开 PostgreSQL 连接池并确认能连上。
	Open = pgdb.Open

	isUniqueViolation           = pgdb.IsUniqueViolation
	isUniqueConstraintViolation = pgdb.IsUniqueConstraintViolation
	isConstraintViolation       = pgdb.IsConstraintViolation
	nullTimeUTC                 = pgdb.NullTimeUTC
)

/*
Store 是 PostgreSQL 存储层门面，隐式实现 redirector.LinkStore、console.Store、
click.Store、console.APIKeyAuthenticator、console.SessionStore、console.MemberStore、
console.OrgStore、console.InvitationStore、console.APIKeyStore、console.AuditLogStore
以及 cmd/console 运维命令所需的存储能力。
*/
type Store struct {
	*pgredirect.Store
	db *sql.DB
	// generate 可以替换，测试里用来强制制造短码冲突。
	generate func() (string, error)
}

// NewStore 包装已打开的连接池。
func NewStore(db *sql.DB) *Store {
	return &Store{
		Store:    pgredirect.NewStore(db),
		db:       db,
		generate: idgen.GenerateCode,
	}
}

/*
Create 生成随机短码，在同一个租户事务内依次完成占本月配额、插入链接、入队异步风控、
扇出 Webhook 与写审计日志，成功后回填 ID、Code 和 UTC CreatedAt。
租户不存在返回 ErrTenantNotFound；本月配额不足返回 link.ErrQuotaExceeded，不写链接也不增加用量。
删除链接不返还配额。短码冲突或跨月竞态时整事务回滚后重试，最多重试 maxCodeAttempts 次。
*/
func (s *Store) Create(ctx context.Context, l *link.Link) error {
	if l.TenantID <= 0 {
		return ErrTenantNotFound
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, _, _, err := requireAuditMeta(ctx); err != nil {
		return fmt.Errorf("create link: %w", err)
	}

	var lastRetryErr error
	for range maxCodeAttempts {
		code, err := s.generate()
		if err != nil {
			return fmt.Errorf("generate code: %w", err)
		}

		var (
			createdID int64
			createdAt time.Time
		)
		err = s.withTenantTx(ctx, l.TenantID, nil, func(tx *sql.Tx) error {
			if err := claimLinkQuotaTx(ctx, tx, l.TenantID); err != nil {
				return err
			}
			id, at, err := insertLinkRowTx(ctx, tx, code, l)
			if err != nil {
				return err
			}
			if err := insertJobTx(ctx, tx, job.KindLinkRiskScan, l.TenantID, linkRiskScanPayload{
				LinkID: id,
				Code:   code,
				URL:    l.URL,
			}); err != nil {
				return fmt.Errorf("insert link: %w", err)
			}
			var expiresAtUTC *time.Time
			if l.ExpiresAt != nil {
				t := l.ExpiresAt.UTC()
				expiresAtUTC = &t
			}
			if err := enqueueWebhookEventTx(ctx, tx, l.TenantID, webhook.EventLinkCreated, at, linkCreatedWebhookData{
				LinkID:    id,
				Code:      code,
				URL:       l.URL,
				ExpiresAt: expiresAtUTC,
				CreatedAt: at,
			}); err != nil {
				return fmt.Errorf("insert link: %w", err)
			}
			auditMeta := map[string]any{
				"link_id": id,
				"url":     l.URL,
			}
			if expiresAtUTC != nil {
				auditMeta["expires_at"] = expiresAtUTC.Format(time.RFC3339Nano)
			}
			if err := insertAuditLogTx(ctx, tx, l.TenantID, 0, audit.ActionLinkCreate, audit.TargetLink, code, auditMeta); err != nil {
				return fmt.Errorf("insert link: %w", err)
			}
			createdID = id
			createdAt = at
			return nil
		})
		if err == nil {
			l.ID = createdID
			l.CreatedAt = createdAt
			l.Code = code
			return nil
		}
		if errors.Is(err, errCreateCodeCollision) || errors.Is(err, errCreateQuotaRaced) {
			lastRetryErr = err
			continue
		}
		return err
	}
	return fmt.Errorf("insert link: %w after %d attempts", lastRetryErr, maxCodeAttempts)
}

func claimLinkQuotaTx(ctx context.Context, tx *sql.Tx, tenantID int64) error {
	res, err := tx.ExecContext(ctx, claimLinkQuotaSQL, tenantID)
	if err != nil {
		return fmt.Errorf("insert link: claim quota: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("insert link: claim quota rows: %w", err)
	}
	if n > 0 {
		return nil
	}
	exceeded, exists, err := checkCreateQuotaTx(ctx, tx, tenantID)
	if err != nil {
		return err
	}
	if !exists {
		return ErrTenantNotFound
	}
	if exceeded {
		return link.ErrQuotaExceeded
	}
	/*
		扣配额返回 0 行但补查时未满：两条语句之间配额被调高，或正好跨月。
		交由外层回滚并重试，区分于短码碰撞。
	*/
	return errCreateQuotaRaced
}

func insertLinkRowTx(ctx context.Context, tx *sql.Tx, code string, l *link.Link) (int64, time.Time, error) {
	var (
		createdID int64
		createdAt time.Time
	)
	err := tx.QueryRowContext(ctx, insertLinkRowSQL, code, l.URL, l.ExpiresAt, l.TenantID).Scan(&createdID, &createdAt)
	if err != nil {
		if isUniqueConstraintViolation(err, "links_code_key") {
			return 0, time.Time{}, errCreateCodeCollision
		}
		return 0, time.Time{}, fmt.Errorf("insert link: %w", err)
	}
	return createdID, createdAt.UTC(), nil
}

/*
checkCreateQuotaTx 用和扣配额语句相同的「有效已用量」区分租户不存在与本月配额已满。
*/
func checkCreateQuotaTx(ctx context.Context, tx *sql.Tx, tenantID int64) (exceeded, exists bool, err error) {
	err = tx.QueryRowContext(ctx, quotaExceededSQL, tenantID).Scan(&exceeded)
	if errors.Is(err, sql.ErrNoRows) {
		return false, false, nil
	}
	if err != nil {
		return false, false, fmt.Errorf("check link quota: %w", err)
	}
	return exceeded, true, nil
}
