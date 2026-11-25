package job

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"runtime/debug"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"shortlink/internal/logx"
)

const (
	DefaultConcurrency     = 8
	DefaultMaxPerTenant    = 4
	DefaultPollInterval    = 200 * time.Millisecond
	DefaultLeaseDuration   = 30 * time.Second
	DefaultJobTimeout      = 20 * time.Second
	DefaultReapInterval    = 15 * time.Second
	DefaultBaseBackoff     = 2 * time.Second
	DefaultMaxBackoff      = 5 * time.Minute
	DefaultShutdownTimeout = 10 * time.Second
	DefaultMetricsInterval = 5 * time.Second

	processedMetricName  = "shortlink_jobs_processed_total"
	durationMetricName   = "shortlink_job_duration_seconds"
	inFlightMetricName   = "shortlink_jobs_in_flight"
	reapedMetricName     = "shortlink_jobs_reaped_total"
	oldestPendingAgeName = "shortlink_jobs_oldest_pending_age_seconds"

	resultDone    = "done"
	resultSkipped = "skipped"
	resultRetry   = "retry"
	resultDead    = "dead"

	// CommitTimeout 是任务执行结束后写回队列状态的数据库超时上限；配置校验需保证其与 JobTimeout 之和不超过 LeaseDuration。
	CommitTimeout         = 5 * time.Second
	reapQueryLimit        = 5 * time.Second
	maxReapBatchesPerTick = 20
	periodicTimeout       = 30 * time.Second
)

// 覆盖 1ms 到约 65s，适配空 handler 到 5s+ 外部抓取任务耗时。
var jobDurationBuckets = prometheus.ExponentialBuckets(0.001, 2, 17)

/*
Options 控制 Runner 的并发、轮询、租约、退避和周期维护任务。
多数零值字段会填入默认值；MaxPerTenant 为 0 表示不限制单租户并发（LoadWorkerConfig 默认设为 DefaultMaxPerTenant）。
*/
type Options struct {
	WorkerID        string
	Concurrency     int
	MaxPerTenant    int
	PollInterval    time.Duration
	LeaseDuration   time.Duration
	JobTimeout      time.Duration
	ReapInterval    time.Duration
	ReapBatchSize   int
	BaseBackoff     time.Duration
	MaxBackoff      time.Duration
	ShutdownTimeout time.Duration
	MetricsInterval time.Duration
	PeriodicTasks   []PeriodicTask
}

func (o Options) withDefaults() Options {
	if o.WorkerID == "" {
		o.WorkerID = defaultWorkerID()
	}
	if o.Concurrency <= 0 {
		o.Concurrency = DefaultConcurrency
	}
	if o.MaxPerTenant < 0 {
		o.MaxPerTenant = 0
	}
	if o.PollInterval <= 0 {
		o.PollInterval = DefaultPollInterval
	}
	if o.LeaseDuration <= 0 {
		o.LeaseDuration = DefaultLeaseDuration
	}
	if o.JobTimeout <= 0 {
		o.JobTimeout = min(DefaultJobTimeout, o.LeaseDuration)
	}
	if o.ReapInterval <= 0 {
		o.ReapInterval = DefaultReapInterval
	}
	if o.ReapBatchSize <= 0 {
		o.ReapBatchSize = DefaultReapBatchSize
	}
	if o.BaseBackoff <= 0 {
		o.BaseBackoff = DefaultBaseBackoff
	}
	if o.MaxBackoff <= 0 {
		o.MaxBackoff = DefaultMaxBackoff
	}
	if o.MaxBackoff < o.BaseBackoff {
		o.MaxBackoff = o.BaseBackoff
	}
	if o.ShutdownTimeout <= 0 {
		o.ShutdownTimeout = DefaultShutdownTimeout
	}
	if o.MetricsInterval <= 0 {
		o.MetricsInterval = DefaultMetricsInterval
	}
	return o
}

func defaultWorkerID() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "worker"
	}
	var suffix [4]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return fmt.Sprintf("%s-%d", host, os.Getpid())
	}
	return fmt.Sprintf("%s-%d-%s", host, os.Getpid(), hex.EncodeToString(suffix[:]))
}

/*
Runner 协调后台任务的抢占、并发执行、租约回收、周期维护与优雅退出。

必须用指针：内部持有并发槽位、指标和同步状态。
抢占前先占用空槽位，确保不会把已抢占的任务积压在内存通道里白白消耗租约。
*/
type Runner struct {
	queue    Queue
	handlers map[string]Handler
	logger   *slog.Logger
	opts     Options

	processed     *prometheus.CounterVec
	duration      *prometheus.HistogramVec
	inFlight      *prometheus.GaugeVec
	reaped        *prometheus.CounterVec
	oldestPending *prometheus.GaugeVec

	mu        sync.Mutex
	seenKinds map[string]struct{}
}

