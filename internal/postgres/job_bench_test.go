package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"shortlink/internal/cache"
	"shortlink/internal/idgen"
	"shortlink/internal/job"
	"shortlink/internal/risk"
	"shortlink/internal/safehttp"
)

/*
BenchmarkEnqueueInCreate 对比「创建链接 + 同语句入队风控任务」与「仅插入链接」的额外开销（ADR 0020）。
需要设置 TEST_DATABASE_URL，默认单测不触发。
*/
func BenchmarkEnqueueInCreate(b *testing.B) {
	store, tenantID := newTestStore(b)
	ctx := testAuditCtx(context.Background())
	highQuota := int64(1_000_000)
	if _, err := store.SetTenantLimits(ctx, tenantID, SetTenantLimitsParams{LinkQuota: &highQuota}); err != nil {
		b.Fatalf("SetTenantLimits: %v", err)
	}

	b.Run("with_enqueue", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for i := range b.N {
			l := mustLink(b, fmt.Sprintf("https://example.com/bench-enq-%d", i), tenantID)
			if err := store.Create(ctx, l); err != nil {
				b.Fatalf("Create: %v", err)
			}
		}
	})

	b.Run("link_only_baseline", func(b *testing.B) {
		const linkOnlySQL = `WITH quota AS (
    UPDATE tenants
    SET link_usage = (` + effectiveUsageSQL + `) + 1,
        link_usage_month = GREATEST(link_usage_month, ` + currentMonthSQL + `)
    WHERE id = $4
      AND (` + effectiveUsageSQL + `) < link_quota
    RETURNING 1
)
INSERT INTO links (code, url, expires_at, tenant_id)
SELECT $1, $2, $3, $4
WHERE EXISTS (SELECT 1 FROM quota)
RETURNING id, created_at`

		b.ReportAllocs()
		b.ResetTimer()
		for i := range b.N {
			code, err := idgen.GenerateCode()
			if err != nil {
				b.Fatalf("GenerateCode: %v", err)
			}
			var (
				id        int64
				createdAt time.Time
			)
			if err := store.db.QueryRowContext(ctx, linkOnlySQL,
				code, fmt.Sprintf("https://example.com/bench-only-%d", i), nil, tenantID).Scan(&id, &createdAt); err != nil {
				b.Fatalf("insert link only: %v", err)
			}
		}
	})
}

/*
BenchmarkJobClaimSkipLocked 测量空 handler 下 N = 8、32、64 并发 worker
通过 FOR UPDATE SKIP LOCKED 抢占并完成任务的队列吞吐上限（ADR 0020）。
*/
func BenchmarkJobClaimSkipLocked(b *testing.B) {
	for _, workers := range []int{8, 32, 64} {
		b.Run(fmt.Sprintf("workers_%d", workers), func(b *testing.B) {
			store, tenantID := newTestStore(b)
			ctx := testAuditCtx(context.Background())

			// 批量预置 b.N 条待执行任务，去掉单条入队循环的准备耗时。
			if _, err := store.db.ExecContext(ctx, `INSERT INTO jobs (kind, tenant_id, payload)
SELECT 'link.risk_scan', $1, jsonb_build_object('code', 'bench', 'url', 'https://example.com')
FROM generate_series(1, $2)`, tenantID, b.N); err != nil {
				b.Fatalf("seed jobs: %v", err)
			}

			var processed atomic.Int64
			b.ResetTimer()
			start := time.Now()

			var wg sync.WaitGroup
			for w := range workers {
				wg.Go(func() {
					workerID := fmt.Sprintf("bench-w-%d", w)
					for {
						batch, err := store.Claim(ctx, job.ClaimParams{
							Kinds:        []string{job.KindLinkRiskScan},
							Limit:        1,
							MaxPerTenant: 0, // 压队列本身上限时关闭单租户限并发
							Lease:        time.Minute,
							LockedBy:     workerID,
						})
						if err != nil {
							b.Errorf("Claim: %v", err)
							return
						}
						if len(batch) == 0 {
							return
						}
						if err := store.Complete(ctx, batch[0].ID, workerID, batch[0].LockVersion); err != nil {
							b.Errorf("Complete: %v", err)
							return
						}
						processed.Add(1)
					}
				})
			}
			wg.Wait()
			elapsed := time.Since(start)
			b.StopTimer()

			if got := processed.Load(); got != int64(b.N) {
				b.Fatalf("processed = %d, want %d", got, b.N)
			}
			if elapsed > 0 {
				b.ReportMetric(float64(b.N)/elapsed.Seconds(), "jobs/s")
			}
		})
	}
}

