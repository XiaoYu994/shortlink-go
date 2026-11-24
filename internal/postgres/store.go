/*
Package postgres 是存储层的 PostgreSQL 实现。

使用 database/sql 加 pgx 驱动，表名按 PostgreSQL 习惯用复数 links。
id 由数据库自增，只作内部主键；对外短码由 idgen.GenerateCode 随机生成，遇到唯一约束冲突时重试。
*/
package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib"

	"shortlink/internal/audit"
	"shortlink/internal/idgen"
	"shortlink/internal/job"
	"shortlink/internal/link"
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
		占本月配额、插入链接与入队异步风控任务在同一条多 CTE 语句里完成，三条动作一起提交或一起回滚（ADR 0020、ADR 0022）。
		quota 把「本月已创建数」和它所属的月份一起更新：月份过期时先按 0 算再加一；
		EXISTS (SELECT 1 FROM quota) 靠它的 RETURNING 判断是否拿到了配额，
		数据修改 CTE 的改动只有 RETURNING 能看到。
		租户不存在时 quota 为空，不再绕过配额静默写入孤儿链接，由 Create 补查后返回 ErrTenantNotFound（守住 ADR 0010 应用层完整性）。
		语句失败（例如短码冲突）时配额更新和风控入队随语句一起回滚，重试不会重复计数也不留孤儿任务。
	*/
	insertLinkSQL = `WITH quota AS (
    UPDATE tenants
    SET link_usage = (` + effectiveUsageSQL + `) + 1,
        link_usage_month = GREATEST(link_usage_month, ` + currentMonthSQL + `)
    WHERE id = $4
      AND (` + effectiveUsageSQL + `) < link_quota
    RETURNING 1
),
inserted AS (
    INSERT INTO links (code, url, expires_at, tenant_id)
    SELECT $1, $2, $3, $4
    WHERE EXISTS (SELECT 1 FROM quota)
    RETURNING id, code, url, expires_at, created_at, tenant_id
),
enqueued AS (
    INSERT INTO jobs (kind, tenant_id, payload)
    SELECT $8::text,
           i.tenant_id,
           jsonb_build_object('link_id', i.id, 'code', i.code, 'url', i.url)
    FROM inserted i
),
audited AS (
    INSERT INTO audit_logs (tenant_id, actor_kind, actor_id, action, target_type, target_id, request_id, metadata)
    SELECT i.tenant_id,
           $5::text,
           $6::bigint,
           $9::text,
           $10::text,
           i.code,
           $7::text,
           jsonb_strip_nulls(jsonb_build_object(
               'link_id', i.id,
               'url', i.url,
               'expires_at', i.expires_at
           ))
    FROM inserted i
)
SELECT id, created_at FROM inserted`
	/*
		过期、停用和封禁的行都留在表里，短码继续占着。查询把它们看成找不到。
		到期时刻本身算过期，界限是 expires_at > now()；
		disabled_at 和 banned_at 的过滤和 link.Servable 是同一组条件。
	*/
	getLinkSQL = `SELECT id, code, url, expires_at, created_at, tenant_id FROM ` + linksTable + `
WHERE code = $1
  AND (expires_at IS NULL OR expires_at > now())
  AND disabled_at IS NULL
  AND banned_at IS NULL`

	// quotaExceededSQL 只在插入没有返回行时执行，也用有效已用量区分配额不足和租户不存在。
	quotaExceededSQL = `SELECT (` + effectiveUsageSQL + `) >= link_quota FROM tenants WHERE id = $1`

	/*
		maxCodeAttempts 是短码冲突时的最大重试次数。
		8 位 Base62 约有 2.18e14 种组合；已有 100 万条链接时，单次碰撞概率约为 4.6e-9，
		连续 5 次都碰撞的概率约为 2e-42，可以忽略。
	*/
	maxCodeAttempts = 5
)

// PoolConfig 是数据库连接池参数。启动时由配置加载填好，再传给 Open。
type PoolConfig struct {
	MaxOpenConns    int
	MaxIdleConns    int
	ConnMaxLifetime time.Duration
	ConnMaxIdleTime time.Duration
}

// DefaultPoolConfig 返回本地开发的默认连接池参数。
func DefaultPoolConfig() PoolConfig {
	return PoolConfig{
		MaxOpenConns:    10,
		MaxIdleConns:    5,
		ConnMaxLifetime: time.Hour,
		ConnMaxIdleTime: 10 * time.Minute,
	}
}

/*
Store 是 PostgreSQL 存储层门面，隐式实现 redirector.LinkStore、console.Store、
click.Store、console.APIKeyAuthenticator、console.SessionStore、console.MemberStore、
console.OrgStore、console.InvitationStore、console.APIKeyStore、console.AuditLogStore
以及 cmd/console 运维命令所需的存储能力。
*/
type Store struct {
	db *sql.DB
	// generate 可以替换，测试里用来强制制造短码冲突。
	generate func() (string, error)
}

