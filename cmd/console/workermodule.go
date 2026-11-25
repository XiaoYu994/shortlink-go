package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"shortlink/internal/job"
	"shortlink/internal/logx"
)

/*
WorkerRegistrar 向各后台模块提供受控的注册入口，用于声明任务处理器与周期维护任务（ADR 0029）。
*/
type WorkerRegistrar interface {
	Handle(kind string, h job.Handler)
	HandleDisabled(kind, featureName string)
	Periodic(task job.PeriodicTask)
}

/*
WorkerModule 把一个后台特性的任务处理器与配套周期维护任务打包成独立模块。
关闭某特性时，模块仍通过 HandleDisabled 为该特性的历史残留任务类型注册兜底处理器，
让抢占后的任务记日志并按 job.Skip 干净收尾，既不修改 claim SQL 索引路径，也不污染死信队列。
*/
type WorkerModule interface {
	Name() string
	RegisterWorker(r WorkerRegistrar) error
}

type workerRegistry struct {
	logger   *slog.Logger
	handlers map[string]job.Handler
	periodic []job.PeriodicTask
	taskSet  map[string]struct{}
	err      error
}

func (r *workerRegistry) Handle(kind string, h job.Handler) {
	if r.err != nil {
		return
	}
	if kind == "" {
		r.err = errors.New("worker module registered empty job kind")
		return
	}
	if h == nil {
		r.err = fmt.Errorf("worker module registered nil handler for kind %q", kind)
		return
	}
	if _, dup := r.handlers[kind]; dup {
		r.err = fmt.Errorf("duplicate worker handler for job kind %q", kind)
		return
	}
	r.handlers[kind] = h
}

func (r *workerRegistry) HandleDisabled(kind, featureName string) {
	r.Handle(kind, disabledSkipHandler(r.logger, featureName))
}

func (r *workerRegistry) Periodic(task job.PeriodicTask) {
	if r.err != nil {
		return
	}
	if task.Name == "" {
		r.err = errors.New("worker module registered empty periodic task name")
		return
	}
	if task.Run == nil {
		r.err = fmt.Errorf("worker module registered nil periodic task %q", task.Name)
		return
	}
	if _, dup := r.taskSet[task.Name]; dup {
		r.err = fmt.Errorf("duplicate worker periodic task %q", task.Name)
		return
	}
	r.taskSet[task.Name] = struct{}{}
	r.periodic = append(r.periodic, task)
}

// assembleWorkerModules 按注册顺序收集全部模块的 Handler 与 PeriodicTask，并校验无重复声明与空依赖。
func assembleWorkerModules(logger *slog.Logger, modules ...WorkerModule) (map[string]job.Handler, []job.PeriodicTask, error) {
	if logger != nil {
		logger = slog.New(logx.WrapContext(logger.Handler()))
	}
	reg := &workerRegistry{
		logger:   logger,
		handlers: make(map[string]job.Handler),
		taskSet:  make(map[string]struct{}),
	}
	seenMod := make(map[string]struct{}, len(modules))
	for _, m := range modules {
		if m == nil {
			continue
		}
		name := m.Name()
		if name == "" {
			return nil, nil, errors.New("worker module name must not be empty")
		}
		if _, dup := seenMod[name]; dup {
			return nil, nil, fmt.Errorf("duplicate worker module %q", name)
		}
		seenMod[name] = struct{}{}
		if err := m.RegisterWorker(reg); err != nil {
			return nil, nil, fmt.Errorf("worker module %q: %w", name, err)
		}
		if reg.err != nil {
			return nil, nil, fmt.Errorf("worker module %q: %w", name, reg.err)
		}
	}
	return reg.handlers, reg.periodic, nil
}

