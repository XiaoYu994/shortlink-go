package cache

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"

	"shortlink/internal/link"
)

type memoryRedisHook struct {
	mu   sync.Mutex
	data map[string]string
	err  error
}

func newMemoryRedisClient(hook *memoryRedisHook) *redis.Client {
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	rdb.AddHook(hook)
	return rdb
}

func (h *memoryRedisHook) DialHook(next redis.DialHook) redis.DialHook {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		return next(ctx, network, addr)
	}
}

func (h *memoryRedisHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.data == nil {
			h.data = make(map[string]string)
		}
		if h.err != nil {
			cmd.SetErr(h.err)
			return h.err
		}
		switch cmd.Name() {
		case "get":
			sc := cmd.(*redis.StringCmd)
			key := cmd.Args()[1].(string)
			v, ok := h.data[key]
			if !ok {
				sc.SetErr(redis.Nil)
				return redis.Nil
			}
			sc.SetVal(v)
			return nil
		case "set":
			key := cmd.Args()[1].(string)
			val := stringArg(cmd.Args()[2])
			switch c := cmd.(type) {
			case *redis.BoolCmd:
				if _, exists := h.data[key]; exists {
					c.SetVal(false)
					return nil
				}
				h.data[key] = val
				c.SetVal(true)
			case *redis.StatusCmd:
				h.data[key] = val
				c.SetVal("OK")
			}
			return nil
		case "del":
			ic := cmd.(*redis.IntCmd)
			var n int64
			for _, arg := range cmd.Args()[1:] {
				key := arg.(string)
				if _, ok := h.data[key]; ok {
					delete(h.data, key)
					n++
				}
			}
			ic.SetVal(n)
			return nil
		default:
			return next(ctx, cmd)
		}
	}
}

func (h *memoryRedisHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

func stringArg(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case []byte:
		return string(x)
	default:
		return ""
	}
}

func setupCacheTestTracer(t *testing.T) *tracetest.InMemoryExporter {
	t.Helper()
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	otel.SetTracerProvider(tp)
	t.Cleanup(func() {
		_ = tp.Shutdown(t.Context())
		otel.SetTracerProvider(noop.NewTracerProvider())
	})
	return exp
}

func TestCacheTracingHitsAndWrites(t *testing.T) {
	exp := setupCacheTestTracer(t)

	hook := &memoryRedisHook{}
	rdb := newMemoryRedisClient(hook)
	t.Cleanup(func() { _ = rdb.Close() })

	want := &link.Link{ID: 7, Code: "hit00001", URL: "https://example.com/hit"}
	next := &fakeStore{}
	s := New(next, rdb, prometheus.NewRegistry(), Options{})

	// 1. 已取消上下文直接返回，不产生 span 也不访问存储。
	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.GetByCode(canceledCtx, want.Code); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled GetByCode err = %v", err)
	}
	if spans := exp.GetSpans(); len(spans) != 0 {
		t.Fatalf("canceled GetByCode produced %d spans, want 0", len(spans))
	}

	// 2. Create 写穿产生 redis.set span。
	ctx, root := otel.Tracer("test").Start(context.Background(), "create-and-read")
	if err := s.Create(ctx, want); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// 3. GetByCode 命中正缓存产生 cache.get -> redis.get(hit)。
	got, err := s.GetByCode(ctx, want.Code)
	if err != nil || got.URL != want.URL {
		t.Fatalf("GetByCode hit = (%+v, %v)", got, err)
	}

	// 4. 负缓存命中产生 cache.get(neg_hit)。
	hook.mu.Lock()
	negRaw, _ := json.Marshal((*link.Link)(nil))
	hook.data[keyPrefix+"neg00001"] = string(negRaw)
	hook.mu.Unlock()
	if _, err := s.GetByCode(ctx, "neg00001"); !errors.Is(err, link.ErrNotFound) {
		t.Fatalf("GetByCode neg_hit err = %v, want ErrNotFound", err)
	}
	root.End()

	spans := exp.GetSpans()
	var sawSet, sawHit, sawNegHit bool
	for _, sp := range spans {
		attrs := cacheAttrMap(sp.Attributes)
		if sp.Name == "redis.set" && attrs["cache.write_result"] == writeSuccess {
			sawSet = true
		}
		if sp.Name == "cache.get" && attrs["cache.result"] == resultHit {
			sawHit = true
		}
		if sp.Name == "cache.get" && attrs["cache.result"] == "neg_hit" {
			sawNegHit = true
		}
	}
	if !sawSet || !sawHit || !sawNegHit {
		t.Errorf("missing expected spans: set=%v hit=%v neg_hit=%v", sawSet, sawHit, sawNegHit)
	}
}

