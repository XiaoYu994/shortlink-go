package ratelimit

import (
	"fmt"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

func TestLimiterDisabledAndNil(t *testing.T) {
	var nilLimiter *Limiter[string]
	if delay, ok := nilLimiter.Allow("a", 1, 1); !ok || delay != 0 {
		t.Fatalf("nil Allow = (%v, %v), want (0, true)", delay, ok)
	}
	if got := nilLimiter.Len(); got != 0 {
		t.Fatalf("nil Len = %d, want 0", got)
	}

	l := New[int64](Options{})
	for range 10 {
		if delay, ok := l.Allow(1, 0, 0); !ok || delay != 0 {
			t.Fatalf("unlimited Allow = (%v, %v), want (0, true)", delay, ok)
		}
	}
	if got := l.Len(); got != 0 {
		t.Errorf("Len = %d, want 0 when rps <= 0", got)
	}
}

/*
TestLimiterRejectCancelsReservation 验证超限被拒不会透支未来令牌：
在虚拟时钟内连续被拒多次后，满 1 秒仍能正常恢复放行。
*/
func TestLimiterRejectCancelsReservation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := New[string](Options{})
		if _, ok := l.Allow("k", 1, 1); !ok {
			t.Fatal("first request should be allowed")
		}
		for i := range 20 {
			delay, ok := l.Allow("k", 1, 1)
			if ok || delay <= 0 {
				t.Fatalf("rejected attempt %d = (%v, %v), want (positive delay, false)", i, delay, ok)
			}
		}

		time.Sleep(500 * time.Millisecond)
		if _, ok := l.Allow("k", 1, 1); ok {
			t.Fatal("request after 500ms should still be rejected")
		}

		time.Sleep(500 * time.Millisecond)
		if _, ok := l.Allow("k", 1, 1); !ok {
			t.Fatal("request after 1s should recover if rejected reservations were canceled")
		}
	})
}

func TestLimiterDynamicLimitChange(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := New[int64](Options{})
		if _, ok := l.Allow(1, 1, 1); !ok {
			t.Fatal("first request should be allowed")
		}
		if _, ok := l.Allow(1, 1, 1); ok {
			t.Fatal("second request at rps=1 should be rejected")
		}
		if _, ok := l.Allow(1, 100, 100); !ok {
			t.Fatal("request after limit increase should be allowed immediately")
		}
	})
}

/*
TestLimiterConcurrentLimitChange 让同一个 key 的并发请求交替带两种速率，
每次都会换桶。锁内拷贝 *rate.Limiter 指针后解锁调用，-race 下不得出现数据竞争。
*/
func TestLimiterConcurrentLimitChange(t *testing.T) {
	l := New[int64](Options{})
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			rps := 10 + i%2
			for range 200 {
				l.Allow(1, rps, rps)
			}
		})
	}
	wg.Wait()
}

/*
TestLimiterEvictsIdle 在 synctest 虚拟时钟下用默认 IdleTTL / SweepInterval
验证精确 LRU 队尾惰性清理和清理节流间隔。
*/
func TestLimiterEvictsIdle(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := New[int64](Options{})

		l.Allow(1, 10, 10)
		l.Allow(2, 10, 10)

		// 刚满 DefaultIdleTTL 不算超过（> DefaultIdleTTL 才清）；同时推进 lastSweep。
		time.Sleep(DefaultIdleTTL)
		l.Allow(3, 10, 10)
		for _, id := range []int64{1, 2} {
			if _, ok := l.buckets[id]; !ok {
				t.Errorf("key %d evicted at exact idle TTL", id)
			}
		}

		// 离上次清理差 1 秒才到 DefaultSweepInterval，跳过清理。
		time.Sleep(DefaultSweepInterval - time.Second)
		l.Allow(4, 10, 10)
		for _, id := range []int64{1, 2} {
			if _, ok := l.buckets[id]; !ok {
				t.Errorf("key %d evicted before sweep interval elapsed", id)
			}
		}

		// 再过 1 秒到达清理周期，已超过 idle TTL 的 1、2 被回收，活跃的 3、4、5 保留。
		time.Sleep(time.Second)
		l.Allow(5, 10, 10)
		for _, id := range []int64{1, 2} {
			if _, ok := l.buckets[id]; ok {
				t.Errorf("idle key %d was not evicted", id)
			}
		}
		for _, id := range []int64{3, 4, 5} {
			if _, ok := l.buckets[id]; !ok {
				t.Errorf("active key %d missing from limiter", id)
			}
		}
		if got := l.Len(); got != 3 {
			t.Errorf("Len = %d, want 3", got)
		}
	})
}

/*
TestLimiterBoundedLRUEviction 验证满容时严格按 LRU 驱逐最久未活跃条目：
活跃（包括正在被限流拒收的）热 Key 停留在队头，不会被随机新 Key 洪水踢出从而逃逸限流。
*/
func TestLimiterBoundedLRUEviction(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := New[string](Options{MaxBuckets: 3})

		if _, ok := l.Allow("hot", 1, 1); !ok {
			t.Fatal("hot first request should be allowed")
		}
		l.Allow("a", 1, 1)
		l.Allow("b", 1, 1)

		// 被拒收的访问同样刷新热 Key 的 LRU 位置，使其移回队头。
		if _, ok := l.Allow("hot", 1, 1); ok {
			t.Fatal("hot second request should be rejected")
		}

		// 涌入两个新 Key 触发两次驱逐：应先踢 "a" 再踢 "b"，保留 "hot"。
		l.Allow("c", 1, 1)
		l.Allow("d", 1, 1)

		if got := l.Len(); got != 3 {
			t.Fatalf("Len = %d, want 3", got)
		}
		if _, ok := l.buckets["a"]; ok {
			t.Error("oldest key a should have been evicted")
		}
		if _, ok := l.buckets["b"]; ok {
			t.Error("second oldest key b should have been evicted")
		}
		if _, ok := l.Allow("hot", 1, 1); ok {
			t.Error("hot key was evicted and escaped rate limit")
		}
		// 已被淘汰的 "a" 重新进入会新建满桶并放行。
		if _, ok := l.Allow("a", 1, 1); !ok {
			t.Error("evicted key a should start fresh when re-entering")
		}
	})
}

/*
TestLimiterBatchSweepDrainsGradually 验证大批条目同时过期时，
单次请求只回收 defaultMaxSweepBatch 个节点，且不推进 lastSweep，由后续请求立刻分摊清完。
*/
func TestLimiterBatchSweepDrainsGradually(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := New[string](Options{})
		const total = defaultMaxSweepBatch + 20
		for i := range total {
			l.Allow(fmt.Sprintf("k-%d", i), 10, 10)
		}
		if got := l.Len(); got != total {
			t.Fatalf("seeded Len = %d, want %d", got, total)
		}

		time.Sleep(DefaultIdleTTL + time.Second)

		// 第一次触发：新增 1 个活跃键，同时只清掉第一批 defaultMaxSweepBatch 个过期键。
		l.Allow("active-1", 10, 10)
		if got, want := l.Len(), total-defaultMaxSweepBatch+1; got != want {
			t.Fatalf("Len after first batch sweep = %d, want %d", got, want)
		}

		// 紧接着下一次请求（不推进时钟）：因队尾仍有过期键未推进 lastSweep，立即清掉剩余 20 个过期键。
		l.Allow("active-2", 10, 10)
		if got := l.Len(); got != 2 {
			t.Fatalf("Len after second batch sweep = %d, want 2", got)
		}
	})
}
