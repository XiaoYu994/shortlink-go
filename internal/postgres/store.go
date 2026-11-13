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
)

const (
	linksTable = "links"

	insertLinkSQL = `INSERT INTO ` + linksTable + ` (code, url) VALUES ($1, $2) RETURNING id, created_at`
	getLinkSQL    = `SELECT id, code, url, created_at FROM ` + linksTable + ` WHERE code = $1`
	/*
		同一行在 data 里只出现一次，调用方已经按 id 加总。
		unnest 的两个数组按下标配对，长度必须一致。
	*/
	addClicksSQL = `UPDATE ` + linksTable + ` AS l
SET clicks = l.clicks + data.n
FROM unnest($1::bigint[], $2::bigint[]) AS data(id, n)
WHERE l.id = data.id`

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
Create 生成随机短码并插入数据库，成功后回填 ID、Code 和 CreatedAt。
短码冲突时重新生成，最多重试 maxCodeAttempts 次。
*/
func (s *Store) Create(ctx context.Context, l *link.Link) error {
	for range maxCodeAttempts {
		code, err := s.generate()
		if err != nil {
			return fmt.Errorf("generate code: %w", err)
		}

		err = s.db.QueryRowContext(ctx, insertLinkSQL, code, l.URL).Scan(&l.ID, &l.CreatedAt)
		if err == nil {
			l.Code = code
			return nil
		}
		if isUniqueViolation(err) {
			continue
		}
		return fmt.Errorf("insert link: %w", err)
	}
	return fmt.Errorf("insert link: code collision after %d attempts", maxCodeAttempts)
}

/*
GetByCode 按短码查找链接，找不到返回 link.ErrNotFound。
*/
func (s *Store) GetByCode(ctx context.Context, code string) (*link.Link, error) {
	var l link.Link
	err := s.db.QueryRowContext(ctx, getLinkSQL, code).Scan(&l.ID, &l.Code, &l.URL, &l.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, link.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get link: %w", err)
	}
	return &l, nil
}

/*
AddClicks 把一批点击加到已有计数上。
不存在的 id 匹配不到行，不影响同一条语句里的其他 id。
counts 为空时不访问数据库。[]int64 由 pgx 编成 bigint[]。
*/
func (s *Store) AddClicks(ctx context.Context, counts map[int64]int64) error {
	if len(counts) == 0 {
		return nil
	}

	ids := make([]int64, 0, len(counts))
	ns := make([]int64, 0, len(counts))
	for id, n := range counts {
		if id <= 0 || n <= 0 {
			continue
		}
		ids = append(ids, id)
		ns = append(ns, n)
	}
	if len(ids) == 0 {
		return nil
	}

	if _, err := s.db.ExecContext(ctx, addClicksSQL, ids, ns); err != nil {
		return fmt.Errorf("add clicks: %w", err)
	}
	return nil
}

// isUniqueViolation 报告错误是否是 PostgreSQL 的唯一约束冲突（SQLSTATE 23505）。
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
