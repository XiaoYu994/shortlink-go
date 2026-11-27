package click

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// record 是大多数测试的简写：只关心链接 ID，元数据用零值。
func record(r *Recorder, id int64) {
	r.Record(Click{LinkID: id})
}

type memStore struct {
	mu     sync.Mutex
	counts map[int64]int64
	last   []Click
	err    error
}

func (s *memStore) RecordClicks(_ context.Context, clicks []Click) error {
	if s.err != nil {
		return s.err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.counts == nil {
		s.counts = make(map[int64]int64)
	}
	for _, c := range clicks {
		s.counts[c.LinkID]++
	}
	s.last = append([]Click(nil), clicks...)
	return nil
}

func (s *memStore) get(id int64) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.counts[id]
}

func (s *memStore) lastClicks() []Click {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Click(nil), s.last...)
}

func TestCloseFlushesPartialBatch(t *testing.T) {
	store := &memStore{}
	reg := prometheus.NewRegistry()
	// 间隔很长、批量也大，这三条只能靠 Close 写出去。
	r := New(store, discardLogger(), reg, Options{Buffer: 8, BatchSize: 10, Interval: time.Hour})

	record(r, 7)
	record(r, 7)
	record(r, 8)
	record(r, 0)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := r.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := store.get(7); got != 2 {
		t.Errorf("clicks[7] = %d, want 2", got)
	}
	if got := store.get(8); got != 1 {
		t.Errorf("clicks[8] = %d, want 1", got)
	}
	if got := gatheredCounter(t, reg, droppedMetricName, nil); got != 0 {
		t.Errorf("dropped = %v, want 0", got)
	}
	if got := gatheredCounter(t, reg, writtenMetricName, map[string]string{"result": resultSuccess}); got != 3 {
		t.Errorf("written success = %v, want 3", got)
	}
}

// TestRecordPreservesMetadata 验证 Recorder 只补 At，不改来源、设备和访客哈希。
func TestRecordPreservesMetadata(t *testing.T) {
	store := &memStore{}
	reg := prometheus.NewRegistry()
	r := New(store, discardLogger(), reg, Options{Buffer: 8, BatchSize: 10, Interval: time.Hour})

	hash := []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	r.Record(Click{
		LinkID:      9,
		Referrer:    "news.ycombinator.com",
		Device:      DeviceDesktop,
		Client:      ClientBrowser,
		VisitorHash: hash,
	})

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := r.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	got := store.lastClicks()
	if len(got) != 1 {
		t.Fatalf("clicks = %d, want 1", len(got))
	}
	if got[0].Referrer != "news.ycombinator.com" || got[0].Device != DeviceDesktop || got[0].Client != ClientBrowser {
		t.Errorf("metadata = %+v, want preserved", got[0])
	}
	if len(got[0].VisitorHash) != 16 || got[0].VisitorHash[0] != 1 {
		t.Errorf("VisitorHash = %v, want preserved", got[0].VisitorHash)
	}
	if got[0].At.IsZero() {
		t.Error("At was not filled in")
	}
}

func TestFlushesWhenBatchFull(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := &memStore{}
		reg := prometheus.NewRegistry()
		r := New(store, discardLogger(), reg, Options{Buffer: 8, BatchSize: 2, Interval: time.Hour})
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if err := r.Close(ctx); err != nil {
				t.Errorf("Close: %v", err)
			}
		})

		record(r, 3)
		record(r, 3)
		// Wait 等后台 worker 取走这两条点击并在同步写完后重新阻塞在 select 上。
		synctest.Wait()

		if got := store.get(3); got != 2 {
			t.Errorf("clicks[3] = %d, want 2", got)
		}
	})
}

func TestCloseReturnsWriteError(t *testing.T) {
	store := &memStore{err: errors.New("db down")}
	reg := prometheus.NewRegistry()
	r := New(store, discardLogger(), reg, Options{Buffer: 8, BatchSize: 10, Interval: time.Hour})
	record(r, 1)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := r.Close(ctx)
	if !errors.Is(err, store.err) {
		t.Fatalf("Close error = %v, want db down", err)
	}
	if got := gatheredCounter(t, reg, writtenMetricName, map[string]string{"result": resultError}); got != 1 {
		t.Errorf("written error = %v, want 1", got)
	}
}

func TestCloseTwice(t *testing.T) {
	reg := prometheus.NewRegistry()
	r := New(&memStore{}, discardLogger(), reg, Options{})

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := r.Close(ctx); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := r.Close(ctx); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

/*
blockingStore 让第一次写停住，worker 就不再从 channel 取数据，
后面的 Record 才会把缓冲填满。
*/
type blockingStore struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *blockingStore) RecordClicks(context.Context, []Click) error {
	s.once.Do(func() { close(s.entered) })
	<-s.release
	return nil
}

