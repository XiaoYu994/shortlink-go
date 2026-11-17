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

	"shortlink/internal/idgen"
	"shortlink/internal/link"
	"shortlink/internal/tenant"
)

const (
	linksTable = "links"

	/*
		占配额和插入链接在同一条语句里完成，两条动作一起提交或一起回滚。
		quota 只在 link_count < link_quota 时加一；EXISTS (SELECT 1 FROM quota)
		靠它的 RETURNING 判断是否拿到了配额，数据修改 CTE 的改动只有 RETURNING 能看到。
		租户行不存在时照常插入链接，不加计数：这是 ADR 0010 允许的孤儿引用，
		只会在直接调用存储时出现，HTTP 路径的租户一定来自鉴权。
		语句失败（例如短码冲突）时配额更新随语句一起回滚，重试不会重复计数。
	*/
	insertLinkSQL = `WITH quota AS (
    UPDATE tenants
    SET link_count = link_count + 1
    WHERE id = $4 AND link_count < link_quota
    RETURNING 1
)
INSERT INTO links (code, url, expires_at, tenant_id)
SELECT $1, $2, $3, $4
WHERE EXISTS (SELECT 1 FROM quota)
   OR NOT EXISTS (SELECT 1 FROM tenants WHERE id = $4)
RETURNING id, created_at`
	/*
		过期行留在表里，短码继续占着。查询把它看成找不到。
		到期时刻本身算过期，界限是 expires_at > now()。
	*/
	getLinkSQL = `SELECT id, code, url, expires_at, created_at, tenant_id FROM ` + linksTable + `
WHERE code = $1 AND (expires_at IS NULL OR expires_at > now())`

	// quotaExceededSQL 只在插入没有返回行时执行，区分配额不足和租户不存在。
	quotaExceededSQL = `SELECT link_count >= link_quota FROM tenants WHERE id = $1`

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

// Store 是 PostgreSQL 版的链接存储，实现 httpapi.LinkStore。
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
Create 生成随机短码，在同一语句里占用配额并插入链接，成功后回填 ID、Code 和 CreatedAt。
配额不足返回 link.ErrQuotaExceeded，不写链接也不增加计数。
短码冲突时重新生成，最多重试 maxCodeAttempts 次；冲突的语句会连同计数一起回滚。
*/
func (s *Store) Create(ctx context.Context, l *link.Link) error {
	for range maxCodeAttempts {
		code, err := s.generate()
		if err != nil {
			return fmt.Errorf("generate code: %w", err)
		}

		err = s.db.QueryRowContext(ctx, insertLinkSQL, code, l.URL, l.ExpiresAt, l.TenantID).Scan(&l.ID, &l.CreatedAt)
		if err == nil {
			l.Code = code
			return nil
		}
		if errors.Is(err, sql.ErrNoRows) {
			exceeded, checkErr := s.quotaExceeded(ctx, l.TenantID)
			if checkErr != nil {
				return checkErr
			}
			if exceeded {
				return link.ErrQuotaExceeded
			}
			/*
				插入没有返回行，复查时配额又够了：并发的删除刚释放了配额。
				重新走一遍即可，换一个短码没有副作用。
			*/
			continue
		}
		if isUniqueViolation(err) {
			continue
		}
		return fmt.Errorf("insert link: %w", err)
	}
	return fmt.Errorf("insert link: code collision after %d attempts", maxCodeAttempts)
}

/*
quotaExceeded 复查租户是否真的没有配额。
插入语句只在租户存在且计数已满时不返回行；租户不存在时链接照常写入，
所以查不到租户按「没有超配额」处理，交给调用方走其他分支。
*/
func (s *Store) quotaExceeded(ctx context.Context, tenantID int64) (bool, error) {
	var exceeded bool
	err := s.db.QueryRowContext(ctx, quotaExceededSQL, tenantID).Scan(&exceeded)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("check link quota: %w", err)
	}
	return exceeded, nil
}

/*
GetByCode 按短码查找链接，找不到或已过期都返回 link.ErrNotFound。
过期行仍留在表里。
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

/*
Authenticate 用 API Key 的哈希查找租户，同时读出限流配置。
格式不对、查无此密钥、已过期、已吊销都是 ErrUnauthenticated。
这几种情况共用一个错误，调用方返回同一个 401，不透露密钥是否存在过。
其他错误原样往上送，避免把数据库故障说成密钥无效。
到期时刻本身算过期，界限和链接一样是 expires_at > now()。
*/
func (s *Store) Authenticate(ctx context.Context, token string) (tenant.Tenant, error) {
	if err := ctx.Err(); err != nil {
		return tenant.Tenant{}, err
	}
	if !tenant.ValidToken(token) {
		return tenant.Tenant{}, tenant.ErrUnauthenticated
	}
	var tn tenant.Tenant
	var externalID sql.NullString
	err := s.db.QueryRowContext(ctx, `
SELECT t.id, t.external_id, t.rate_limit_per_second
FROM api_keys k
JOIN tenants t ON t.id = k.tenant_id
WHERE k.key_hash = $1
  AND k.revoked_at IS NULL
  AND (k.expires_at IS NULL OR k.expires_at > now())`, tenant.HashAPIKey(token)).Scan(&tn.ID, &externalID, &tn.RateLimit)
	if errors.Is(err, sql.ErrNoRows) {
		return tenant.Tenant{}, tenant.ErrUnauthenticated
	}
	if err != nil {
		return tenant.Tenant{}, fmt.Errorf("authenticate api key: %w", err)
	}
	if externalID.Valid {
		tn.ExternalID = externalID.String
	}
	return tn, nil
}

// isUniqueViolation 报告错误是否是 PostgreSQL 的唯一约束冲突（SQLSTATE 23505）。
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
