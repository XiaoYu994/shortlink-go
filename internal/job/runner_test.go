package job

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"

	"shortlink/internal/logx"
)

type fakeQueue struct {
	mu             sync.Mutex
	jobs           []Job
	nextID         int64
	reaped         []ReapedRecord
	reapErr        error
	ages           map[string]time.Duration
	lastClaimKinds []string

	claimedCount atomic.Int32
	claimCalls   atomic.Int32
}

func newFakeQueue() *fakeQueue {
	return &fakeQueue{
		ages: make(map[string]time.Duration),
	}
}

func (q *fakeQueue) add(j Job) int64 {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.nextID++
	j.ID = q.nextID
	if j.Status == "" {
		j.Status = StatusPending
	}
	if j.MaxAttempts <= 0 {
		j.MaxAttempts = DefaultMaxAttempts
	}
	if j.RunAt.IsZero() {
		j.RunAt = time.Now()
	}
	q.jobs = append(q.jobs, j)
	return j.ID
}

func (q *fakeQueue) get(id int64) (Job, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, j := range q.jobs {
		if j.ID == id {
			return j, true
		}
	}
	return Job{}, false
}

func (q *fakeQueue) Claim(_ context.Context, p ClaimParams) ([]Job, error) {
	q.claimCalls.Add(1)
	q.mu.Lock()
	defer q.mu.Unlock()

	q.lastClaimKinds = slices.Clone(p.Kinds)
	if len(p.Kinds) == 0 {
		return nil, nil
	}
	allowed := make(map[string]struct{}, len(p.Kinds))
	for _, k := range p.Kinds {
		if k != "" {
			allowed[k] = struct{}{}
		}
	}

	now := time.Now()
	var out []Job
	for i := range q.jobs {
		if len(out) >= p.Limit {
			break
		}
		j := &q.jobs[i]
		if j.Status != StatusPending || j.RunAt.After(now) {
			continue
		}
		if _, ok := allowed[j.Kind]; !ok {
			continue
		}
		j.Status = StatusRunning
		j.Attempts++
		j.LockVersion++
		j.LockedBy = p.LockedBy
		lockedAt := now
		lockedUntil := now.Add(p.Lease)
		j.LockedAt = &lockedAt
		j.LockedUntil = &lockedUntil
		out = append(out, *j)
		q.claimedCount.Add(1)
	}
	return out, nil
}

func (q *fakeQueue) Complete(_ context.Context, id int64, lockedBy string, lockVersion int64) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	for i := range q.jobs {
		if q.jobs[i].ID == id && q.jobs[i].Status == StatusRunning &&
			q.jobs[i].LockedBy == lockedBy && q.jobs[i].LockVersion == lockVersion {
			now := time.Now()
			q.jobs[i].Status = StatusDone
			q.jobs[i].LockedBy = ""
			q.jobs[i].LockedAt = nil
			q.jobs[i].LockedUntil = nil
			q.jobs[i].LastError = ""
			q.jobs[i].FinishedAt = &now
			return nil
		}
	}
	return ErrLeaseLost
}

func (q *fakeQueue) Retry(_ context.Context, p RetryParams) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	for i := range q.jobs {
		if q.jobs[i].ID == p.ID && q.jobs[i].Status == StatusRunning &&
			q.jobs[i].LockedBy == p.LockedBy && q.jobs[i].LockVersion == p.LockVersion {
			j := &q.jobs[i]
			j.Status = StatusPending
			if !p.ConsumeAttempt && j.Attempts > 0 {
				j.Attempts--
			}
			j.RunAt = time.Now().Add(p.Delay)
			j.LockedBy = ""
			j.LockedAt = nil
			j.LockedUntil = nil
			j.LastError = p.LastError
			return nil
		}
	}
	return ErrLeaseLost
}

func (q *fakeQueue) Dead(_ context.Context, id int64, lockedBy string, lockVersion int64, lastError string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	for i := range q.jobs {
		if q.jobs[i].ID == id && q.jobs[i].Status == StatusRunning &&
			q.jobs[i].LockedBy == lockedBy && q.jobs[i].LockVersion == lockVersion {
			now := time.Now()
			q.jobs[i].Status = StatusDead
			q.jobs[i].LockedBy = ""
			q.jobs[i].LockedAt = nil
			q.jobs[i].LockedUntil = nil
			q.jobs[i].LastError = lastError
			q.jobs[i].FinishedAt = &now
			return nil
		}
	}
	return ErrLeaseLost
}

