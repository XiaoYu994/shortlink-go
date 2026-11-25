package job

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"shortlink/internal/logx"
)

type fakeQueue struct {
	mu      sync.Mutex
	jobs    []Job
	nextID  int64
	reaped  []ReapedRecord
	reapErr error
	ages    map[string]time.Duration

	claimedCount atomic.Int32
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
	q.mu.Lock()
	defer q.mu.Unlock()

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
		assertStatus(idUnknown, StatusDead)

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
