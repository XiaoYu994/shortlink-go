/*
Package click 异步累计短链点击（第 10 课）。

跳转请求只把链接 ID 放进缓冲 channel。后台 goroutine 按批写回 links.clicks，
进程退出前由 Close 把缓冲区排空。这条 goroutine 在 New 里启动，
在 channel 被关闭且排空写完后退出。入口会 recover：panic 时停掉记录，
并把错误交给 Close。
*/
package click

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

const (
	defaultBuffer       = 1024
	defaultBatchSize    = 100
	defaultInterval     = time.Second
	defaultWriteTimeout = 2 * time.Second

	droppedMetricName = "shortlink_clicks_dropped_total"
	writtenMetricName = "shortlink_clicks_written_total"

	resultSuccess = "success"
	resultError   = "error"
)

/*
Store 是 Recorder 要的写能力，定义在使用方。
测试要挡住写入才能把 channel 填满，所以这里用接口，click 不依赖 postgres。
*/
type Store interface {
	AddClicks(ctx context.Context, counts map[int64]int64) error
}

/*
Options 里的零值会换成默认。
测试可以只改 Buffer 和 BatchSize，其余字段留零。
*/
type Options struct {
	Buffer       int
	BatchSize    int
	Interval     time.Duration
	WriteTimeout time.Duration
}

func (o Options) withDefaults() Options {
	if o.Buffer <= 0 {
		o.Buffer = defaultBuffer
	}
	if o.BatchSize <= 0 {
		o.BatchSize = defaultBatchSize
	}
	if o.Interval <= 0 {
		o.Interval = defaultInterval
	}
	if o.WriteTimeout <= 0 {
		o.WriteTimeout = defaultWriteTimeout
	}
	return o
}

/*
Recorder 把点击攒成一批再写。

必须用指针：里面有 channel 和锁，复制之后关闭和发送就不是同一份状态。
*/
type Recorder struct {
	store        Store
	logger       *slog.Logger
	ch           chan int64
	done         chan struct{}
	dropped      prometheus.Counter
	written      *prometheus.CounterVec
	batchSize    int
	interval     time.Duration
	writeTimeout time.Duration

	/*
		mu 保护 stopped、exitCtx 和 channel 的关闭。
		Record 持有读锁再发送，Close 和 panic 收尾拿写锁后才 close。
		发送不阻塞，否则关停会等不到这把写锁。
		closeErr 只由 worker 在退出前写，Close 在收到 done 之后读。
	*/
	mu       sync.RWMutex
	stopped  bool
	exitCtx  context.Context
	closeErr error
}

/*
New 注册点击指标并启动后台 goroutine。
goroutine 的退出点在 loop：Close 关闭 channel，排空写完，loop 返回。
panic 也从这里退出，之后 Record 不再接收，Close 返回错误。
*/
func New(store Store, logger *slog.Logger, registry *prometheus.Registry, opts Options) *Recorder {
	opts = opts.withDefaults()
	r := &Recorder{
		store:  store,
		logger: logger,
		ch:     make(chan int64, opts.Buffer),
		done:   make(chan struct{}),
		dropped: prometheus.NewCounter(prometheus.CounterOpts{
			Name: droppedMetricName,
			Help: "Clicks dropped because the recorder buffer was full.",
		}),
		written: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: writtenMetricName,
			Help: "Clicks included in a store write, by result.",
		}, []string{"result"}),
		batchSize:    opts.BatchSize,
		interval:     opts.Interval,
		writeTimeout: opts.WriteTimeout,
	}
	// 先取出两个 label，进程一启动 /metrics 里就是 0。没写出过点击时也能看到这条指标。
	r.written.WithLabelValues(resultSuccess)
	r.written.WithLabelValues(resultError)
	registry.MustRegister(r.dropped, r.written)
	go r.loop()
	return r
}

/*
Record 在缓冲有空位时放入 ID，满了就计一次 dropped。

id 小于等于 0 直接丢掉，那是调用方的 bug，不是队列满。
Close 已经开始，或 worker 因 panic 停下时，也不再放入，并且不计入 dropped：
dropped 只表示缓冲满。
*/
func (r *Recorder) Record(id int64) {
	if id <= 0 {
		return
	}

	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.stopped {
		return
	}
	select {
	case r.ch <- id:
	default:
		r.dropped.Inc()
	}
}

