package postgres

import (
	"context"
	"database/sql"
	"fmt"
)

/*
testDBLockID 是测试库的会话级咨询锁。
go test 会并行跑不同的包，而这些测试都会清空同一份测试库。
先拿到这把锁再清空，另一个包的测试会等到本次结束。
*/
const testDBLockID int64 = 7483941

/*
HoldTestDBLock 占住测试库的咨询锁，直到调用方执行返回的释放函数。
锁绑在单独的一条连接上，释放函数会解锁并关掉它；连接断开时锁也会消失。
生产代码不要调用。
*/
func HoldTestDBLock(ctx context.Context, db *sql.DB) (func(), error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("hold test database lock: %w", err)
	}
	if _, err := conn.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, testDBLockID); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("hold test database lock: %w", err)
	}
	return func() {
		_, _ = conn.ExecContext(context.Background(), `SELECT pg_advisory_unlock($1)`, testDBLockID)
		_ = conn.Close()
	}, nil
}