func TestRecordDropsWhenBufferFull(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := &blockingStore{
			entered: make(chan struct{}),
			release: make(chan struct{}),
		}
		var releaseOnce sync.Once
		reg := prometheus.NewRegistry()
		r := New(store, discardLogger(), reg, Options{Buffer: 2, BatchSize: 1, Interval: time.Hour})
		t.Cleanup(func() {
			releaseOnce.Do(func() { close(store.release) })
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if err := r.Close(ctx); err != nil {
				t.Errorf("Close: %v", err)
			}
		})

		record(r, 1)
		synctest.Wait()
		select {
		case <-store.entered:
		default:
			t.Fatal("worker did not block in RecordClicks")
		}

		// worker 已经取走 1 并停在写库上，channel 是空的。再放 3 条，缓冲只有 2 个位置。
		record(r, 2)
		record(r, 3)
		record(r, 4)

		if got := gatheredCounter(t, reg, droppedMetricName, nil); got != 1 {
			t.Errorf("dropped = %v, want 1", got)
		}
	})
}

func TestRecordAfterCloseDoesNotPanic(t *testing.T) {
	store := &memStore{}
	reg := prometheus.NewRegistry()
	r := New(store, discardLogger(), reg, Options{Buffer: 4, BatchSize: 10, Interval: time.Hour})

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := r.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	record(r, 1)
	if got := store.get(1); got != 0 {
		t.Errorf("clicks[1] = %d, want 0", got)
	}
	if got := gatheredCounter(t, reg, droppedMetricName, nil); got != 0 {
		t.Errorf("dropped = %v, want 0", got)
	}
}

func TestRecordOverlapsClose(t *testing.T) {
	reg := prometheus.NewRegistry()
	r := New(&memStore{}, discardLogger(), reg, Options{Buffer: 8, BatchSize: 5, Interval: time.Hour})

	started := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		close(started)
		for i := range 200 {
			record(r, int64(i%20+1))
		}
	}()
	<-started

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := r.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	wg.Wait()
}

func TestMetricsRegistered(t *testing.T) {
	reg := prometheus.NewRegistry()
	r := New(&memStore{}, discardLogger(), reg, Options{Buffer: 1, BatchSize: 10, Interval: time.Hour})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := r.Close(ctx); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	if got := gatheredCounter(t, reg, droppedMetricName, nil); got != 0 {
		t.Errorf("dropped = %v, want 0", got)
	}
	if got := gatheredCounter(t, reg, writtenMetricName, map[string]string{"result": resultSuccess}); got != 0 {
		t.Errorf("written success = %v, want 0", got)
	}
	if got := gatheredCounter(t, reg, writtenMetricName, map[string]string{"result": resultError}); got != 0 {
		t.Errorf("written error = %v, want 0", got)
	}
	if got := gatheredGauge(t, reg, stoppedMetricName); got != 0 {
		t.Errorf("%s = %v, want 0", stoppedMetricName, got)
	}
}

func TestFlushesOnInterval(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := &memStore{}
		reg := prometheus.NewRegistry()
		r := New(store, discardLogger(), reg, Options{Buffer: 8, BatchSize: 10, Interval: time.Second})
		t.Cleanup(func() {
			_ = r.Close(context.Background())
		})

		record(r, 5)
		// 差 1ms 到达间隔时，定时刷盘还没有触发。
		synctest.Sleep(time.Second - time.Millisecond)
		if got := store.get(5); got != 0 {
			t.Fatalf("clicks[5] before interval = %d, want 0", got)
		}

		// 补齐最后 1ms，synctest.Sleep 会等到 worker 处理完定时信号并写回后才返回。
		synctest.Sleep(time.Millisecond)
		if got := store.get(5); got != 1 {
			t.Errorf("clicks[5] = %d, want 1", got)
		}
		if got := gatheredCounter(t, reg, writtenMetricName, map[string]string{"result": resultSuccess}); got != 1 {
			t.Errorf("written success = %v, want 1", got)
		}

		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := r.Close(ctx); err != nil {
			t.Fatalf("Close: %v", err)
		}
	})
}

