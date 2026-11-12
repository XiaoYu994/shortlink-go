/*
Package cache 持有 Redis 客户端和短链的读缓存（CachedStore）。

CachedStore 会包在 storemetrics 外面：命中不再访问数据库，
存储耗时指标里就只有真正打到 PostgreSQL 的调用。
*/
package cache

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

/*
Open 建立 Redis 客户端并确认能连上。
addr 形如 localhost:6379。NewClient 本身不连网络，Ping 才连。
ctx 由调用方设置超时；Ping 失败时关掉已创建的客户端，避免留下连接池。

超时和重试都比 go-redis 默认值短：缓存故障时请求要尽快降级到数据库。
默认读超时 5 秒、重试 3 次，Redis 挂掉时一次 GET 就可能超过 REQUEST_TIMEOUT，
请求还没走到数据库就已经超时了。
*/
func Open(ctx context.Context, addr string) (*redis.Client, error) {
	client := redis.NewClient(&redis.Options{
		Addr:         addr,
		DialTimeout:  500 * time.Millisecond,
		ReadTimeout:  200 * time.Millisecond,
		WriteTimeout: 200 * time.Millisecond,
		MaxRetries:   1, // 0 表示使用默认的 3 次重试；-1 表示不重试
	})
	if err := client.Ping(ctx).Err(); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("ping redis: %w", err)
	}
	return client, nil
}
