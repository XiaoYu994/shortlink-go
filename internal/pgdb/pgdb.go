/*
Package pgdb 提供无业务依赖的 PostgreSQL 连接池打开与驱动约束错误判定（ADR 0024）。
供 pgredirect、postgres 和 migrate 共用，确保 cmd/redirector 不拖入任何管理域或鉴权包。
*/
package pgdb

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib"
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

// IsUniqueViolation 报告错误是否是 PostgreSQL 的唯一约束冲突（SQLSTATE 23505）。
func IsUniqueViolation(err error) bool {
	pgErr, ok := errors.AsType[*pgconn.PgError](err)
	return ok && pgErr.Code == "23505"
}

// IsUniqueConstraintViolation 报告错误是否命中了指定名称的唯一约束（SQLSTATE 23505）。
func IsUniqueConstraintViolation(err error, constraint string) bool {
	return IsConstraintViolation(err, "23505", constraint)
}

// IsConstraintViolation 报告错误是否命中指定 SQLSTATE 与约束名。
func IsConstraintViolation(err error, code, constraint string) bool {
	pgErr, ok := errors.AsType[*pgconn.PgError](err)
	return ok && pgErr.Code == code && pgErr.ConstraintName == constraint
}

// NullTimeUTC 将可空数据库时间规范化为 UTC *time.Time。
func NullTimeUTC(nt sql.NullTime) *time.Time {
	if !nt.Valid {
		return nil
	}
	t := nt.Time.UTC()
	return &t
}
