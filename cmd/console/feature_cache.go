package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"

	"shortlink/internal/cache"
	"shortlink/internal/storemetrics"
)

/*
openRedisCache 统一 serve 与 worker 的可选 Redis 缓存初始化（ADR 0029、ADR 0030、ADR 0031）。
REDIS_URL 为空时返回 nil 缓存与空 cleanup，调用方直连 storemetrics -> pgStore。
*/
func openRedisCache(
	redisURL string,
	metricsStore *storemetrics.Store,
	registry *prometheus.Registry,
	fetchTimeout time.Duration,
	logger *slog.Logger,
) (*cache.Store, *redis.Client, func(), error) {
	if redisURL == "" {
		if logger != nil {
			logger.Info("redis cache disabled: REDIS_URL is not set")
		}
		return nil, nil, func() {}, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	rdb, err := cache.Open(ctx, redisURL)
	cancel()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("connect redis: %w", err)
	}
	if logger != nil {
		logger.Info("connected to redis")
	}
	cached := cache.New(metricsStore, rdb, registry, cache.Options{
		FetchTimeout: fetchTimeout,
		Logger:       logger,
	})
	return cached, rdb, func() { _ = rdb.Close() }, nil
}
