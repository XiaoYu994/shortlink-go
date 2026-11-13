package click

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

type memStore struct {
	mu     sync.Mutex
	counts map[int64]int64
	err    error
	// wrote 在成功写入后收到一次信号。容量是 1，避免堵住 worker。
	wrote chan struct{}
}

func (s *memStore) AddClicks(_ context.Context, counts map[int64]int64) error {
	if s.err != nil {
		s.signalWrote()
		return s.err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.counts == nil {
		s.counts = make(map[int64]int64)
	}
	for id, n := range counts {
		s.counts[id] += n
	}
	s.signalWrote()
	return nil
}

func (s *memStore) signalWrote() {
	if s.wrote == nil {
		return
	}
	select {
	case s.wrote <- struct{}{}:
	default:
	}
}

func (s *memStore) get(id int64) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.counts[id]
}

func TestCloseFlushesPartialBatch(t *testing.T) {
	store := &memStore{}
	reg := prometheus.NewRegistry()
	// 间隔很长、批量也大，这三条只能靠 Close 写出去。
	r := New(store, discardLogger(), reg, Options{Buffer: 8, BatchSize: 10, Interval: time.Hour})

	r.Record(7)
	r.Record(7)
	r.Record(8)
	r.Record(0)

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

func TestFlushesWhenBatchFull(t *testing.T) {
	store := &memStore{wrote: make(chan struct{}, 1)}
	reg := prometheus.NewRegistry()
	r := New(store, discardLogger(), reg, Options{Buffer: 8, BatchSize: 2, Interval: time.Hour})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := r.Close(ctx); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	r.Record(3)
	r.Record(3)
	select {
	case <-store.wrote:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for batch flush")
	}
	if got := store.get(3); got != 2 {
		t.Errorf("clicks[3] = %d, want 2", got)
	}
}

func TestCloseReturnsWriteError(t *testing.T) {
	store := &memStore{err: errors.New("db down")}
	reg := prometheus.NewRegistry()
	r := New(store, discardLogger(), reg, Options{Buffer: 8, BatchSize: 10, Interval: time.Hour})
	r.Record(1)

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

func (s *blockingStore) AddClicks(context.Context, map[int64]int64) error {
	s.once.Do(func() { close(s.entered) })
	<-s.release
	return nil
}

func TestRecordDropsWhenBufferFull(t *testing.T) {
	store := &blockingStore{
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	reg := prometheus.NewRegistry()
	r := New(store, discardLogger(), reg, Options{Buffer: 2, BatchSize: 1, Interval: time.Hour})
	t.Cleanup(func() {
		close(store.release)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := r.Close(ctx); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	r.Record(1)
	select {
	case <-store.entered:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for the worker to block in AddClicks")
	}

	// worker 已经取走 1 并停在写库上，channel 是空的。再放 3 条，缓冲只有 2 个位置。
	r.Record(2)
	r.Record(3)
	r.Record(4)

	if got := gatheredCounter(t, reg, droppedMetricName, nil); got != 1 {
		t.Errorf("dropped = %v, want 1", got)
	}
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
	r.Record(1)
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
			r.Record(int64(i%20 + 1))
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
}

func TestFlushesOnInterval(t *testing.T) {
	store := &memStore{wrote: make(chan struct{}, 1)}
	reg := prometheus.NewRegistry()
	r := New(store, discardLogger(), reg, Options{Buffer: 8, BatchSize: 10, Interval: 15 * time.Millisecond})

	r.Record(5)
	select {
	case <-store.wrote:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for interval flush")
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := r.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := store.get(5); got != 1 {
		t.Errorf("clicks[5] = %d, want 1", got)
	}
	if got := gatheredCounter(t, reg, writtenMetricName, map[string]string{"result": resultSuccess}); got != 1 {
		t.Errorf("written success = %v, want 1", got)
	}
}

func TestRunningWriteErrorIsMetricOnly(t *testing.T) {
	store := &memStore{err: errors.New("db down"), wrote: make(chan struct{}, 1)}
	reg := prometheus.NewRegistry()
	r := New(store, discardLogger(), reg, Options{Buffer: 4, BatchSize: 1, Interval: time.Hour})

	r.Record(1)
	select {
	case <-store.wrote:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for the running flush")
	}

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
}

/*
TestDrainFullBatchError 把 worker 挡在第一批上，让后面的满批留在 channel 里。
Close 之后这些满批走排空，写失败要返回给 Close，不能只打日志。
*/
func TestDrainFullBatchError(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	storeErr := errors.New("db down")
	var mu sync.Mutex
	var calls int
	store := &fnStore{fn: func(context.Context, map[int64]int64) error {
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

	r.Record(1)
	r.Record(1)
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for the first flush")
	}
	for range 4 {
		r.Record(2)
	}

	errCh := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		errCh <- r.Close(ctx)
	}()
	waitStopped(t, r)
	close(release)

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
}

/*
TestDrainStopsWhenCloseContextEnds 固定排空的时限是 Close 的 ctx。
若满批各自再占一份 writeTimeout，这里会按批数乘上去，Close 不会在 2s 内返回。
*/
func TestDrainStopsWhenCloseContextEnds(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var mu sync.Mutex
	var calls int
	store := &fnStore{fn: func(ctx context.Context, _ map[int64]int64) error {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n == 1 {
			close(entered)
			<-release
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
			return nil
		}
	}}
	reg := prometheus.NewRegistry()
	r := New(store, discardLogger(), reg, Options{
		Buffer: 16, BatchSize: 2, Interval: time.Hour, WriteTimeout: 5 * time.Second,
	})

	r.Record(1)
	r.Record(1)
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for the first flush")
	}
	for range 6 {
		r.Record(2)
	}

	errCh := make(chan error, 1)
	started := time.Now()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
		defer cancel()
		errCh <- r.Close(ctx)
	}()
	waitStopped(t, r)
	close(release)

	var err error
	select {
	case err = <-errCh:
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not return within 2s")
	}
	if time.Since(started) > 2*time.Second {
		t.Fatalf("Close took %s, drain did not share the close context", time.Since(started))
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close error = %v, want deadline exceeded", err)
	}
}

/*
TestCloseWaitsForCanceledWrite 固定 ctx 到期后 Close 仍等这一批写完。
尾部用的就是 Close 的 ctx，取消会让写入返回，Close 再把这个错误交出去。
*/
func TestCloseWaitsForCanceledWrite(t *testing.T) {
	finished := make(chan struct{})
	store := &fnStore{fn: func(ctx context.Context, _ map[int64]int64) error {
		<-ctx.Done()
		close(finished)
		return ctx.Err()
	}}
	reg := prometheus.NewRegistry()
	r := New(store, discardLogger(), reg, Options{Buffer: 4, BatchSize: 10, Interval: time.Hour})
	r.Record(1)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := r.Close(ctx)
	select {
	case <-finished:
	default:
		t.Fatal("Close returned before the write finished")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close error = %v, want deadline exceeded", err)
	}
	if got := gatheredCounter(t, reg, writtenMetricName, map[string]string{"result": resultError}); got != 1 {
		t.Errorf("written error = %v, want 1", got)
	}
}

func TestWorkerPanicStopsRecorder(t *testing.T) {
	reg := prometheus.NewRegistry()
	r := New(panicStore{}, discardLogger(), reg, Options{Buffer: 4, BatchSize: 1, Interval: time.Hour})
	r.Record(1)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := r.Close(ctx)
	if err == nil || !strings.Contains(err.Error(), "store exploded") {
		t.Fatalf("Close error = %v, want panic", err)
	}
	r.Record(2)
	if got := gatheredCounter(t, reg, droppedMetricName, nil); got != 0 {
		t.Errorf("dropped = %v, want 0", got)
	}
}

type fnStore struct {
	fn func(ctx context.Context, counts map[int64]int64) error
}

func (s *fnStore) AddClicks(ctx context.Context, counts map[int64]int64) error {
	return s.fn(ctx, counts)
}

type panicStore struct{}

func (panicStore) AddClicks(context.Context, map[int64]int64) error {
	panic("store exploded")
}

func waitStopped(t *testing.T, r *Recorder) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		r.mu.RLock()
		stopped := r.stopped
		r.mu.RUnlock()
		if stopped {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("timed out waiting for the recorder to stop")
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