func TestRunningWriteErrorIsMetricOnly(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := &memStore{err: errors.New("db down")}
		reg := prometheus.NewRegistry()
		r := New(store, discardLogger(), reg, Options{Buffer: 4, BatchSize: 1, Interval: time.Hour})
		t.Cleanup(func() {
			_ = r.Close(context.Background())
		})

		record(r, 1)
		synctest.Wait()

		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := r.Close(ctx); err != nil {
			t.Fatalf("Close = %v, want nil", err)
		}
		if got := gatheredCounter(t, reg, writtenMetricName, map[string]string{"result": resultError}); got != 1 {
			t.Errorf("written error = %v, want 1", got)
		}
		if got := gatheredCounter(t, reg, droppedMetricName, nil); got != 0 {
			t.Errorf("dropped = %v, want 0", got)
		}
	})
}

/*
TestDrainFullBatchError 把 worker 挡在第一批上，让后面的满批留在 channel 里。
Close 之后这些满批走排空，写失败要返回给 Close，不能只打日志。
*/
func TestDrainFullBatchError(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		entered := make(chan struct{})
		release := make(chan struct{})
		var releaseOnce sync.Once
		storeErr := errors.New("db down")
		var mu sync.Mutex
		var calls int
		store := &fnStore{fn: func(context.Context, []Click) error {
			mu.Lock()
			calls++
			n := calls
			mu.Unlock()
			if n == 1 {
				close(entered)
				<-release
				return nil
			}
			return storeErr
		}}
		reg := prometheus.NewRegistry()
		r := New(store, discardLogger(), reg, Options{Buffer: 8, BatchSize: 2, Interval: time.Hour})
		t.Cleanup(func() {
			releaseOnce.Do(func() { close(release) })
			_ = r.Close(context.Background())
		})

		record(r, 1)
		record(r, 1)
		synctest.Wait()
		select {
		case <-entered:
		default:
			t.Fatal("worker did not block in first flush")
		}
		for range 4 {
			record(r, 2)
		}

		errCh := make(chan error, 1)
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			errCh <- r.Close(ctx)
		}()
		// Close 持有写锁置位 stopped 并阻塞在 r.done 上，synctest.Wait 确保排空状态已就绪。
		synctest.Wait()
		if r.draining() == nil {
			t.Fatal("Close has not started draining")
		}
		releaseOnce.Do(func() { close(release) })

		err := <-errCh
		if !errors.Is(err, storeErr) {
			t.Fatalf("Close error = %v, want db down", err)
		}
		if got := gatheredCounter(t, reg, writtenMetricName, map[string]string{"result": resultError}); got != 4 {
			t.Errorf("written error = %v, want 4", got)
		}
		if got := gatheredCounter(t, reg, writtenMetricName, map[string]string{"result": resultSuccess}); got != 2 {
			t.Errorf("written success = %v, want 2", got)
		}
	})
}

/*
TestDrainStopsWhenCloseContextEnds 固定排空的时限是 Close 的 ctx。
若满批各自再占一份 writeTimeout，这里会按批数乘上去，耗时将超过 150ms。
*/
func TestDrainStopsWhenCloseContextEnds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		entered := make(chan struct{})
		release := make(chan struct{})
		var releaseOnce sync.Once
		var mu sync.Mutex
		var calls int
		store := &fnStore{fn: func(ctx context.Context, _ []Click) error {
			mu.Lock()
			calls++
			n := calls
			mu.Unlock()
			if n == 1 {
				close(entered)
				<-release
				return nil
			}
			timer := time.NewTimer(5 * time.Second)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-timer.C:
				return nil
			}
		}}
		reg := prometheus.NewRegistry()
		r := New(store, discardLogger(), reg, Options{
			Buffer: 16, BatchSize: 2, Interval: time.Hour, WriteTimeout: 5 * time.Second,
		})
		t.Cleanup(func() {
			releaseOnce.Do(func() { close(release) })
			_ = r.Close(context.Background())
		})

		record(r, 1)
		record(r, 1)
		synctest.Wait()
		select {
		case <-entered:
		default:
			t.Fatal("worker did not block in first flush")
		}
		for range 6 {
			record(r, 2)
		}

		errCh := make(chan error, 1)
		started := time.Now()
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
			defer cancel()
			errCh <- r.Close(ctx)
		}()
		synctest.Wait()
		releaseOnce.Do(func() { close(release) })

		err := <-errCh
		if got := time.Since(started); got != 150*time.Millisecond {
			t.Fatalf("Close took %s, want exact close timeout 150ms", got)
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Close error = %v, want deadline exceeded", err)
		}
	})
}