// New 构造 Runner 并将任务指标注册到 registry。registry 为 nil 时跳过注册。
func New(queue Queue, handlers map[string]Handler, logger *slog.Logger, registry *prometheus.Registry, opts Options) *Runner {
	opts = opts.withDefaults()
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	} else {
		logger = slog.New(logx.WrapContext(logger.Handler()))
	}
	copiedHandlers := make(map[string]Handler, len(handlers))
	seen := make(map[string]struct{}, len(handlers))
	for kind, h := range handlers {
		copiedHandlers[kind] = h
		seen[kind] = struct{}{}
	}

	r := &Runner{
		queue:     queue,
		handlers:  copiedHandlers,
		logger:    logger,
		opts:      opts,
		seenKinds: seen,
		processed: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: processedMetricName,
			Help: "Total number of background jobs processed by kind and result.",
		}, []string{"kind", "result"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    durationMetricName,
			Help:    "Background job execution duration in seconds by kind.",
			Buckets: jobDurationBuckets,
		}, []string{"kind"}),
		inFlight: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: inFlightMetricName,
			Help: "Current number of in-flight background jobs by kind.",
		}, []string{"kind"}),
		reaped: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: reapedMetricName,
			Help: "Total number of expired background job leases reaped by kind.",
		}, []string{"kind"}),
		oldestPending: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: oldestPendingAgeName,
			Help: "Age in seconds of the oldest pending background job by kind.",
		}, []string{"kind"}),
	}

	// 预热已知 kind 的零值序列，启动后 /metrics 就能直接看到。
	for kind := range copiedHandlers {
		r.inFlight.WithLabelValues(kind).Set(0)
		r.oldestPending.WithLabelValues(kind).Set(0)
		r.reaped.WithLabelValues(kind)
	}
	if registry != nil {
		registry.MustRegister(r.processed, r.duration, r.inFlight, r.reaped, r.oldestPending)
	}
	return r
}

/*
Run 启动轮询抢占、租约回收、指标采样与周期维护任务，直到 ctx 取消并排空在途任务。

ctx 取消后不再抢占新任务，给在途任务最多 ShutdownTimeout 完成执行并写回状态；
超时后取消在途任务的 context 并等待全部 goroutine 退出再返回，确保调用方随后关闭
数据库连接池时不会打断正在收尾的协程。
*/
func (r *Runner) Run(ctx context.Context) error {
	r.logger.Info("worker started",
		"worker_id", r.opts.WorkerID,
		"concurrency", r.opts.Concurrency,
		"max_per_tenant", r.opts.MaxPerTenant,
	)

	inFlightCtx, cancelInFlight := context.WithCancel(context.Background())
	defer cancelInFlight()

	var (
		bgWG     sync.WaitGroup
		workerWG sync.WaitGroup
	)

	bgWG.Go(func() { r.reapLoop(ctx) })
	bgWG.Go(func() { r.metricsLoop(ctx) })
	for _, task := range r.opts.PeriodicTasks {
		if task.Run == nil || task.Interval <= 0 {
			continue
		}
		bgWG.Go(func() { r.periodicLoop(ctx, task) })
	}

	// sem 的容量等于最大并发数；只有拿到空槽位才向数据库抢占对应条数。
	sem := make(chan struct{}, r.opts.Concurrency)
	r.pollLoop(ctx, inFlightCtx, sem, &workerWG)

	r.logger.Info("worker stopping, waiting for in-flight jobs", "timeout", r.opts.ShutdownTimeout)
	bgWG.Wait()

	drained := make(chan struct{})
	go func() {
		workerWG.Wait()
		close(drained)
	}()

	timer := time.NewTimer(r.opts.ShutdownTimeout)
	defer timer.Stop()

	var shutdownErr error
	select {
	case <-drained:
	case <-timer.C:
		shutdownErr = fmt.Errorf("worker shutdown: %w", context.DeadlineExceeded)
		r.logger.Warn("worker shutdown timeout reached, canceling in-flight jobs")
		cancelInFlight()
		<-drained
	}

	r.logger.Info("worker stopped", "worker_id", r.opts.WorkerID)
	return shutdownErr
}