func (q *fakeQueue) ReapExpired(_ context.Context, retryDelay time.Duration, limit int) ([]ReapedRecord, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.reapErr != nil {
		return nil, q.reapErr
	}
	if limit <= 0 {
		limit = DefaultReapBatchSize
	}
	now := time.Now()
	var records []ReapedRecord
	for i := range q.jobs {
		if len(records) >= limit {
			break
		}
		j := &q.jobs[i]
		if j.Status != StatusRunning || j.LockedUntil == nil || j.LockedUntil.After(now) {
			continue
		}
		j.LockedAt = nil
		j.LockedUntil = nil
		j.LockedBy = ""
		if j.Attempts >= j.MaxAttempts {
			j.Status = StatusDead
			j.FinishedAt = &now
			j.LastError = "lease expired"
		} else {
			j.Status = StatusPending
			j.RunAt = now.Add(retryDelay)
			j.LastError = "lease expired"
		}
		records = append(records, ReapedRecord{Kind: j.Kind, Status: j.Status})
	}
	q.reaped = append(q.reaped, records...)
	return records, nil
}

func (q *fakeQueue) OldestPendingAges(_ context.Context) (map[string]time.Duration, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make(map[string]time.Duration, len(q.ages))
	for k, v := range q.ages {
		out[k] = v
	}
	return out, nil
}

func TestBackoffBounds(t *testing.T) {
	base := 2 * time.Second
	maxDelay := 10 * time.Second

	tests := []struct {
		attempt int
		wantMin time.Duration
		wantMax time.Duration
	}{
		{attempt: 1, wantMin: 1600 * time.Millisecond, wantMax: 2 * time.Second},
		{attempt: 2, wantMin: 3200 * time.Millisecond, wantMax: 4 * time.Second},
		{attempt: 3, wantMin: 6400 * time.Millisecond, wantMax: 8 * time.Second},
		{attempt: 4, wantMin: 8 * time.Second, wantMax: 10 * time.Second},
		{attempt: 20, wantMin: 8 * time.Second, wantMax: 10 * time.Second},
	}
	for _, tt := range tests {
		got := Backoff(tt.attempt, base, maxDelay)
		if got < tt.wantMin || got > tt.wantMax {
			t.Errorf("Backoff(%d) = %v, want in [%v, %v]", tt.attempt, got, tt.wantMin, tt.wantMax)
		}
	}
}

func TestRunnerLifecycleOutcomes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		q := newFakeQueue()
		idOK := q.add(Job{Kind: "ok"})
		idSkip := q.add(Job{Kind: "skip"})
		idPerm := q.add(Job{Kind: "perm"})
		idPanic := q.add(Job{Kind: "panic", MaxAttempts: 1})
		idUnknown := q.add(Job{Kind: "unknown"})

		reg := prometheus.NewRegistry()
		runner := New(q, map[string]Handler{
			"ok":    func(context.Context, Job) error { return nil },
			"skip":  func(context.Context, Job) error { return Skip("url changed") },
			"perm":  func(context.Context, Job) error { return Permanent(errors.New("bad json")) },
			"panic": func(context.Context, Job) error { panic("boom") },
		}, nil, reg, Options{
			Concurrency:     4,
			PollInterval:    50 * time.Millisecond,
			ReapInterval:    time.Hour,
			MetricsInterval: time.Hour,
		})

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- runner.Run(ctx) }()

		synctest.Wait()
		time.Sleep(100 * time.Millisecond)
		synctest.Wait()

		cancel()
		if err := <-done; err != nil {
			t.Fatalf("Run returned error: %v", err)
		}

		assertStatus := func(id int64, want string) {
			t.Helper()
			j, ok := q.get(id)
			if !ok {
				t.Fatalf("job %d missing", id)
			}
			if j.Status != want {
				t.Errorf("job %d status = %q, want %q", id, j.Status, want)
			}
		}
		assertStatus(idOK, StatusDone)
		assertStatus(idSkip, StatusDone)
		assertStatus(idPerm, StatusDead)
		assertStatus(idPanic, StatusDead)

		// 未注册的任务类型不得被 Claim 抢走或打进 dead，应原样留在 pending。
		jUnknown, ok := q.get(idUnknown)
		if !ok || jUnknown.Status != StatusPending || jUnknown.Attempts != 0 {
			t.Fatalf("unknown job = %+v (ok=%v), want pending with 0 attempts", jUnknown, ok)
		}
		if got := testutil.ToFloat64(runner.processed.WithLabelValues("unknown", resultDead)); got != 0 {
			t.Errorf("processed{unknown,dead} = %v, want 0", got)
		}

		if got := testutil.ToFloat64(runner.processed.WithLabelValues("ok", resultDone)); got != 1 {
			t.Errorf("processed{ok,done} = %v, want 1", got)
		}
		if got := testutil.ToFloat64(runner.processed.WithLabelValues("skip", resultSkipped)); got != 1 {
			t.Errorf("processed{skip,skipped} = %v, want 1", got)
		}
		if got := testutil.ToFloat64(runner.processed.WithLabelValues("perm", resultDead)); got != 1 {
			t.Errorf("processed{perm,dead} = %v, want 1", got)
		}
		if got := testutil.ToFloat64(runner.processed.WithLabelValues("panic", resultDead)); got != 1 {
			t.Errorf("processed{panic,dead} = %v, want 1", got)
		}
	})
}