func TestCacheTracingSingleflightWaiterLink(t *testing.T) {
	exp := setupCacheTestTracer(t)

	hook := &memoryRedisHook{}
	rdb := newMemoryRedisClient(hook)
	t.Cleanup(func() { _ = rdb.Close() })

	want := &link.Link{ID: 11, Code: "sf000001", URL: "https://example.com/sf"}
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	next := &fakeStore{
		get: func(ctx context.Context, _ string) (*link.Link, error) {
			once.Do(func() { close(entered) })
			<-release
			return want, nil
		},
	}
	s := New(next, rdb, prometheus.NewRegistry(), Options{})

	leaderCtx, leaderRoot := otel.Tracer("test").Start(context.Background(), "leader-req")
	waiterCtx, waiterRoot := otel.Tracer("test").Start(context.Background(), "waiter-req")

	errCh := make(chan error, 2)
	go func() {
		_, err := s.GetByCode(leaderCtx, want.Code)
		leaderRoot.End()
		errCh <- err
	}()
	<-entered
	go func() {
		_, err := s.GetByCode(waiterCtx, want.Code)
		waiterRoot.End()
		errCh <- err
	}()

	time.Sleep(30 * time.Millisecond)
	close(release)
	for range 2 {
		if err := <-errCh; err != nil {
			t.Fatalf("GetByCode err = %v", err)
		}
	}

	spans := exp.GetSpans()
	var (
		originSpanID trace.SpanID
		originTrace  trace.TraceID
		waiterSpan   *tracetest.SpanStub
		leaderSawSet bool
	)
	for i := range spans {
		sp := &spans[i]
		attrs := cacheAttrMap(sp.Attributes)
		switch sp.Name {
		case "cache.origin_fetch":
			originSpanID = sp.SpanContext.SpanID()
			originTrace = sp.SpanContext.TraceID()
		case "redis.setnx":
			leaderSawSet = true
		case "cache.get":
			if sp.SpanContext.TraceID() == waiterRoot.SpanContext().TraceID() {
				waiterSpan = sp
			}
		}
		_ = attrs
	}
	if !originSpanID.IsValid() {
		t.Fatal("missing cache.origin_fetch span")
	}
	if originTrace != leaderRoot.SpanContext().TraceID() {
		t.Errorf("origin_fetch trace = %s, want leader trace %s", originTrace, leaderRoot.SpanContext().TraceID())
	}
	if !leaderSawSet {
		t.Error("expected redis.setnx span during origin fetch")
	}
	if waiterSpan == nil {
		t.Fatal("missing waiter cache.get span")
	}
	waiterAttrs := cacheAttrMap(waiterSpan.Attributes)
	if waiterAttrs["cache.singleflight.shared"] != true || waiterAttrs["cache.singleflight.leader"] != false {
		t.Errorf("waiter singleflight attrs = %+v", waiterAttrs)
	}
	if len(waiterSpan.Links) != 1 {
		t.Fatalf("waiter links = %d, want 1", len(waiterSpan.Links))
	}
	if waiterSpan.Links[0].SpanContext.SpanID() != originSpanID ||
		waiterSpan.Links[0].SpanContext.TraceID() != originTrace {
		t.Errorf("waiter link = %+v, want origin (%s, %s)", waiterSpan.Links[0].SpanContext, originTrace, originSpanID)
	}
}

func TestCacheTracingFallbackAndShedError(t *testing.T) {
	exp := setupCacheTestTracer(t)

	hook := &memoryRedisHook{err: errors.New("redis down")}
	rdb := newMemoryRedisClient(hook)
	t.Cleanup(func() { _ = rdb.Close() })

	entered := make(chan struct{})
	release := make(chan struct{})
	next := &fakeStore{
		get: func(ctx context.Context, _ string) (*link.Link, error) {
			entered <- struct{}{}
			<-release
			return &link.Link{ID: 1, Code: "fb", URL: "https://example.com"}, nil
		},
	}
	s := New(next, rdb, prometheus.NewRegistry(), Options{})

	// 占满 fallbackConcurrency 个槽位后，第 fallbackConcurrency+1 个请求触发 shed。
	var wg sync.WaitGroup
	for i := range fallbackConcurrency {
		wg.Go(func() {
			_, _ = s.GetByCode(context.Background(), "slot-"+string(rune('a'+i)))
		})
		<-entered
	}

	shedCtx, shedRoot := otel.Tracer("test").Start(context.Background(), "shed-req")
	_, err := s.GetByCode(shedCtx, "shed-code")
	shedRoot.End()
	close(release)
	wg.Wait()

	if _, ok := errors.AsType[fetchShedError](err); !ok {
		t.Fatalf("expected fetchShedError, got %v", err)
	}

	var sawShed bool
	for _, sp := range exp.GetSpans() {
		if sp.SpanContext.TraceID() == shedRoot.SpanContext().TraceID() && sp.Name == "cache.get" {
			attrs := cacheAttrMap(sp.Attributes)
			if attrs["cache.shed"] == true && attrs["cache.shed.path"] == resultFallback && sp.Status.Code == codes.Error {
				sawShed = true
			}
		}
	}
	if !sawShed {
		t.Error("expected cache.get span on shed request to record cache.shed=true and Error status")
	}
}

func cacheAttrMap(attrs []attribute.KeyValue) map[string]any {
	out := make(map[string]any, len(attrs))
	for _, a := range attrs {
		out[string(a.Key)] = a.Value.AsInterface()
	}
	return out
}