/*
TestCloseWaitsForCanceledWrite 固定 ctx 到期后 Close 仍等这一批写完。
尾部用的就是 Close 的 ctx，取消会让写入返回，Close 再把这个错误交出去。
*/
func TestCloseWaitsForCanceledWrite(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		finished := make(chan struct{})
		store := &fnStore{fn: func(ctx context.Context, _ []Click) error {
			<-ctx.Done()
			// 模拟存储驱动收到取消后再做一次 10ms 收尾；Close 必须等它彻底返回。
			time.Sleep(10 * time.Millisecond)
			close(finished)
			return ctx.Err()
		}}
		reg := prometheus.NewRegistry()
		r := New(store, discardLogger(), reg, Options{Buffer: 4, BatchSize: 10, Interval: time.Hour})
		record(r, 1)

		started := time.Now()
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		err := r.Close(ctx)
		select {
		case <-finished:
		default:
			t.Fatal("Close returned before the write finished")
		}
		if got := time.Since(started); got != 60*time.Millisecond {
			t.Errorf("Close took %s, want 60ms (50ms deadline + 10ms cleanup)", got)
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Close error = %v, want deadline exceeded", err)
		}
		if got := gatheredCounter(t, reg, writtenMetricName, map[string]string{"result": resultError}); got != 1 {
			t.Errorf("written error = %v, want 1", got)
		}
	})
}

func TestWorkerPanicStopsRecorder(t *testing.T) {
	reg := prometheus.NewRegistry()
	r := New(panicStore{}, discardLogger(), reg, Options{Buffer: 4, BatchSize: 1, Interval: time.Hour})
	if got := gatheredGauge(t, reg, stoppedMetricName); got != 0 {
		t.Fatalf("initial %s = %v, want 0", stoppedMetricName, got)
	}
	record(r, 1)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := r.Close(ctx)
	if err == nil || !strings.Contains(err.Error(), "store exploded") {
		t.Fatalf("Close error = %v, want panic", err)
	}
	if got := gatheredGauge(t, reg, stoppedMetricName); got != 1 {
		t.Errorf("after panic %s = %v, want 1", stoppedMetricName, got)
	}
	record(r, 2)
	if got := gatheredCounter(t, reg, droppedMetricName, nil); got != 0 {
		t.Errorf("dropped = %v, want 0", got)
	}
}

type fnStore struct {
	fn func(ctx context.Context, clicks []Click) error
}

func (s *fnStore) RecordClicks(ctx context.Context, clicks []Click) error {
	return s.fn(ctx, clicks)
}

type panicStore struct{}

func (panicStore) RecordClicks(context.Context, []Click) error {
	panic("store exploded")
}

func gatheredCounter(t *testing.T, g prometheus.Gatherer, name string, labels map[string]string) float64 {
	t.Helper()
	families, err := g.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		for _, m := range f.GetMetric() {
			if !labelsMatch(m.GetLabel(), labels) {
				continue
			}
			c := m.GetCounter()
			if c == nil {
				t.Fatalf("metric %s is not a counter", name)
			}
			return c.GetValue()
		}
		t.Fatalf("metric %s labels %v not found", name, labels)
	}
	t.Fatalf("metric %s is not registered", name)
	return 0
}

func gatheredGauge(t *testing.T, g prometheus.Gatherer, name string) float64 {
	t.Helper()
	families, err := g.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		for _, m := range f.GetMetric() {
			if m.GetGauge() == nil {
				t.Fatalf("metric %s is not a gauge", name)
			}
			return m.GetGauge().GetValue()
		}
	}
	t.Fatalf("metric %s is not registered", name)
	return 0
}

func labelsMatch(got []*dto.LabelPair, want map[string]string) bool {
	if len(got) != len(want) {
		return false
	}
	for _, l := range got {
		if want[l.GetName()] != l.GetValue() {
			return false
		}
	}
	return true
}

func TestRecorderTracingFlushSpan(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	otel.SetTracerProvider(tp)
	t.Cleanup(func() {
		_ = tp.Shutdown(t.Context())
		otel.SetTracerProvider(noop.NewTracerProvider())
	})

	var gotSpanInStore atomic.Bool
	store := &tracingStore{onRecord: func(ctx context.Context) {
		gotSpanInStore.Store(trace.SpanContextFromContext(ctx).IsValid())
	}}
	r := New(store, discardLogger(), prometheus.NewRegistry(), Options{Buffer: 8, BatchSize: 10, Interval: time.Hour})
	r.Record(Click{LinkID: 7})

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := r.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !gotSpanInStore.Load() {
		t.Error("expected active trace span inside RecordClicks ctx")
	}
	spans := exp.GetSpans()
	if len(spans) != 1 || spans[0].Name != "click.flush" {
		t.Fatalf("spans = %+v, want single click.flush", spans)
	}
}

type tracingStore struct {
	onRecord func(context.Context)
}

func (s *tracingStore) RecordClicks(ctx context.Context, _ []Click) error {
	if s.onRecord != nil {
		s.onRecord(ctx)
	}
	return nil
}