func TestRunnerRetryBackoffAndDead(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		q := newFakeQueue()
		id := q.add(Job{Kind: "flaky", MaxAttempts: 3})

		var attempts atomic.Int32
		reg := prometheus.NewRegistry()
		runner := New(q, map[string]Handler{
			"flaky": func(context.Context, Job) error {
				attempts.Add(1)
				return errors.New("temporary network error")
			},
		}, nil, reg, Options{
			Concurrency:     1,
			PollInterval:    50 * time.Millisecond,
			BaseBackoff:     time.Second,
			MaxBackoff:      5 * time.Second,
			ReapInterval:    time.Hour,
			MetricsInterval: time.Hour,
		})

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- runner.Run(ctx) }()

		// 第 1 次立即执行 -> 退避约 1s -> 第 2 次 -> 退避约 2s -> 第 3 次进入 dead。
		time.Sleep(5 * time.Second)
		synctest.Wait()

		cancel()
		if err := <-done; err != nil {
			t.Fatalf("Run returned error: %v", err)
		}

		if got := attempts.Load(); got != 3 {
			t.Errorf("attempts = %d, want 3", got)
		}
		j, _ := q.get(id)
		if j.Status != StatusDead {
			t.Errorf("status = %q, want dead", j.Status)
		}
		if got := testutil.ToFloat64(runner.processed.WithLabelValues("flaky", resultRetry)); got != 2 {
			t.Errorf("retry count = %v, want 2", got)
		}
		if got := testutil.ToFloat64(runner.processed.WithLabelValues("flaky", resultDead)); got != 1 {
			t.Errorf("dead count = %v, want 1", got)
		}
	})
}

func TestRunnerDeferDoesNotConsumeAttempt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		q := newFakeQueue()
		id := q.add(Job{Kind: "throttled", MaxAttempts: 1})

		var calls atomic.Int32
		runner := New(q, map[string]Handler{
			"throttled": func(context.Context, Job) error {
				if calls.Add(1) < 3 {
					return Defer(200*time.Millisecond, "host rate limited")
				}
				return nil
			},
		}, nil, prometheus.NewRegistry(), Options{
			Concurrency:     1,
			PollInterval:    50 * time.Millisecond,
			ReapInterval:    time.Hour,
			MetricsInterval: time.Hour,
		})

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- runner.Run(ctx) }()

		time.Sleep(time.Second)
		synctest.Wait()
		cancel()
		if err := <-done; err != nil {
			t.Fatalf("Run returned error: %v", err)
		}

		j, _ := q.get(id)
		if j.Status != StatusDone {
			t.Fatalf("status = %q, want done (MaxAttempts=1 should not dead on Defer)", j.Status)
		}
		if j.Attempts != 1 {
			t.Errorf("attempts = %d, want 1", j.Attempts)
		}
	})
}

func TestRunnerReaperAndPeriodicTasks(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		q := newFakeQueue()
		expired := time.Now().Add(-time.Second)
		q.add(Job{
			Kind:        KindLinkRiskScan,
			Status:      StatusRunning,
			Attempts:    1,
			MaxAttempts: 3,
			LockedAt:    &expired,
			LockedUntil: &expired,
			LockedBy:    "crashed-worker",
		})

		var handled atomic.Int32
		var cleaned atomic.Int32
		reg := prometheus.NewRegistry()
		runner := New(q, map[string]Handler{
			KindLinkRiskScan: func(context.Context, Job) error {
				handled.Add(1)
				return nil
			},
		}, nil, reg, Options{
			Concurrency:     2,
			PollInterval:    100 * time.Millisecond,
			BaseBackoff:     100 * time.Millisecond,
			ReapInterval:    500 * time.Millisecond,
			MetricsInterval: time.Hour,
			PeriodicTasks: []PeriodicTask{
				{
					Name:     "session-cleanup",
					Interval: 400 * time.Millisecond,
					Run: func(context.Context) (int64, error) {
						cleaned.Add(1)
						return 5, nil
					},
				},
			},
		})

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- runner.Run(ctx) }()

		time.Sleep(1500 * time.Millisecond)
		synctest.Wait()
		cancel()
		if err := <-done; err != nil {
			t.Fatalf("Run returned error: %v", err)
		}

		if got := testutil.ToFloat64(runner.reaped.WithLabelValues(KindLinkRiskScan)); got != 1 {
			t.Errorf("reaped = %v, want 1", got)
		}
		if got := handled.Load(); got != 1 {
			t.Errorf("reaped job handled = %d, want 1", got)
		}
		if got := cleaned.Load(); got < 3 {
			t.Errorf("periodic task runs = %d, want >= 3", got)
		}
	})
}

