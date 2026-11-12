/*
Package cache 持有 Redis 客户端和短链的读缓存。

读缓存包在 storemetrics 外面，命中不再进入存储耗时。
*/
package cache

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

/*
Open 建立客户端并 Ping。NewClient 不连网络；Ping 失败时关掉客户端，避免留下连接池。

超时和重试比 go-redis 默认值短。默认读超时 5 秒、重试 3 次，
Redis 挂掉时一次 GET 可能超过 REQUEST_TIMEOUT，请求还没回源就超时了。
*/
func Open(ctx context.Context, redisURL string) (*redis.Client, error) {
	opts, err := newOptions(redisURL)
	if err != nil {
		return nil, err
	}
	client := redis.NewClient(opts)
	if err := client.Ping(ctx).Err(); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("ping redis: %w", err)
	}
	return client, nil
}

/*
newOptions 把 Redis URL 转成 go-redis 的 Options。
密码、TLS、DB 编号都由 redis.ParseURL 解析，再统一覆盖超时和重试。
*/
func newOptions(redisURL string) (*redis.Options, error) {
	if redisURL == "" {
		return nil, fmt.Errorf("redis URL is empty")
	}
	opts, err := redis.ParseURL(redisURL)
	if err != nil {
		return nil, fmt.Errorf("parse REDIS_URL: %w", err)
	}

	opts.DialTimeout = 500 * time.Millisecond
	opts.ReadTimeout = 200 * time.Millisecond
	opts.WriteTimeout = 200 * time.Millisecond
	opts.MaxRetries = 1 // 0 表示使用默认的 3 次重试；-1 表示不重试
	return opts, nil
}
