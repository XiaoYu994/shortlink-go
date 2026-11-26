package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"shortlink/internal/console"
	"shortlink/internal/job"
)

type statsMaintenanceStore interface {
	PurgeLinkStatsBatch(ctx context.Context, linkID int64, limit int) (int64, error)
	EnsureStatsPartitions(ctx context.Context, now time.Time, monthsAhead int) (int, error)
	DropExpiredStatsPartitions(ctx context.Context, now time.Time, retentionDays int) (int64, error)
}

// analyticsConsoleModule 按 FEATURE_ANALYTICS 返回点击统计 HTTP 模块（关闭时返回 nil）。
func analyticsConsoleModule(enabled bool, store console.LinkStatsStore) console.Module {
	if !enabled {
		return nil
	}
	return console.NewStatsModule(store)
}

/*
analyticsWorkerModule 收口点击统计特性的后台生命周期（ADR 0029、ADR 0030）：
启动期预建统计月分区、消费 link.purge_stats 分批清理与定期淘汰过期统计分区；
关闭特性时仅注册 link.purge_stats 的 disabledSkipHandler。
*/
type analyticsWorkerModule struct {
	enabled         bool
	store           statsMaintenanceStore
	cleanupInterval time.Duration
	retentionDays   int
}

func (m analyticsWorkerModule) Name() string { return "analytics" }

func (m analyticsWorkerModule) RegisterWorker(r WorkerRegistrar) error {
	if !m.enabled {
		r.HandleDisabled(job.KindLinkPurgeStats, m.Name())
		return nil
	}
	if m.store == nil {
		return errors.New("stats maintenance store is nil")
	}
	r.OnStart("stats_partitions.ensure", func(ctx context.Context) error {
		_, err := m.store.EnsureStatsPartitions(ctx, time.Now().UTC(), partitionMonthsAhead)
		return err
	})
	r.Handle(job.KindLinkPurgeStats, func(ctx context.Context, j job.Job) error {
		var p struct {
			LinkID int64 `json:"link_id"`
		}
		if err := json.Unmarshal(j.Payload, &p); err != nil {
			return job.Permanent(fmt.Errorf("decode link.purge_stats payload: %w", err))
		}
		if p.LinkID <= 0 {
			return job.Permanent(errors.New("link.purge_stats payload missing link_id"))
		}
		for range maxPurgeStatsBatchesPerRun {
			n, err := m.store.PurgeLinkStatsBatch(ctx, p.LinkID, purgeStatsBatchLimit)
			if err != nil {
				return err
			}
			if n < purgeStatsBatchLimit {
				return nil
			}
		}
		return job.Defer(purgeStatsDeferDelay, "more link stats remain to purge")
	})
	r.Periodic(job.PeriodicTask{
		Name:     "stats_partitions.maintain",
		Interval: m.cleanupInterval,
		Run: func(ctx context.Context) (int64, error) {
			now := time.Now().UTC()
			if _, err := m.store.EnsureStatsPartitions(ctx, now, partitionMonthsAhead); err != nil {
				return 0, err
			}
			return m.store.DropExpiredStatsPartitions(ctx, now, m.retentionDays)
		},
	})
	return nil
}