func TestRunnerGracefulShutdownWaitsAndTimesOut(t *testing.T) {
	t.Run("drains in-flight job", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			q := newFakeQueue()
			id := q.add(Job{Kind: "slow"})
			q.add(Job{Kind: "slow"}) // 第二条不应在取消后被抢占

			started := make(chan struct{})
			release := make(chan struct{})
			runner := New(q, map[string]Handler{
				"slow": func(context.Context, Job) error {
					close(started)
					<-release
					return nil
				},
			}, nil, prometheus.NewRegistry(), Options{
				Concurrency:     1,
				PollInterval:    50 * time.Millisecond,
				ShutdownTimeout: 2 * time.Second,
				ReapInterval:    time.Hour,
				MetricsInterval: time.Hour,
			})

			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- runner.Run(ctx) }()

			<-started
			cancel()
			synctest.Wait()

			close(release)
			if err := <-done; err != nil {
				t.Fatalf("Run returned error: %v", err)
			}
			j, _ := q.get(id)
			if j.Status != StatusDone {
				t.Errorf("in-flight job status = %q, want done", j.Status)
			}
			if got := q.claimedCount.Load(); got != 1 {
				t.Errorf("claimedCount = %d, want 1 (should not claim after cancel)", got)
			}
		})
	})

	t.Run("cancels stuck job after shutdown timeout", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			q := newFakeQueue()
			q.add(Job{Kind: "stuck"})

			started := make(chan struct{})
			runner := New(q, map[string]Handler{
				"stuck": func(ctx context.Context, _ Job) error {
					close(started)
					<-ctx.Done()
					return ctx.Err()
				},
			}, nil, prometheus.NewRegistry(), Options{
				Concurrency:     1,
				PollInterval:    50 * time.Millisecond,
				ShutdownTimeout: 300 * time.Millisecond,
				JobTimeout:      time.Minute,
				ReapInterval:    time.Hour,
				MetricsInterval: time.Hour,
			})

			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- runner.Run(ctx) }()

			<-started
			cancel()
			err := <-done
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("Run error = %v, want DeadlineExceeded", err)
			}
		})
	})

	t.Run("shutdown timeout on last attempt does not mark dead", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			q := newFakeQueue()
			id := q.add(Job{Kind: "stuck-last", MaxAttempts: 1})

			started := make(chan struct{})
			runner := New(q, map[string]Handler{
				"stuck-last": func(ctx context.Context, _ Job) error {
					close(started)
					<-ctx.Done()
					return ctx.Err()
				},
			}, nil, prometheus.NewRegistry(), Options{
				Concurrency:     1,
				PollInterval:    50 * time.Millisecond,
				ShutdownTimeout: 300 * time.Millisecond,
				JobTimeout:      time.Minute,
				ReapInterval:    time.Hour,
				MetricsInterval: time.Hour,
			})

			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- runner.Run(ctx) }()

			<-started
			cancel()
			err := <-done
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("Run error = %v, want DeadlineExceeded", err)
			}
			j, ok := q.get(id)
			if !ok {
				t.Fatalf("job %d missing", id)
			}
			if j.Status != StatusPending {
				t.Errorf("status = %q, want pending (must not move to dead on shutdown)", j.Status)
			}
			if j.Attempts != 0 {
				t.Errorf("attempts = %d, want 0 (must not consume attempt on shutdown)", j.Attempts)
			}
		})
	})
}

func TestRunnerStaleLeaseLostDoesNotOverwriteSameWorkerReclaim(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		q := newFakeQueue()
		id := q.add(Job{Kind: "aba", MaxAttempts: 3})

		firstStarted := make(chan struct{})
		releaseFirst := make(chan struct{})
		secondStarted := make(chan struct{})
		releaseSecond := make(chan struct{})
		var calls atomic.Int32

		reg := prometheus.NewRegistry()
		runner := New(q, map[string]Handler{
			"aba": func(context.Context, Job) error {
				switch calls.Add(1) {
				case 1:
					close(firstStarted)
					<-releaseFirst
					return nil
				case 2:
					close(secondStarted)
					<-releaseSecond
					return nil
				default:
					return nil
				}
			},
		}, nil, reg, Options{
			WorkerID:        "same-worker",
			Concurrency:     2,
			PollInterval:    50 * time.Millisecond,
			LeaseDuration:   200 * time.Millisecond,
			JobTimeout:      10 * time.Second,
			ReapInterval:    100 * time.Millisecond,
			BaseBackoff:     50 * time.Millisecond,
			MetricsInterval: time.Hour,
		})

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- runner.Run(ctx) }()

		<-firstStarted
		// 等第一个租约超时被同一进程 Reaper 回收并被同一 WorkerID 再次抢占。
		<-secondStarted

		// 第一个陈旧协程此时返回成功，必须被 Fencing Token 拦下，不能覆盖第二个协程的 running 状态也不能记 done 指标。
		close(releaseFirst)
		synctest.Wait()

		j, _ := q.get(id)
		if j.Status != StatusRunning || j.LockVersion != 2 {
			t.Fatalf("after stale completion: status=%q lockVersion=%d, want running v2", j.Status, j.LockVersion)
		}
		if got := testutil.ToFloat64(runner.processed.WithLabelValues("aba", resultDone)); got != 0 {
			t.Fatalf("processed{done} after stale completion = %v, want 0", got)
		}

		close(releaseSecond)
		synctest.Wait()
		cancel()
		if err := <-done; err != nil {
			t.Fatalf("Run returned error: %v", err)
		}

		j, _ = q.get(id)
		if j.Status != StatusDone || j.LockVersion != 2 {
			t.Fatalf("final job: status=%q lockVersion=%d, want done v2", j.Status, j.LockVersion)
		}
		if got := testutil.ToFloat64(runner.processed.WithLabelValues("aba", resultDone)); got != 1 {
			t.Errorf("final processed{done} = %v, want 1", got)
		}
	})
}