func (r *Runner) pollLoop(ctx, inFlightCtx context.Context, sem chan struct{}, wg *sync.WaitGroup) {
	timer := time.NewTimer(0)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}

		slots := acquireAvailable(sem, r.opts.Concurrency)
		if slots == 0 {
			timer.Reset(r.opts.PollInterval)
			continue
		}

		claimCtx, claimCancel := context.WithTimeout(ctx, reapQueryLimit)
		jobs, err := r.queue.Claim(claimCtx, ClaimParams{
			Limit:        slots,
			MaxPerTenant: r.opts.MaxPerTenant,
			Lease:        r.opts.LeaseDuration,
			LockedBy:     r.opts.WorkerID,
		})
		claimCancel()
		if err != nil {
			releaseSlots(sem, slots)
			if ctx.Err() != nil {
				return
			}
			r.logger.Error("claim jobs", "err", err)
			timer.Reset(r.opts.PollInterval)
			continue
		}

		if len(jobs) < slots {
			releaseSlots(sem, slots-len(jobs))
		}
		for _, j := range jobs {
			wg.Go(func() {
				defer func() { <-sem }()
				r.executeJob(inFlightCtx, j)
			})
		}

		// 满批说明队列里可能还有积压，立刻继续抢占；否则按 PollInterval 休眠。
		if len(jobs) == slots {
			timer.Reset(0)
		} else {
			timer.Reset(r.opts.PollInterval)
		}
	}
}

func acquireAvailable(sem chan struct{}, maxSlots int) int {
	n := 0
	for n < maxSlots {
		select {
		case sem <- struct{}{}:
			n++
		default:
			return n
		}
	}
	return n
}

func releaseSlots(sem chan struct{}, n int) {
	for range n {
		<-sem
	}
}

func (r *Runner) executeJob(parentCtx context.Context, j Job) {
	r.inFlight.WithLabelValues(j.Kind).Inc()
	defer r.inFlight.WithLabelValues(j.Kind).Dec()

	start := time.Now()
	err := r.invokeHandler(parentCtx, j)
	r.duration.WithLabelValues(j.Kind).Observe(time.Since(start).Seconds())

	// 回写结果脱离 inFlightCtx 的普通取消，给数据库短超时收尾，同时保留任务关联字段进日志。
	commitCtx, cancel := context.WithTimeout(logx.WithJob(context.Background(), j.ID, j.Kind, jobTenantID(j)), CommitTimeout)
	defer cancel()

	switch {
	case err == nil:
		if cerr := r.queue.Complete(commitCtx, j.ID, r.opts.WorkerID, j.LockVersion); cerr != nil {
			r.logCommitError(commitCtx, "complete job", j, cerr)
			return
		}
		r.processed.WithLabelValues(j.Kind, resultDone).Inc()

	case IsSkipped(err):
		if cerr := r.queue.Complete(commitCtx, j.ID, r.opts.WorkerID, j.LockVersion); cerr != nil {
			r.logCommitError(commitCtx, "complete skipped job", j, cerr)
			return
		}
		r.processed.WithLabelValues(j.Kind, resultSkipped).Inc()

	case isDeferred(err):
		delay, _ := DeferDelay(err)
		if rerr := r.queue.Retry(commitCtx, RetryParams{
			ID:             j.ID,
			LockedBy:       r.opts.WorkerID,
			LockVersion:    j.LockVersion,
			Delay:          delay,
			LastError:      err.Error(),
			ConsumeAttempt: false,
		}); rerr != nil {
			r.logCommitError(commitCtx, "defer job", j, rerr)
			return
		}
		r.processed.WithLabelValues(j.Kind, resultRetry).Inc()

	case parentCtx.Err() != nil && !IsPermanent(err):
		if rerr := r.queue.Retry(commitCtx, RetryParams{
			ID:             j.ID,
			LockedBy:       r.opts.WorkerID,
			LockVersion:    j.LockVersion,
			Delay:          0,
			LastError:      "worker shutdown canceled in-flight job",
			ConsumeAttempt: false,
		}); rerr != nil {
			r.logCommitError(commitCtx, "requeue job on shutdown", j, rerr)
			return
		}
		r.logger.WarnContext(commitCtx, "job requeued on worker shutdown", "attempts", j.Attempts)
		r.processed.WithLabelValues(j.Kind, resultRetry).Inc()

	case IsPermanent(err) || j.Attempts >= j.MaxAttempts:
		if derr := r.queue.Dead(commitCtx, j.ID, r.opts.WorkerID, j.LockVersion, err.Error()); derr != nil {
			r.logCommitError(commitCtx, "mark job dead", j, derr)
			return
		}
		r.logger.WarnContext(commitCtx, "job moved to dead", "attempts", j.Attempts, "err", err)
		r.processed.WithLabelValues(j.Kind, resultDead).Inc()

	default:
		delay, customDelay := RetryAfterDelay(err)
		if !customDelay {
			delay = Backoff(j.Attempts, r.opts.BaseBackoff, r.opts.MaxBackoff)
		}
		if rerr := r.queue.Retry(commitCtx, RetryParams{
			ID:             j.ID,
			LockedBy:       r.opts.WorkerID,
			LockVersion:    j.LockVersion,
			Delay:          delay,
			LastError:      err.Error(),
			ConsumeAttempt: true,
		}); rerr != nil {
			r.logCommitError(commitCtx, "retry job", j, rerr)
			return
		}
		r.logger.WarnContext(commitCtx, "job scheduled for retry",
			"attempts", j.Attempts, "delay", delay, "err", err)
		r.processed.WithLabelValues(j.Kind, resultRetry).Inc()
	}
}