/*
disabledSkipHandler 为已关闭内置特性的历史/滚动发布期残留任务提供兜底处理器（ADR 0029）。
记录一条信息日志后返回 job.Skip 而非未注册错误，使任务按 skipped 归档为 done，避免打入 dead 触发误告警。
*/
func disabledSkipHandler(logger *slog.Logger, featureName string) job.Handler {
	reason := "feature disabled: " + featureName
	return func(ctx context.Context, _ job.Job) error {
		if logger != nil {
			logger.InfoContext(ctx, "job skipped because feature is disabled", "feature", featureName)
		}
		return job.Skip(reason)
	}
}

type coreCleanupStore interface {
	CleanupExpiredSessionsByExpiry(ctx context.Context, retention time.Duration, limit int) (int64, error)
	CleanupRevokedSessions(ctx context.Context, retention time.Duration, limit int) (int64, error)
	CleanupDoneJobs(ctx context.Context, retention time.Duration, limit int) (int64, error)
	CleanupDeadJobs(ctx context.Context, retention time.Duration, limit int) (int64, error)
	CleanupExpiredInvitationsByExpiry(ctx context.Context, retention time.Duration, limit int) (int64, error)
	CleanupRevokedInvitations(ctx context.Context, retention time.Duration, limit int) (int64, error)
	CleanupSupersededInvitations(ctx context.Context, retention time.Duration, limit int) (int64, error)
	EnsureAuditPartitions(ctx context.Context, now time.Time, monthsAhead int) (int, error)
	DropExpiredAuditPartitions(ctx context.Context, now time.Time, retentionDays int) (int64, error)
}

type coreWorkerModule struct {
	store               coreCleanupStore
	cleanupInterval     time.Duration
	sessionRetention    time.Duration
	doneJobRetention    time.Duration
	deadJobRetention    time.Duration
	invitationRetention time.Duration
	auditRetentionDays  int
}

func (m coreWorkerModule) Name() string { return "core" }

func (m coreWorkerModule) RegisterWorker(r WorkerRegistrar) error {
	if m.store == nil {
		return errors.New("core cleanup store is nil")
	}
	r.Periodic(job.PeriodicTask{
		Name:     "sessions.cleanup_expired",
		Interval: m.cleanupInterval,
		Run: func(ctx context.Context) (int64, error) {
			return drainCleanupBatches(ctx, cleanupBatchLimit, maxCleanupBatchesPerTick, func(ctx context.Context, limit int) (int64, error) {
				return m.store.CleanupExpiredSessionsByExpiry(ctx, m.sessionRetention, limit)
			})
		},
	})
	r.Periodic(job.PeriodicTask{
		Name:     "sessions.cleanup_revoked",
		Interval: m.cleanupInterval,
		Run: func(ctx context.Context) (int64, error) {
			return drainCleanupBatches(ctx, cleanupBatchLimit, maxCleanupBatchesPerTick, func(ctx context.Context, limit int) (int64, error) {
				return m.store.CleanupRevokedSessions(ctx, m.sessionRetention, limit)
			})
		},
	})
	r.Periodic(job.PeriodicTask{
		Name:     "jobs.cleanup_done",
		Interval: m.cleanupInterval,
		Run: func(ctx context.Context) (int64, error) {
			return drainCleanupBatches(ctx, cleanupBatchLimit, maxCleanupBatchesPerTick, func(ctx context.Context, limit int) (int64, error) {
				return m.store.CleanupDoneJobs(ctx, m.doneJobRetention, limit)
			})
		},
	})
	r.Periodic(job.PeriodicTask{
		Name:     "jobs.cleanup_dead",
		Interval: m.cleanupInterval,
		Run: func(ctx context.Context) (int64, error) {
			return drainCleanupBatches(ctx, cleanupBatchLimit, maxCleanupBatchesPerTick, func(ctx context.Context, limit int) (int64, error) {
				return m.store.CleanupDeadJobs(ctx, m.deadJobRetention, limit)
			})
		},
	})
	r.Periodic(job.PeriodicTask{
		Name:     "invitations.cleanup",
		Interval: m.cleanupInterval,
		Run: func(ctx context.Context) (int64, error) {
			n1, err := drainCleanupBatches(ctx, cleanupBatchLimit, maxCleanupBatchesPerTick, func(ctx context.Context, limit int) (int64, error) {
				return m.store.CleanupExpiredInvitationsByExpiry(ctx, m.invitationRetention, limit)
			})
			if err != nil {
				return n1, err
			}
			n2, err := drainCleanupBatches(ctx, cleanupBatchLimit, maxCleanupBatchesPerTick, func(ctx context.Context, limit int) (int64, error) {
				return m.store.CleanupRevokedInvitations(ctx, m.invitationRetention, limit)
			})
			if err != nil {
				return n1 + n2, err
			}
			n3, err := drainCleanupBatches(ctx, cleanupBatchLimit, maxCleanupBatchesPerTick, func(ctx context.Context, limit int) (int64, error) {
				return m.store.CleanupSupersededInvitations(ctx, m.invitationRetention, limit)
			})
			return n1 + n2 + n3, err
		},
	})
	r.Periodic(job.PeriodicTask{
		Name:     "audit_partitions.maintain",
		Interval: m.cleanupInterval,
		Run: func(ctx context.Context) (int64, error) {
			now := time.Now().UTC()
			if _, err := m.store.EnsureAuditPartitions(ctx, now, partitionMonthsAhead); err != nil {
				return 0, err
			}
			return m.store.DropExpiredAuditPartitions(ctx, now, m.auditRetentionDays)
		},
	})
	return nil
}