func TestRunnerReaperDrainsAcrossBatches(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		q := newFakeQueue()
		expired := time.Now().Add(-time.Second)
		for range 5 {
			q.add(Job{
				Kind:        KindLinkRiskScan,
				Status:      StatusRunning,
				Attempts:    1,
				MaxAttempts: 3,
				LockedAt:    &expired,
				LockedUntil: &expired,
				LockedBy:    "crashed-worker",
				LockVersion: 1,
			})
		}

		reg := prometheus.NewRegistry()
		runner := New(q, map[string]Handler{}, nil, reg, Options{
			Concurrency:     1,
			PollInterval:    time.Hour,
			ReapInterval:    100 * time.Millisecond,
			ReapBatchSize:   2,
			BaseBackoff:     time.Hour,
			MetricsInterval: time.Hour,
		})

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- runner.Run(ctx) }()

		time.Sleep(150 * time.Millisecond)
		synctest.Wait()
		cancel()
		if err := <-done; err != nil {
			t.Fatalf("Run returned error: %v", err)
		}

		if got := testutil.ToFloat64(runner.reaped.WithLabelValues(KindLinkRiskScan)); got != 5 {
			t.Errorf("reaped = %v, want 5 across batches", got)
		}
	})
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]byte(nil), b.buf.Bytes()...)
}

func TestRunnerInjectsJobContextIntoLogs(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var buf lockedBuffer
		logger, _ := logx.New(&buf, logx.Options{Format: logx.FormatJSON, Level: slog.LevelInfo, Service: "console-worker"})
		downstream := logger.With("component", "verdict_cache")

		q := newFakeQueue()
		tenantID := int64(42)
		jobID := q.add(Job{Kind: KindLinkRiskScan, TenantID: &tenantID, MaxAttempts: 2})

		doneCh := make(chan struct{})
		runner := New(q, map[string]Handler{
			KindLinkRiskScan: func(ctx context.Context, _ Job) error {
				downstream.WarnContext(ctx, "downstream cache miss")
				close(doneCh)
				return errors.New("temporary fetch failure")
			},
		}, logger, prometheus.NewRegistry(), Options{
			Concurrency:     1,
			PollInterval:    20 * time.Millisecond,
			BaseBackoff:     time.Second,
			MetricsInterval: time.Hour,
			ReapInterval:    time.Hour,
		})

		ctx, cancel := context.WithCancel(context.Background())
		errCh := make(chan error, 1)
		go func() { errCh <- runner.Run(ctx) }()

		<-doneCh
		synctest.Wait()
		cancel()
		if err := <-errCh; err != nil {
			t.Fatalf("Run error = %v", err)
		}

		var sawHandler, sawRetry bool
		for _, line := range bytes.Split(bytes.TrimSpace(buf.Bytes()), []byte("\n")) {
			if len(line) == 0 {
				continue
			}
			var entry map[string]any
			if err := json.Unmarshal(line, &entry); err != nil {
				t.Fatalf("decode log: %v\n%s", err, line)
			}
			switch entry["msg"] {
			case "downstream cache miss":
				sawHandler = true
				if entry["job_id"] != float64(jobID) || entry["job_kind"] != KindLinkRiskScan ||
					entry["tenant_id"] != float64(42) || entry["service"] != "console-worker" {
					t.Errorf("handler log = %+v", entry)
				}
			case "job scheduled for retry":
				sawRetry = true
				if entry["job_id"] != float64(jobID) || entry["job_kind"] != KindLinkRiskScan || entry["tenant_id"] != float64(42) {
					t.Errorf("retry log = %+v", entry)
				}
			}
		}
		if !sawHandler || !sawRetry {
			t.Fatalf("sawHandler=%v sawRetry=%v\n%s", sawHandler, sawRetry, string(buf.Bytes()))
		}
	})
}