/*
BenchmarkRiskScanEndToEnd 用可控延迟的 httptest 假站点测异步风控整条链路吞吐，
对比无缓存与开启 Redis 结果缓存后的吞吐差异（ADR 0020）。
*/
func BenchmarkRiskScanEndToEnd(b *testing.B) {
	redisURL := os.Getenv("TEST_REDIS_URL")
	if redisURL == "" {
		b.Skip("TEST_REDIS_URL is not set")
	}

	const simulatedLatency = 25 * time.Millisecond
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(simulatedLatency)
		_, _ = w.Write([]byte("<html>safe landing page</html>"))
	}))
	defer stub.Close()
	stubURL, _ := url.Parse(stub.URL)

	for _, useCache := range []bool{false, true} {
		name := "no_cache"
		if useCache {
			name = "with_redis_cache"
		}
		b.Run(name, func(b *testing.B) {
			store, tenantID := newTestStore(b)
			ctx := testAuditCtx(context.Background())

			rdb, err := cache.Open(ctx, redisURL)
			if err != nil {
				b.Fatalf("open redis: %v", err)
			}
			b.Cleanup(func() { _ = rdb.Close() })

			targetURL := fmt.Sprintf("%s/bench-%s-%d", stub.URL, name, time.Now().UnixNano())
			payload, _ := json.Marshal(risk.ScanPayload{LinkID: 1, Code: "AbC12345", URL: targetURL})

			if _, err := store.db.ExecContext(ctx, `INSERT INTO jobs (kind, tenant_id, payload)
SELECT 'link.risk_scan', $1, $2::jsonb
FROM generate_series(1, $3)`, tenantID, string(payload), b.N); err != nil {
				b.Fatalf("seed risk jobs: %v", err)
			}

			var vc *risk.VerdictCache
			if useCache {
				vc = risk.NewVerdictCache(rdb, prometheus.NewRegistry(), risk.VerdictCacheOptions{})
			}
			scanner := risk.NewRuleScanner(risk.RuleScannerOptions{
				HTTPClient: risk.NewSafeHTTPClient(safehttp.ClientOptions{
					Timeout:          5 * time.Second,
					AllowedHostPorts: []string{stubURL.Host},
				}),
				Limiter: safehttp.NewHostLimiter(0),
			})
			linkStore := cache.New(store, rdb, prometheus.NewRegistry(), cache.Options{})
			handler := risk.NewHandler(risk.HandlerOptions{
				Scanner: scanner,
				Banner:  linkStore,
				Cache:   vc,
			})

			b.ResetTimer()
			start := time.Now()
			var wg sync.WaitGroup
			for w := range 16 {
				wg.Go(func() {
					workerID := fmt.Sprintf("risk-bench-%d", w)
					for {
						batch, err := store.Claim(ctx, job.ClaimParams{
							Kinds:    []string{job.KindLinkRiskScan},
							Limit:    1,
							Lease:    time.Minute,
							LockedBy: workerID,
						})
						if err != nil {
							b.Errorf("Claim: %v", err)
							return
						}
						if len(batch) == 0 {
							return
						}
						if err := handler(ctx, batch[0]); err != nil {
							b.Errorf("handler: %v", err)
							return
						}
						if err := store.Complete(ctx, batch[0].ID, workerID, batch[0].LockVersion); err != nil {
							b.Errorf("Complete: %v", err)
							return
						}
					}
				})
			}
			wg.Wait()
			elapsed := time.Since(start)
			b.StopTimer()
			if elapsed > 0 {
				b.ReportMetric(float64(b.N)/elapsed.Seconds(), "jobs/s")
			}
		})
	}
}