// NewStore 包装已打开的连接池。
func NewStore(db *sql.DB) *Store {
	return &Store{db: db, generate: idgen.GenerateCode}
}

/*
Open 打开 PostgreSQL 连接池并确认能连上。
DSN 形如 postgres://user:pass@host:5432/db?sslmode=disable。
*/
func Open(ctx context.Context, dsn string, cfg PoolConfig) (*sql.DB, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("open postgres: %w", err)
	}
	db.SetMaxOpenConns(cfg.MaxOpenConns)
	db.SetMaxIdleConns(cfg.MaxIdleConns)
	db.SetConnMaxLifetime(cfg.ConnMaxLifetime)
	db.SetConnMaxIdleTime(cfg.ConnMaxIdleTime)

	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	return db, nil
}

/*
Create 生成随机短码，在同一语句里占用本月配额并插入链接，成功后回填 ID、Code 和 CreatedAt。
租户不存在返回 ErrTenantNotFound；本月配额不足返回 link.ErrQuotaExceeded，不写链接也不增加用量。
删除链接不返还配额。短码冲突时重新生成，最多重试 maxCodeAttempts 次；冲突的语句会连同用量一起回滚。
*/
func (s *Store) Create(ctx context.Context, l *link.Link) error {
	if l.TenantID <= 0 {
		return ErrTenantNotFound
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	actorKind, actorID, reqID := resolveAuditMeta(ctx, audit.SystemMeta(""))
	for range maxCodeAttempts {
		code, err := s.generate()
		if err != nil {
			return fmt.Errorf("generate code: %w", err)
		}

		err = s.db.QueryRowContext(ctx, insertLinkSQL,
			code, l.URL, l.ExpiresAt, l.TenantID, actorKind, actorID, reqID,
			job.KindLinkRiskScan, string(audit.ActionLinkCreate), string(audit.TargetLink),
		).Scan(&l.ID, &l.CreatedAt)
		if err == nil {
			l.Code = code
			return nil
		}
		if errors.Is(err, sql.ErrNoRows) {
			exceeded, exists, checkErr := s.checkCreateQuota(ctx, l.TenantID)
			if checkErr != nil {
				return checkErr
			}
			if !exists {
				return ErrTenantNotFound
			}
			if exceeded {
				return link.ErrQuotaExceeded
			}
			/*
				插入没有返回行，复查时又没满：两条语句之间配额被调高，或者正好跨月。
				重新走一遍即可，换一个短码没有副作用。
			*/
			continue
		}
		if isUniqueConstraintViolation(err, "links_code_key") {
			continue
		}
		return fmt.Errorf("insert link: %w", err)
	}
	return fmt.Errorf("insert link: code collision after %d attempts", maxCodeAttempts)
}

/*
checkCreateQuota 用和创建语句相同的「有效已用量」区分租户不存在与本月配额已满。
*/
func (s *Store) checkCreateQuota(ctx context.Context, tenantID int64) (exceeded, exists bool, err error) {
	err = s.db.QueryRowContext(ctx, quotaExceededSQL, tenantID).Scan(&exceeded)
	if errors.Is(err, sql.ErrNoRows) {
		return false, false, nil
	}
	if err != nil {
		return false, false, fmt.Errorf("check link quota: %w", err)
	}
	return exceeded, true, nil
}

/*
GetByCode 按短码查找可跳转的链接，找不到、已过期、已停用或已封禁都返回 link.ErrNotFound。
这些行仍留在表里。
*/
func (s *Store) GetByCode(ctx context.Context, code string) (*link.Link, error) {
	var l link.Link
	err := s.db.QueryRowContext(ctx, getLinkSQL, code).Scan(
		&l.ID, &l.Code, &l.URL, &l.ExpiresAt, &l.CreatedAt, &l.TenantID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, link.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get link: %w", err)
	}
	return &l, nil
}

// isUniqueViolation 报告错误是否是 PostgreSQL 的唯一约束冲突（SQLSTATE 23505）。
func isUniqueViolation(err error) bool {
	pgErr, ok := errors.AsType[*pgconn.PgError](err)
	return ok && pgErr.Code == "23505"
}

func isUniqueConstraintViolation(err error, constraint string) bool {
	return isConstraintViolation(err, "23505", constraint)
}

func isConstraintViolation(err error, code, constraint string) bool {
	pgErr, ok := errors.AsType[*pgconn.PgError](err)
	return ok && pgErr.Code == code && pgErr.ConstraintName == constraint
}

func nullTimeUTC(nt sql.NullTime) *time.Time {
	if !nt.Valid {
		return nil
	}
	t := nt.Time.UTC()
	return &t
}