/*
Close 关闭 channel 并等待排空结束。

可以多次调用。第一次负责关闭，之后的调用只等待已经退出的 goroutine。
并发或重复调用时以第一次的 ctx 为准，后来的 ctx 不会延长排空时限。

ctx 约束 channel 关闭之后的每一批，包括满批和尾部。
已经开始的普通批次继续用自己的 writeTimeout，这里不取消它，
否则关停会把已经交给数据库的点击丢掉。
ctx 到期后仍会等当前这一批结束再返回，避免调用方关掉还在使用的连接池。
*/
func (r *Recorder) Close(ctx context.Context) error {
	r.mu.Lock()
	if !r.stopped {
		r.stopped = true
		// 先赋值再 close。worker 看到 stopped 时，exitCtx 已经写好。
		r.exitCtx = ctx
		close(r.ch)
	}
	r.mu.Unlock()

	select {
	case <-r.done:
		return r.closeErr
	case <-ctx.Done():
	}
	/*
		排空中的批次用的就是这个 ctx，取消后会结束并让 loop 退出。
		已经开始的普通批次不在这个 ctx 上，这里仍要等它写完，
		调用方随后才能关连接池。
	*/
	<-r.done
	if r.closeErr != nil {
		return r.closeErr
	}
	return fmt.Errorf("close click recorder: %w", ctx.Err())
}

/*
loop 凑满一批，或间隔到了，就写一次。
channel 关闭后，剩下的满批和尾部都用 exitCtx 写出，错误交给 Close。
*/
func (r *Recorder) loop() {
	defer close(r.done)
	defer r.recoverLoop()

	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()

	batch := make([]int64, 0, r.batchSize)
	for {
		select {
		case id, ok := <-r.ch:
			if !ok {
				r.flush(batch)
				return
			}
			batch = append(batch, id)
			if len(batch) >= r.batchSize {
				r.flush(batch)
				batch = batch[:0]
			}
		case <-ticker.C:
			r.flush(batch)
			batch = batch[:0]
		}
	}
}

/*
recoverLoop 接住 loop 的 panic。
Store 的实现出了问题也不能把进程打垮。
停掉之后 Record 不再往 channel 里放：没人消费的话，缓冲会满，新点击全部变成 dropped，
Close 还可能看不出 worker 已经不在了。
栈在 panic 现场打一次。错误留给 Close，关停时 main 才能知道记录已经停了。
*/
func (r *Recorder) recoverLoop() {
	rec := recover()
	if rec == nil {
		return
	}
	r.mu.Lock()
	if !r.stopped {
		r.stopped = true
		close(r.ch)
	}
	r.mu.Unlock()
	err := fmt.Errorf("click recorder panic: %v", rec)
	r.noteCloseErr(err)
	r.logger.Error("click recorder panic", "err", err, "stack", string(debug.Stack()))
}

/*
flush 在正常运行时用独立的超时，不用请求的 ctx：跳转响应结束时，这批点击还没写。
写失败只打日志。停在这一批上重试会让 channel 堆满，后面的点击变成 dropped。

Close 开始之后改用 exitCtx，错误留给 Close 返回，这里不再打日志。
*/
func (r *Recorder) flush(batch []int64) {
	if len(batch) == 0 {
		return
	}
	if ctx := r.draining(); ctx != nil {
		err := r.write(ctx, batch)
		r.observeWrite(len(batch), err)
		r.noteCloseErr(err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), r.writeTimeout)
	defer cancel()
	err := r.write(ctx, batch)
	r.observeWrite(len(batch), err)
	if err != nil {
		r.logger.Error("flush clicks", "err", err, "clicks", len(batch))
	}
}

/*
draining 在 Close 已经开始时返回那一次的 ctx。
还在正常运行时返回 nil。读锁和 Close 的写锁配对，不会读到写了一半的 exitCtx。
*/
func (r *Recorder) draining() context.Context {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if !r.stopped {
		return nil
	}
	return r.exitCtx
}

// noteCloseErr 保留排空期间的写入错误。worker 退出前调用，Close 在收到 done 之后读取。
func (r *Recorder) noteCloseErr(err error) {
	if err == nil {
		return
	}
	r.closeErr = errors.Join(r.closeErr, err)
}

func (r *Recorder) observeWrite(n int, err error) {
	result := resultSuccess
	if err != nil {
		result = resultError
	}
	r.written.WithLabelValues(result).Add(float64(n))
}

/*
write 把这一批交给存储。countsOf 会把 ID 收成一份新 map，
所以调用方可以在 write 返回后复用 batch 的底层数组。
*/
func (r *Recorder) write(ctx context.Context, batch []int64) error {
	if len(batch) == 0 {
		return nil
	}
	if err := r.store.AddClicks(ctx, countsOf(batch)); err != nil {
		return fmt.Errorf("flush clicks: %w", err)
	}
	return nil
}

/*
countsOf 把同一条链接在这一批里的点击加成一个总数。
同一 id 在一条 UPDATE 里出现两次时，PostgreSQL 只应用其中一行，用哪一行不确定。
*/
func countsOf(ids []int64) map[int64]int64 {
	counts := make(map[int64]int64, len(ids))
	for _, id := range ids {
		counts[id]++
	}
	return counts
}