func TestRunnerRetryAfterOverridesBackoffAndMarksDeadOnExhaustion(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		q := newFakeQueue()
		jobID := q.add(Job{Kind: KindWebhookDeliver, MaxAttempts: 2})

		var attempts atomic.Int32
		customDelay := 12 * time.Second

		runner := New(q, map[string]Handler{
			KindWebhookDeliver: func(_ context.Context, _ Job) error {
				attempts.Add(1)
				return RetryAfter(customDelay, errors.New("upstream 503"))
			},
		}, nil, prometheus.NewRegistry(), Options{
			Concurrency:     1,
			PollInterval:    20 * time.Millisecond,
			BaseBackoff:     time.Second,
			MaxBackoff:      2 * time.Second,
			MetricsInterval: time.Hour,
			ReapInterval:    time.Hour,
		})

		ctx, cancel := context.WithCancel(context.Background())
		errCh := make(chan error, 1)
		go func() { errCh <- runner.Run(ctx) }()

		// 第一次尝试失败后，RunAt 应至少推迟 customDelay（远大于 MaxBackoff=2s）。
		synctest.Wait()
		time.Sleep(100 * time.Millisecond)
		synctest.Wait()

		j1, ok := q.get(jobID)
		if !ok || j1.Status != StatusPending || j1.Attempts != 1 {
			t.Fatalf("after 1st attempt = %+v, want pending attempt 1", j1)
		}
		if remaining := time.Until(j1.RunAt); remaining < 11*time.Second {
			t.Fatalf("remaining backoff = %v, want ~%v", remaining, customDelay)
		}

		// 等待超过 customDelay，第二次尝试也失败，耗尽 MaxAttempts=2 进入 dead。
		time.Sleep(customDelay)
		synctest.Wait()

		j2, _ := q.get(jobID)
		if j2.Status != StatusDead || j2.Attempts != 2 || attempts.Load() != 2 {
			t.Fatalf("after 2nd attempt = %+v (calls=%d), want dead with 2 attempts", j2, attempts.Load())
		}

		cancel()
		if err := <-errCh; err != nil {
			t.Fatalf("Run error = %v", err)
		}
	})
}

