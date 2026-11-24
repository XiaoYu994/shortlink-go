package postgres

import "shortlink/internal/pgdb"

// HoldTestDBLock 占住测试库的咨询锁，与 pgdb 共用同一锁键。生产代码不要调用。
var HoldTestDBLock = pgdb.HoldTestDBLock
