package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"shortlink/internal/job"
	"shortlink/internal/logx"
)

const featureDisabledSkippedMetricName = "shortlink_jobs_feature_disabled_skipped_total"

/*
WorkerRegistrar 向各后台模块提供受控的注册入口，用于声明任务处理器、周期维护任务与启动钩子（ADR 0029）。
*/
type WorkerRegistrar interface {
	Handle(kind string, h job.Handler)
	HandleDisabled(kind, featureName string)
	Periodic(task job.PeriodicTask)
	OnStart(name string, fn func(context.Context) error)
}

/*
WorkerModule 把一个后台特性的任务处理器、周期维护任务与启动期初始化打包成独立模块。
关闭某特性时，模块仍通过 HandleDisabled 为该特性的历史残留任务类型注册兜底处理器，
让抢占后的任务记日志并按 job.Skip 干净收尾，既不修改 claim SQL 索引路径，也不污染死信队列。
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

type disabledKind struct {
	kind    string
	feature string
}

type workerRegistry struct {
	logger          *slog.Logger
	handlers        map[string]job.Handler
	periodic        []job.PeriodicTask
	taskSet         map[string]struct{}
	startup         []workerStartupHook
	startupSet      map[string]struct{}
	disabled        []disabledKind
	disabledSkipped *prometheus.CounterVec
	err             error
}

func validateUniqueEntry[V any](set map[string]V, label, key string, isNil bool) error {
	if key == "" {
		return fmt.Errorf("worker module registered empty %s", label)
	}
	if isNil {
		return fmt.Errorf("worker module registered nil %s %q", label, key)
	}
	if _, dup := set[key]; dup {
		return fmt.Errorf("duplicate worker %s %q", label, key)
	}
	return nil
}

func (r *workerRegistry) Handle(kind string, h job.Handler) {
	if r.err != nil {
		return
	}
	if err := validateUniqueEntry(r.handlers, "handler for job kind", kind, h == nil); err != nil {
		r.err = err
		return
	}
	r.handlers[kind] = h
}

func (r *workerRegistry) HandleDisabled(kind, featureName string) {
	if r.err != nil {
		return
	}
	if featureName == "" {
		r.err = fmt.Errorf("worker module registered empty feature name for disabled job kind %q", kind)
		return
	}
	r.Handle(kind, disabledSkipHandler(r.logger, r.disabledSkipped, kind, featureName))
	if r.err == nil {
		r.disabled = append(r.disabled, disabledKind{kind: kind, feature: featureName})
	}
}

func (r *workerRegistry) Periodic(task job.PeriodicTask) {
	if r.err != nil {
		return
	}
	if err := validateUniqueEntry(r.taskSet, "periodic task", task.Name, task.Run == nil); err != nil {
		r.err = err
		return
	}
	r.taskSet[task.Name] = struct{}{}
	r.periodic = append(r.periodic, task)
}

func (r *workerRegistry) OnStart(name string, fn func(context.Context) error) {
	if r.err != nil {
		return
	}
	if err := validateUniqueEntry(r.startupSet, "startup hook", name, fn == nil); err != nil {
		r.err = err
		return
	}
	r.startupSet[name] = struct{}{}
	r.startup = append(r.startup, workerStartupHook{name: name, run: fn})
}

// assembleWorkerModules 按注册顺序收集全部模块的 Handler、PeriodicTask 与启动钩子，并校验无重复声明与空依赖。
func assembleWorkerModules(logger *slog.Logger, registry *prometheus.Registry, modules ...WorkerModule) (assembledWorker, error) {
	if logger != nil {
		logger = slog.New(logx.WrapContext(logger.Handler()))
	}
	disabledSkipped := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: featureDisabledSkippedMetricName,
		Help: "Total number of background jobs skipped because their owning feature is disabled.",
	}, []string{"kind", "feature"})
	reg := &workerRegistry{
		logger:          logger,
		handlers:        make(map[string]job.Handler),
		taskSet:         make(map[string]struct{}),
		startupSet:      make(map[string]struct{}),
		disabledSkipped: disabledSkipped,
	}
	seenMod := make(map[string]struct{}, len(modules))
	for _, m := range modules {
		if m == nil {
			continue
		}
		name := m.Name()
		if name == "" {
			return assembledWorker{}, errors.New("worker module name must not be empty")
		}
		if _, dup := seenMod[name]; dup {
			return assembledWorker{}, fmt.Errorf("duplicate worker module %q", name)
		}
		seenMod[name] = struct{}{}
		if err := m.RegisterWorker(reg); err != nil {
			return assembledWorker{}, fmt.Errorf("worker module %q: %w", name, err)
		}
		if reg.err != nil {
			return assembledWorker{}, fmt.Errorf("worker module %q: %w", name, reg.err)
		}
	}
	// 所有模块校验通过后再预热关闭特性的零值序列并注册指标，避免失败路径污染 registry。
	for _, d := range reg.disabled {
		disabledSkipped.WithLabelValues(d.kind, d.feature)
	}
	if registry != nil {
		if err := registry.Register(disabledSkipped); err != nil {
			return assembledWorker{}, fmt.Errorf("register %s metric: %w", featureDisabledSkippedMetricName, err)
		}
	}
	return assembledWorker{
		handlers: reg.handlers,
		periodic: reg.periodic,
		startup:  reg.startup,
	}, nil
}

/*
disabledSkipHandler 为已关闭内置特性的历史/滚动发布期残留任务提供兜底处理器（ADR 0029）。
记录一条信息日志与禁用跳过指标后返回 job.Skip 而非未注册错误，使任务按 skipped 归档为 done，避免打入 dead 触发误告警。
*/
func disabledSkipHandler(logger *slog.Logger, counter *prometheus.CounterVec, kind, featureName string) job.Handler {
	reason := "feature disabled: " + featureName
	return func(ctx context.Context, _ job.Job) error {
		if counter != nil {
			counter.WithLabelValues(kind, featureName).Inc()
		}
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