func (r *Runner) logCommitError(ctx context.Context, op string, j Job, err error) {
	if errors.Is(err, ErrLeaseLost) {
		r.logger.WarnContext(ctx, "job lease lost before commit",
			"op", op, "lock_version", j.LockVersion)
		return
	}
	r.logger.ErrorContext(ctx, op, "err", err)
}

func jobTenantID(j Job) int64 {
	if j.TenantID == nil {
		return 0
	}
	return *j.TenantID
}

func isDeferred(err error) bool {
	_, ok := DeferDelay(err)
	return ok
}

/*
invokeHandler 在独立超时和 recover 保护下执行任务处理器。
handler panic 不会拖垮整个 worker 进程，而是转换成普通错误按重试/死信处理。
*/
func (r *Runner) invokeHandler(parentCtx context.Context, j Job) (err error) {
	handler, ok := r.handlers[j.Kind]
	if !ok {
		return Permanent(fmt.Errorf("unknown job kind %q", j.Kind))
	}

	jobCtx, cancel := context.WithTimeout(logx.WithJob(parentCtx, j.ID, j.Kind, jobTenantID(j)), r.opts.JobTimeout)
	defer cancel()

	defer func() {
		if rec := recover(); rec != nil {
			r.logger.ErrorContext(jobCtx, "job handler panic",
				"panic", rec, "stack", string(debug.Stack()))
			err = fmt.Errorf("handler panic: %v", rec)
		}
	}()
	return handler(jobCtx, j)
}

func (r *Runner) reapLoop(ctx context.Context) {
	ticker := time.NewTicker(r.opts.ReapInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.reapOnce(ctx)
		}
	}
}

func (r *Runner) reapOnce(ctx context.Context) {
	totalReaped := 0
	for range maxReapBatchesPerTick {
		if ctx.Err() != nil {
			return
		}
		qctx, cancel := context.WithTimeout(ctx, reapQueryLimit)
		records, err := r.queue.ReapExpired(qctx, r.opts.BaseBackoff, r.opts.ReapBatchSize)
		cancel()
		if err != nil {
			if ctx.Err() == nil {
				r.logger.Error("reap expired jobs", "err", err)
			}
			return
		}
		for _, rec := range records {
			r.reaped.WithLabelValues(rec.Kind).Inc()
			if rec.Status == StatusDead {
				r.processed.WithLabelValues(rec.Kind, resultDead).Inc()
			}
		}
		totalReaped += len(records)
		if len(records) < r.opts.ReapBatchSize {
			break
		}
	}
	if totalReaped > 0 {
		r.logger.Warn("reaped expired job leases", "count", totalReaped)
	}
}

func (r *Runner) metricsLoop(ctx context.Context) {
	r.refreshOldestPending(ctx)

	ticker := time.NewTicker(r.opts.MetricsInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.refreshOldestPending(ctx)
		}
	}
}

func (r *Runner) refreshOldestPending(ctx context.Context) {
	qctx, cancel := context.WithTimeout(ctx, reapQueryLimit)
	defer cancel()

	ages, err := r.queue.OldestPendingAges(qctx)
	if err != nil {
		if ctx.Err() == nil {
			r.logger.Warn("refresh oldest pending job age", "err", err)
		}
		return
	}

	r.mu.Lock()
	for kind := range ages {
		r.seenKinds[kind] = struct{}{}
	}
	for kind := range r.seenKinds {
		r.oldestPending.WithLabelValues(kind).Set(ages[kind].Seconds())
	}
	r.mu.Unlock()
}

func (r *Runner) periodicLoop(ctx context.Context, task PeriodicTask) {
	ticker := time.NewTicker(task.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.runPeriodicTask(ctx, task)
		}
	}
}

func (r *Runner) runPeriodicTask(ctx context.Context, task PeriodicTask) {
	tctx, cancel := context.WithTimeout(ctx, periodicTimeout)
	defer cancel()

	defer func() {
		if rec := recover(); rec != nil {
			r.logger.Error("periodic task panic",
				"task", task.Name, "panic", rec, "stack", string(debug.Stack()))
		}
	}()

	n, err := task.Run(tctx)
	if err != nil {
		if ctx.Err() == nil {
			r.logger.Error("periodic task failed", "task", task.Name, "err", err)
		}
		return
	}
	if n > 0 {
		r.logger.Info("periodic task completed", "task", task.Name, "affected", n)
	}
}
