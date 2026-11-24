/*
Package pgredirect 实现公开短链跳转进程（cmd/redirector）所需的只读查询与异步点击批量写入（ADR 0009、ADR 0024）。

本包仅依赖 pgdb、link 与 click，彻底隔离管理、身份鉴权、任务队列和审计日志依赖。
postgres.Store 通过嵌入 *pgredirect.Store 复用同一套 SQL 与扫码/写库逻辑。
*/
package pgredirect

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"shortlink/internal/link"
	"shortlink/internal/pgdb"
)

const (
	/*
		过期、停用和封禁的行都留在表里，短码继续占着。查询把它们看成找不到。
		到期时刻本身算过期，界限是 expires_at > now()；
		disabled_at 和 banned_at 的过滤和 link.Servable 是同一组条件。
	*/
	getLinkSQL = `SELECT id, code, url, expires_at, created_at, tenant_id FROM links
WHERE code = $1
  AND (expires_at IS NULL OR expires_at > now())
  AND disabled_at IS NULL
  AND banned_at IS NULL`
)

// Store 实现公开短链跳转的 GetByCode 与点击批次写出 RecordClicks。
type Store struct {
	db *sql.DB
}

// NewStore 包装已打开的数据库连接池。
func NewStore(db *sql.DB) *Store {
	return &Store{db: db}
}

/*
GetByCode 按短码查找可跳转的链接，找不到、已过期、已停用或已封禁都返回 link.ErrNotFound。
这些行仍留在表里。
*/
func (s *Store) GetByCode(ctx context.Context, code string) (*link.Link, error) {
	var (
		l         link.Link
		expiresAt sql.NullTime
	)
	err := s.db.QueryRowContext(ctx, getLinkSQL, code).Scan(
		&l.ID, &l.Code, &l.URL, &expiresAt, &l.CreatedAt, &l.TenantID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, link.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get link: %w", err)
	}
	l.ExpiresAt = pgdb.NullTimeUTC(expiresAt)
	l.CreatedAt = l.CreatedAt.UTC()
	return &l, nil
}