type riskWorkerModule struct {
	enabled bool
	handler job.Handler
}

func (m riskWorkerModule) Name() string { return "risk_scan" }

func (m riskWorkerModule) RegisterWorker(r WorkerRegistrar) error {
	if !m.enabled {
		r.HandleDisabled(job.KindLinkRiskScan, m.Name())
		return nil
	}
	r.Handle(job.KindLinkRiskScan, m.handler)
	return nil
}

type webhookDeliveryCleaner interface {
	CleanupWebhookDeliveries(ctx context.Context, retention time.Duration, limit int) (int64, error)
}

type webhookWorkerModule struct {
	enabled         bool
	handler         job.Handler
	cleaner         webhookDeliveryCleaner
	cleanupInterval time.Duration
	retention       time.Duration
}

func (m webhookWorkerModule) Name() string { return "webhooks" }

func (m webhookWorkerModule) RegisterWorker(r WorkerRegistrar) error {
	if !m.enabled {
		r.HandleDisabled(job.KindWebhookDeliver, m.Name())
		return nil
	}
	if m.cleaner == nil {
		return errors.New("webhook delivery cleaner is nil")
	}
	r.Handle(job.KindWebhookDeliver, m.handler)
	r.Periodic(job.PeriodicTask{
		Name:     "webhooks.cleanup_deliveries",
		Interval: m.cleanupInterval,
		Run: func(ctx context.Context) (int64, error) {
			return drainCleanupBatches(ctx, cleanupBatchLimit, maxCleanupBatchesPerTick, func(ctx context.Context, limit int) (int64, error) {
				return m.cleaner.CleanupWebhookDeliveries(ctx, m.retention, limit)
			})
		},
	})
	return nil
}

type emailWorkerModule struct {
	enabled bool
	handler job.Handler
}

func (m emailWorkerModule) Name() string { return "email" }

func (m emailWorkerModule) RegisterWorker(r WorkerRegistrar) error {
	if !m.enabled {
		r.HandleDisabled(job.KindInvitationEmail, m.Name())
		return nil
	}
	r.Handle(job.KindInvitationEmail, m.handler)
	return nil
}

type cacheReconciler interface {
	Reconcile(ctx context.Context, code string) error
}

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

type statsMaintenanceStore interface {
	PurgeLinkStatsBatch(ctx context.Context, linkID int64, limit int) (int64, error)
	EnsureStatsPartitions(ctx context.Context, now time.Time, monthsAhead int) (int, error)
	DropExpiredStatsPartitions(ctx context.Context, now time.Time, retentionDays int) (int64, error)
}

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