func TestRunnerTracingConsumerSpanLinkAndLogCorrelation(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	otel.SetTracerProvider(tp)
	t.Cleanup(func() {
		_ = tp.Shutdown(t.Context())
		otel.SetTracerProvider(noop.NewTracerProvider())
	})

	var buf lockedBuffer
	logger, _ := logx.New(&buf, logx.Options{Format: logx.FormatJSON, Level: slog.LevelInfo, Service: "console-worker"})

	q := newFakeQueue()
	tenantID := int64(42)
	const sourceTraceparent = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	idDone := q.add(Job{Kind: KindLinkRiskScan, TenantID: &tenantID, Traceparent: sourceTraceparent})
	idRetry := q.add(Job{Kind: KindWebhookDeliver, TenantID: &tenantID, MaxAttempts: 2, Traceparent: "malformed-traceparent"})
	idSkip := q.add(Job{Kind: KindLinkPurgeStats, TenantID: &tenantID})

	var handled atomic.Int32
	allHandled := make(chan struct{})
	var closeOnce sync.Once
	markHandled := func() {
		if handled.Add(1) == 3 {
			closeOnce.Do(func() { close(allHandled) })
		}
	}

	runner := New(q, map[string]Handler{
		KindLinkRiskScan: func(ctx context.Context, _ Job) error {
			logger.InfoContext(ctx, "job running")
			markHandled()
			return nil
		},
		KindWebhookDeliver: func(ctx context.Context, _ Job) error {
			markHandled()
			return errors.New("upstream 500")
		},
		KindLinkPurgeStats: func(ctx context.Context, _ Job) error {
			markHandled()
			return Skip("already purged")
		},
	}, logger, prometheus.NewRegistry(), Options{
		Concurrency:     3,
		PollInterval:    10 * time.Millisecond,
		BaseBackoff:     time.Minute,
		MetricsInterval: time.Hour,
		ReapInterval:    time.Hour,
	})

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- runner.Run(ctx) }()

	<-allHandled
	// 等待队列状态写回完成再停机。
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		j1, _ := q.get(idDone)
		j2, _ := q.get(idRetry)
		j3, _ := q.get(idSkip)
		if j1.Status == StatusDone && j2.Status == StatusPending && j3.Status == StatusDone {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	if err := <-errCh; err != nil {
		t.Fatalf("Run error = %v", err)
	}

	spans := exp.GetSpans()
	byName := make(map[string]tracetest.SpanStub, len(spans))
	for _, sp := range spans {
		byName[sp.Name] = sp
	}

	doneSpan, ok := byName["job.process "+KindLinkRiskScan]
	if !ok {
		t.Fatalf("missing done span in %+v", spans)
	}
	if doneSpan.SpanKind != trace.SpanKindConsumer {
		t.Errorf("span kind = %v, want consumer", doneSpan.SpanKind)
	}
	if doneSpan.Parent.IsValid() {
		t.Errorf("consumer span should start a new root, got parent %+v", doneSpan.Parent)
	}
	if doneSpan.SpanContext.TraceID().String() == "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Error("consumer span reused producer trace_id instead of new root + link")
	}
	if len(doneSpan.Links) != 1 ||
		doneSpan.Links[0].SpanContext.TraceID().String() != "4bf92f3577b34da6a3ce929d0e0e4736" ||
		doneSpan.Links[0].SpanContext.SpanID().String() != "00f067aa0ba902b7" {
		t.Fatalf("done span links = %+v, want source traceparent link", doneSpan.Links)
	}
	if doneSpan.Status.Code != codes.Unset {
		t.Errorf("done span status = %v, want Unset", doneSpan.Status.Code)
	}
	doneAttrs := jobAttrMap(doneSpan.Attributes)
	if doneAttrs["shortlink.job.result"] != resultDone ||
		doneAttrs["shortlink.job.source_trace_id"] != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Errorf("done span attrs = %+v", doneAttrs)
	}

	retrySpan, ok := byName["job.process "+KindWebhookDeliver]
	if !ok {
		t.Fatalf("missing retry span in %+v", spans)
	}
	if len(retrySpan.Links) != 0 {
		t.Errorf("malformed traceparent should produce 0 links, got %+v", retrySpan.Links)
	}
	if retrySpan.Status.Code != codes.Error {
		t.Errorf("retry span status = %v, want Error", retrySpan.Status.Code)
	}

	skipSpan, ok := byName["job.process "+KindLinkPurgeStats]
	if !ok {
		t.Fatalf("missing skip span in %+v", spans)
	}
	if skipSpan.Status.Code != codes.Unset || jobAttrMap(skipSpan.Attributes)["shortlink.job.result"] != resultSkipped {
		t.Errorf("skip span = (%v, %+v)", skipSpan.Status.Code, skipSpan.Attributes)
	}

	var sawJobLog, sawRetryLog bool
	for _, line := range bytes.Split(bytes.TrimSpace(buf.Bytes()), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal(line, &entry); err != nil {
			t.Fatalf("decode log: %v", err)
		}
		switch entry["msg"] {
		case "job running":
			sawJobLog = true
			if entry["trace_id"] != doneSpan.SpanContext.TraceID().String() ||
				entry["span_id"] != doneSpan.SpanContext.SpanID().String() {
				t.Errorf("handler log trace mismatch: %+v vs %s/%s",
					entry, doneSpan.SpanContext.TraceID(), doneSpan.SpanContext.SpanID())
			}
		case "job scheduled for retry":
			sawRetryLog = true
			if entry["trace_id"] != retrySpan.SpanContext.TraceID().String() {
				t.Errorf("retry log trace_id = %v, want %s", entry["trace_id"], retrySpan.SpanContext.TraceID())
			}
		}
	}
	if !sawJobLog || !sawRetryLog {
		t.Fatalf("sawJobLog=%v sawRetryLog=%v", sawJobLog, sawRetryLog)
	}
}

type misbehavingKindQueue struct {
	mu      sync.Mutex
	claimed bool
	retried int
	dead    int
}

func (q *misbehavingKindQueue) Claim(_ context.Context, _ ClaimParams) ([]Job, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.claimed {
		return nil, nil
	}
	q.claimed = true
	return []Job{{
		ID:          999,
		Kind:        "unexpected.new_kind",
		Status:      StatusRunning,
		Attempts:    1,
		MaxAttempts: 5,
		LockVersion: 1,
	}}, nil
}

func (q *misbehavingKindQueue) Complete(context.Context, int64, string, int64) error { return nil }

func (q *misbehavingKindQueue) Retry(_ context.Context, _ RetryParams) error {
	q.mu.Lock()
	q.retried++
	q.mu.Unlock()
	return nil
}

func (q *misbehavingKindQueue) Dead(_ context.Context, _ int64, _ string, _ int64, _ string) error {
	q.mu.Lock()
	q.dead++
	q.mu.Unlock()
	return nil
}

func (q *misbehavingKindQueue) ReapExpired(context.Context, time.Duration, int) ([]ReapedRecord, error) {
	return nil, nil
}

func (q *misbehavingKindQueue) OldestPendingAges(context.Context) (map[string]time.Duration, error) {
	return nil, nil
}

