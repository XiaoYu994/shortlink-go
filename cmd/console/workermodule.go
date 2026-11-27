package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"shortlink/internal/job"
	"shortlink/internal/logx"
)

/*
WorkerRegistrar 向各后台模块提供受控的注册入口，用于声明任务处理器、周期维护任务与启动钩子（ADR 0029、ADR 0031）。
*/
type WorkerRegistrar interface {
	Handle(kind string, h job.Handler)
	HandleDisabled(kind, featureName string)
	Periodic(task job.PeriodicTask)
	OnStart(name string, fn func(context.Context) error)
}

/*
WorkerModule 把一个后台特性的任务处理器、周期维护任务与启动期初始化打包成独立模块。
与抢占期的 ClaimParams.Kinds 白名单隔离分工互补：
  - 滚动发布新增未注册任务类型（如新版本新加 kind）时，旧版 worker 靠 ClaimParams.Kinds 根本不抢，避免误杀进 dead；
  - 明确关闭已知特性的开关时，模块仍通过 HandleDisabled 注册兜底处理器，把历史残留任务记日志并按 job.Skip 排空收尾，避免永久拉高积压告警指标。
*/
type WorkerModule interface {
	Name() string
	RegisterWorker(r WorkerRegistrar) error
}

type workerStartupHook struct {
	name string
	run  func(context.Context) error
}

type assembledWorker struct {
	handlers map[string]job.Handler
	periodic []job.PeriodicTask
	startup  []workerStartupHook
}

func (w assembledWorker) runStartup(ctx context.Context) error {
	for _, hook := range w.startup {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("startup %s: %w", hook.name, err)
		}
		if err := hook.run(ctx); err != nil {
			return fmt.Errorf("startup %s: %w", hook.name, err)
		}
	}
	return nil
}

type workerRegistry struct {
	logger   *slog.Logger
	handlers map[string]job.Handler
	periodic []job.PeriodicTask
	startup  []workerStartupHook
}

func (r *workerRegistry) Handle(kind string, h job.Handler) {
	r.handlers[kind] = h
}

func (r *workerRegistry) HandleDisabled(kind, featureName string) {
	r.handlers[kind] = disabledSkipHandler(r.logger, featureName)
}

func (r *workerRegistry) Periodic(task job.PeriodicTask) {
	r.periodic = append(r.periodic, task)
}

func (r *workerRegistry) OnStart(name string, fn func(context.Context) error) {
	r.startup = append(r.startup, workerStartupHook{name: name, run: fn})
}

// assembleWorkerModules 按注册顺序收集同仓固定模块的 Handler、PeriodicTask 与启动钩子（ADR 0031）。
func assembleWorkerModules(logger *slog.Logger, modules ...WorkerModule) (assembledWorker, error) {
	if logger != nil {
		logger = slog.New(logx.WrapContext(logger.Handler()))
	}
	reg := &workerRegistry{
		logger:   logger,
		handlers: make(map[string]job.Handler),
	}
	for _, m := range modules {
		if m == nil {
			continue
		}
		if err := m.RegisterWorker(reg); err != nil {
			return assembledWorker{}, fmt.Errorf("worker module %q: %w", m.Name(), err)
		}
	}
	return assembledWorker{
		handlers: reg.handlers,
		periodic: reg.periodic,
		startup:  reg.startup,
	}, nil
}

/*
disabledSkipHandler 为已关闭内置特性的历史/滚动发布期残留任务提供兜底处理器（ADR 0029、ADR 0031）。
记一条信息日志后返回 job.Skip，由 job.Runner 按 result="skipped" 归档为 done，避免打入 dead 触发误告警。
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

type retentionCleanupFunc func(ctx context.Context, retention time.Duration, limit int) (int64, error)

func drainRetentionBatches(ctx context.Context, retention time.Duration, fn retentionCleanupFunc) (int64, error) {
	return drainCleanupBatches(ctx, cleanupBatchLimit, maxCleanupBatchesPerTick, func(ctx context.Context, limit int) (int64, error) {
		return fn(ctx, retention, limit)
	})
}

func cleanupBatchTask(name string, interval, retention time.Duration, fn retentionCleanupFunc) job.PeriodicTask {
	return job.PeriodicTask{
		Name:     name,
		Interval: interval,
		Run: func(ctx context.Context) (int64, error) {
			return drainRetentionBatches(ctx, retention, fn)
		},
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
	r.OnStart("audit_partitions.ensure", func(ctx context.Context) error {
		_, err := m.store.EnsureAuditPartitions(ctx, time.Now().UTC(), partitionMonthsAhead)
		return err
	})
	r.Periodic(cleanupBatchTask("sessions.cleanup_expired", m.cleanupInterval, m.sessionRetention, m.store.CleanupExpiredSessionsByExpiry))
	r.Periodic(cleanupBatchTask("sessions.cleanup_revoked", m.cleanupInterval, m.sessionRetention, m.store.CleanupRevokedSessions))
	r.Periodic(cleanupBatchTask("jobs.cleanup_done", m.cleanupInterval, m.doneJobRetention, m.store.CleanupDoneJobs))
	r.Periodic(cleanupBatchTask("jobs.cleanup_dead", m.cleanupInterval, m.deadJobRetention, m.store.CleanupDeadJobs))
	r.Periodic(job.PeriodicTask{
		Name:     "invitations.cleanup",
		Interval: m.cleanupInterval,
		Run: func(ctx context.Context) (int64, error) {
			var total int64
			for _, fn := range []retentionCleanupFunc{
				m.store.CleanupExpiredInvitationsByExpiry,
				m.store.CleanupRevokedInvitations,
				m.store.CleanupSupersededInvitations,
			} {
				n, err := drainRetentionBatches(ctx, m.invitationRetention, fn)
				total += n
				if err != nil {
					return total, err
				}
			}
			return total, nil
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
