package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"

	"shortlink/internal/cache"
	"shortlink/internal/job"
	"shortlink/internal/storemetrics"
)

type cacheReconciler interface {
	Reconcile(ctx context.Context, code string) error
}

/*
openRedisCache 统一 serve 与 worker 的可选 Redis 缓存初始化（ADR 0029、ADR 0030）。
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

/*
cacheWorkerModule 负责消费 cache.reconcile 缓存电平对账任务（ADR 0025、ADR 0029、ADR 0030）。
未启用 Redis 时注册 disabledSkipHandler。
*/
type cacheWorkerModule struct {
	reconciler cacheReconciler
}

func (m cacheWorkerModule) Name() string { return "cache" }

func (m cacheWorkerModule) RegisterWorker(r WorkerRegistrar) error {
	if m.reconciler == nil {
		r.HandleDisabled(job.KindCacheReconcile, m.Name())
		return nil
	}
	r.Handle(job.KindCacheReconcile, func(ctx context.Context, j job.Job) error {
		var p struct {
			Code string `json:"code"`
		}
		if err := json.Unmarshal(j.Payload, &p); err != nil {
			return job.Permanent(fmt.Errorf("decode cache.reconcile payload: %w", err))
		}
		if p.Code == "" {
			return job.Permanent(errors.New("cache.reconcile payload missing code"))
		}
		return m.reconciler.Reconcile(ctx, p.Code)
	})
	return nil
}