func TestRunnerKindFilteringRollingUpgradeAndDefenseInDepth(t *testing.T) {
	t.Run("rolling upgrade isolates unknown kind until new worker starts", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			q := newFakeQueue()
			idOld := q.add(Job{Kind: "old.kind"})
			// 即便新任务 run_at 更早，旧 worker 也绝不能抢占。
			idNew := q.add(Job{Kind: "export.csv", RunAt: time.Now().Add(-time.Hour)})

			oldRunner := New(q, map[string]Handler{
				"old.kind": func(context.Context, Job) error { return nil },
			}, nil, prometheus.NewRegistry(), Options{
				Concurrency:     2,
				PollInterval:    20 * time.Millisecond,
				ReapInterval:    time.Hour,
				MetricsInterval: time.Hour,
			})

			ctx1, cancel1 := context.WithCancel(context.Background())
			done1 := make(chan error, 1)
			go func() { done1 <- oldRunner.Run(ctx1) }()

			synctest.Wait()
			time.Sleep(80 * time.Millisecond)
			synctest.Wait()
			cancel1()
			if err := <-done1; err != nil {
				t.Fatalf("oldRunner.Run: %v", err)
			}

			jOld, _ := q.get(idOld)
			if jOld.Status != StatusDone {
				t.Fatalf("old job status = %q, want done", jOld.Status)
			}
			jNewBefore, _ := q.get(idNew)
			if jNewBefore.Status != StatusPending || jNewBefore.Attempts != 0 || jNewBefore.LockVersion != 0 {
				t.Fatalf("new job after old worker = %+v, want untouched pending", jNewBefore)
			}

			// 启动新版 worker（注册了 export.csv），新任务被正常消费完成。
			newRunner := New(q, map[string]Handler{
				"old.kind":   func(context.Context, Job) error { return nil },
				"export.csv": func(context.Context, Job) error { return nil },
			}, nil, prometheus.NewRegistry(), Options{
				Concurrency:     2,
				PollInterval:    20 * time.Millisecond,
				ReapInterval:    time.Hour,
				MetricsInterval: time.Hour,
			})

			ctx2, cancel2 := context.WithCancel(context.Background())
			done2 := make(chan error, 1)
			go func() { done2 <- newRunner.Run(ctx2) }()

			synctest.Wait()
			time.Sleep(80 * time.Millisecond)
			synctest.Wait()
			cancel2()
			if err := <-done2; err != nil {
				t.Fatalf("newRunner.Run: %v", err)
			}

			jNewAfter, _ := q.get(idNew)
			if jNewAfter.Status != StatusDone {
				t.Fatalf("new job after new worker = %+v, want done", jNewAfter)
			}
		})
	})

	t.Run("empty handler map makes zero claim calls", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			q := newFakeQueue()
			id := q.add(Job{Kind: "export.csv"})

			emptyRunner := New(q, map[string]Handler{}, nil, prometheus.NewRegistry(), Options{
				Concurrency:     2,
				PollInterval:    10 * time.Millisecond,
				ReapInterval:    time.Hour,
				MetricsInterval: time.Hour,
			})

			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- emptyRunner.Run(ctx) }()

			time.Sleep(60 * time.Millisecond)
			synctest.Wait()
			cancel()
			if err := <-done; err != nil {
				t.Fatalf("emptyRunner.Run: %v", err)
			}

			if calls := q.claimCalls.Load(); calls != 0 {
				t.Fatalf("claimCalls = %d, want 0 when no handlers registered", calls)
			}
			j, _ := q.get(id)
			if j.Status != StatusPending {
				t.Fatalf("pending job status = %q, want pending", j.Status)
			}
		})
	})

	t.Run("unfiltered queue unknown kind schedules retry instead of immediate dead", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			q := &misbehavingKindQueue{}
			reg := prometheus.NewRegistry()
			runner := New(q, map[string]Handler{
				"known.kind": func(context.Context, Job) error { return nil },
			}, nil, reg, Options{
				Concurrency:     1,
				PollInterval:    20 * time.Millisecond,
				BaseBackoff:     time.Second,
				ReapInterval:    time.Hour,
				MetricsInterval: time.Hour,
			})

			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- runner.Run(ctx) }()

			synctest.Wait()
			time.Sleep(50 * time.Millisecond)
			synctest.Wait()
			cancel()
			if err := <-done; err != nil {
				t.Fatalf("runner.Run: %v", err)
			}

			q.mu.Lock()
			retried, dead := q.retried, q.dead
			q.mu.Unlock()
			if retried != 1 || dead != 0 {
				t.Fatalf("retried=%d dead=%d, want retried=1 dead=0", retried, dead)
			}
			if got := testutil.ToFloat64(runner.processed.WithLabelValues("unexpected.new_kind", resultRetry)); got != 1 {
				t.Errorf("processed{retry} = %v, want 1", got)
			}
			if got := testutil.ToFloat64(runner.processed.WithLabelValues("unexpected.new_kind", resultDead)); got != 0 {
				t.Errorf("processed{dead} = %v, want 0", got)
			}
		})
	})
}

func jobAttrMap(attrs []attribute.KeyValue) map[string]any {
	out := make(map[string]any, len(attrs))
	for _, a := range attrs {
		out[string(a.Key)] = a.Value.AsInterface()
	}
	return out
}
