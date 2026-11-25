package main

import (
	"bytes"
	"context"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"shortlink/internal/job"
)

type stubWorkerStore struct {
	purgeCalls int
	statsCalls int
	auditCalls int
	whClean    int
}

func (s *stubWorkerStore) CleanupExpiredSessionsByExpiry(context.Context, time.Duration, int) (int64, error) {
	return 0, nil
}
func (s *stubWorkerStore) CleanupRevokedSessions(context.Context, time.Duration, int) (int64, error) {
	return 0, nil
}
func (s *stubWorkerStore) CleanupDoneJobs(context.Context, time.Duration, int) (int64, error) {
	return 0, nil
}
func (s *stubWorkerStore) CleanupDeadJobs(context.Context, time.Duration, int) (int64, error) {
	return 0, nil
}
func (s *stubWorkerStore) CleanupExpiredInvitationsByExpiry(context.Context, time.Duration, int) (int64, error) {
	return 0, nil
}
func (s *stubWorkerStore) CleanupRevokedInvitations(context.Context, time.Duration, int) (int64, error) {
	return 0, nil
}
func (s *stubWorkerStore) CleanupSupersededInvitations(context.Context, time.Duration, int) (int64, error) {
	return 0, nil
}
func (s *stubWorkerStore) EnsureAuditPartitions(context.Context, time.Time, int) (int, error) {
	s.auditCalls++
	return 0, nil
}
func (s *stubWorkerStore) DropExpiredAuditPartitions(context.Context, time.Time, int) (int64, error) {
	return 0, nil
}
func (s *stubWorkerStore) CleanupWebhookDeliveries(context.Context, time.Duration, int) (int64, error) {
	s.whClean++
	return 0, nil
}
func (s *stubWorkerStore) PurgeLinkStatsBatch(context.Context, int64, int) (int64, error) {
	s.purgeCalls++
	return 0, nil
}
func (s *stubWorkerStore) EnsureStatsPartitions(context.Context, time.Time, int) (int, error) {
	s.statsCalls++
	return 0, nil
}
func (s *stubWorkerStore) DropExpiredStatsPartitions(context.Context, time.Time, int) (int64, error) {
	return 0, nil
}

type stubReconciler struct {
	calls int
	codes []string
}

func (s *stubReconciler) Reconcile(_ context.Context, code string) error {
	s.calls++
	s.codes = append(s.codes, code)
	return nil
}

func TestAssembleWorkerModulesAllEnabled(t *testing.T) {
	store := &stubWorkerStore{}
	reconciler := &stubReconciler{}
	var riskCalls, whCalls, emailCalls int

	handlers, periodic, err := assembleWorkerModules(
		nil,
		coreWorkerModule{
			store:               store,
			cleanupInterval:     time.Hour,
			sessionRetention:    time.Hour,
			doneJobRetention:    time.Hour,
			deadJobRetention:    time.Hour,
			invitationRetention: time.Hour,
			auditRetentionDays:  180,
		},
		riskWorkerModule{enabled: true, handler: func(context.Context, job.Job) error { riskCalls++; return nil }},
		webhookWorkerModule{
			enabled:         true,
			handler:         func(context.Context, job.Job) error { whCalls++; return nil },
			cleaner:         store,
			cleanupInterval: time.Hour,
			retention:       time.Hour,
		},
		emailWorkerModule{enabled: true, handler: func(context.Context, job.Job) error { emailCalls++; return nil }},
		cacheWorkerModule{reconciler: reconciler},
		analyticsWorkerModule{
			enabled:         true,
			store:           store,
			cleanupInterval: time.Hour,
			retentionDays:   90,
		},
	)
	if err != nil {
		t.Fatalf("assembleWorkerModules: %v", err)
	}

	if err := handlers[job.KindLinkRiskScan](context.Background(), job.Job{}); err != nil {
		t.Fatalf("risk handler: %v", err)
	}
	if err := handlers[job.KindWebhookDeliver](context.Background(), job.Job{}); err != nil {
		t.Fatalf("webhook handler: %v", err)
	}
	if err := handlers[job.KindInvitationEmail](context.Background(), job.Job{}); err != nil {
		t.Fatalf("email handler: %v", err)
	}
	if err := handlers[job.KindCacheReconcile](context.Background(), job.Job{Payload: []byte(`{"code":"AbC12345"}`)}); err != nil {
		t.Fatalf("cache handler: %v", err)
	}
	if err := handlers[job.KindLinkPurgeStats](context.Background(), job.Job{Payload: []byte(`{"link_id":7}`)}); err != nil {
		t.Fatalf("purge stats handler: %v", err)
	}
	if riskCalls != 1 || whCalls != 1 || emailCalls != 1 || reconciler.calls != 1 || store.purgeCalls != 1 {
		t.Fatalf("handler calls = risk:%d wh:%d email:%d cache:%d purge:%d, want all 1",
			riskCalls, whCalls, emailCalls, reconciler.calls, store.purgeCalls)
	}

	names := make([]string, 0, len(periodic))
	for _, p := range periodic {
		names = append(names, p.Name)
	}
	for _, want := range []string{
		"sessions.cleanup_expired",
		"sessions.cleanup_revoked",
		"jobs.cleanup_done",
		"jobs.cleanup_dead",
		"invitations.cleanup",
		"audit_partitions.maintain",
		"webhooks.cleanup_deliveries",
		"stats_partitions.maintain",
	} {
		if !slices.Contains(names, want) {
			t.Errorf("missing periodic task %q in %v", want, names)
		}
	}
}

func TestAssembleWorkerModulesDisabledReturnSkipWithoutSideEffects(t *testing.T) {
	store := &stubWorkerStore{}
	var realHandlerCalls int
	shouldNotRun := func(context.Context, job.Job) error {
		realHandlerCalls++
		return nil
	}

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))

	handlers, periodic, err := assembleWorkerModules(
		logger,
		coreWorkerModule{
			store:               store,
			cleanupInterval:     time.Hour,
			sessionRetention:    time.Hour,
			doneJobRetention:    time.Hour,
			deadJobRetention:    time.Hour,
			invitationRetention: time.Hour,
			auditRetentionDays:  180,
		},
		riskWorkerModule{enabled: false, handler: shouldNotRun},
		webhookWorkerModule{enabled: false, handler: shouldNotRun, cleaner: store, cleanupInterval: time.Hour, retention: time.Hour},
		emailWorkerModule{enabled: false, handler: shouldNotRun},
		cacheWorkerModule{reconciler: nil},
		analyticsWorkerModule{enabled: false, store: store, cleanupInterval: time.Hour, retentionDays: 90},
	)
	if err != nil {
		t.Fatalf("assembleWorkerModules: %v", err)
	}

	// 关闭的特性必须仍注册对应 kind 并记日志返回 job.Skip，防止残留任务落入 dead。
	disabledKinds := []struct {
		kind    string
		feature string
	}{
		{job.KindLinkRiskScan, "risk_scan"},
		{job.KindWebhookDeliver, "webhooks"},
		{job.KindInvitationEmail, "email"},
		{job.KindCacheReconcile, "cache"},
		{job.KindLinkPurgeStats, "analytics"},
	}
	for _, tc := range disabledKinds {
		h, ok := handlers[tc.kind]
		if !ok {
			t.Fatalf("missing disabled skip handler for %q", tc.kind)
		}
		err := h(context.Background(), job.Job{Kind: tc.kind, Payload: []byte(`{"link_id":1,"code":"AbC12345"}`)})
		if !job.IsSkipped(err) {
			t.Errorf("%s error = %v, want job.Skip", tc.kind, err)
		}
		if job.IsPermanent(err) {
			t.Errorf("%s error must not be job.Permanent", tc.kind)
		}
		if !strings.Contains(err.Error(), tc.feature) {
			t.Errorf("%s skip reason = %q, want feature name %q", tc.kind, err.Error(), tc.feature)
		}
		if !strings.Contains(logBuf.String(), "feature="+tc.feature) {
			t.Errorf("skip log missing feature=%s: %s", tc.feature, logBuf.String())
		}
	}
	if realHandlerCalls != 0 || store.purgeCalls != 0 || store.whClean != 0 || store.statsCalls != 0 {
		t.Fatalf("disabled modules produced unexpected side effects: real=%d purge=%d whClean=%d stats=%d",
			realHandlerCalls, store.purgeCalls, store.whClean, store.statsCalls)
	}

	// 关闭 webhooks 与 analytics 后，不再注册它们的周期维护任务，但审计分区维护保持启用。
	names := make([]string, 0, len(periodic))
	for _, p := range periodic {
		names = append(names, p.Name)
	}
	if slices.Contains(names, "webhooks.cleanup_deliveries") {
		t.Errorf("disabled webhooks still registered periodic cleanup: %v", names)
	}
	if slices.Contains(names, "stats_partitions.maintain") {
		t.Errorf("disabled analytics still registered stats partition maintenance: %v", names)
	}
	if !slices.Contains(names, "audit_partitions.maintain") {
		t.Errorf("core audit partition maintenance missing: %v", names)
	}
}

func TestAssembleWorkerModulesValidation(t *testing.T) {
	store := &stubWorkerStore{}
	noop := func(context.Context, job.Job) error { return nil }

	tests := []struct {
		name    string
		modules []WorkerModule
	}{
		{
			name:    "duplicate risk module",
			modules: []WorkerModule{riskWorkerModule{enabled: false}, riskWorkerModule{enabled: false}},
		},
		{
			name: "duplicate core module",
			modules: []WorkerModule{
				coreWorkerModule{store: store, cleanupInterval: time.Hour, auditRetentionDays: 180},
				coreWorkerModule{store: store, cleanupInterval: time.Hour, auditRetentionDays: 180},
			},
		},
		{
			name:    "nil core store",
			modules: []WorkerModule{coreWorkerModule{cleanupInterval: time.Hour, auditRetentionDays: 180}},
		},
		{
			name:    "enabled webhook module with nil cleaner",
			modules: []WorkerModule{webhookWorkerModule{enabled: true, handler: noop, cleanupInterval: time.Hour}},
		},
		{
			name:    "enabled analytics module with nil store",
			modules: []WorkerModule{analyticsWorkerModule{enabled: true, cleanupInterval: time.Hour, retentionDays: 90}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, _, err := assembleWorkerModules(nil, tt.modules...); err == nil {
				t.Fatalf("assembleWorkerModules(%s) = nil, want error", tt.name)
			}
		})
	}
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type stubJobQueue struct {
	mu        sync.Mutex
	pending   []job.Job
	completed []int64
	retried   []int64
	dead      []int64
	doneOnce  sync.Once
	doneCh    chan struct{}
	wantDone  int
}

func (q *stubJobQueue) Claim(_ context.Context, p job.ClaimParams) ([]job.Job, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.pending) == 0 {
		return nil, nil
	}
	n := min(p.Limit, len(q.pending))
	out := make([]job.Job, n)
	copy(out, q.pending[:n])
	q.pending = q.pending[n:]
	return out, nil
}

func (q *stubJobQueue) Complete(_ context.Context, id int64, _ string, _ int64) error {
	q.mu.Lock()
	q.completed = append(q.completed, id)
	done := len(q.completed) >= q.wantDone
	q.mu.Unlock()
	if done && q.doneCh != nil {
		q.doneOnce.Do(func() { close(q.doneCh) })
	}
	return nil
}

func (q *stubJobQueue) Retry(_ context.Context, p job.RetryParams) error {
	q.mu.Lock()
	q.retried = append(q.retried, p.ID)
	q.mu.Unlock()
	return nil
}

func (q *stubJobQueue) Dead(_ context.Context, id int64, _ string, _ int64, _ string) error {
	q.mu.Lock()
	q.dead = append(q.dead, id)
	q.mu.Unlock()
	return nil
}

func (q *stubJobQueue) ReapExpired(context.Context, time.Duration, int) ([]job.ReapedRecord, error) {
	return nil, nil
}

func (q *stubJobQueue) OldestPendingAges(context.Context) (map[string]time.Duration, error) {
	return map[string]time.Duration{}, nil
}

/*
TestDisabledWorkerModulesThroughRunnerMarkDoneAndRecordSkippedMetric 验证 ADR 0029 阶段 C 验收路径：
预置历史 webhook.deliver 与 link.risk_scan pending 任务后，关闭对应开关通过真实 job.Runner 消费，
任务被 disabledSkipHandler 记日志并以 job.Skip 标记为 done，指标记录为 result="skipped"，既不进 dead 也不 Defer。
*/
func TestDisabledWorkerModulesThroughRunnerMarkDoneAndRecordSkippedMetric(t *testing.T) {
	var logBuf syncBuffer
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))
	registry := prometheus.NewRegistry()

	handlers, _, err := assembleWorkerModules(
		logger,
		riskWorkerModule{enabled: false},
		webhookWorkerModule{enabled: false},
	)
	if err != nil {
		t.Fatalf("assembleWorkerModules: %v", err)
	}

	tenantID := int64(42)
	q := &stubJobQueue{
		pending: []job.Job{
			{ID: 101, Kind: job.KindWebhookDeliver, TenantID: &tenantID, Payload: []byte(`{"webhook_id":1}`), Attempts: 1, MaxAttempts: 5, LockVersion: 1},
			{ID: 102, Kind: job.KindLinkRiskScan, TenantID: &tenantID, Payload: []byte(`{"link_id":2}`), Attempts: 1, MaxAttempts: 5, LockVersion: 1},
		},
		doneCh:   make(chan struct{}),
		wantDone: 2,
	}

	runner := job.New(q, handlers, logger, registry, job.Options{
		Concurrency:     2,
		PollInterval:    5 * time.Millisecond,
		ReapInterval:    time.Hour,
		MetricsInterval: time.Hour,
		ShutdownTimeout: 2 * time.Second,
	})

	ctx, cancel := context.WithCancel(context.Background())
	runErrCh := make(chan error, 1)
	go func() {
		runErrCh <- runner.Run(ctx)
	}()

	select {
	case <-q.doneCh:
		cancel()
	case <-time.After(3 * time.Second):
		cancel()
		t.Fatal("timed out waiting for disabled jobs to complete")
	}
	if err := <-runErrCh; err != nil {
		t.Fatalf("runner.Run: %v", err)
	}

	q.mu.Lock()
	completed := slices.Clone(q.completed)
	retried := slices.Clone(q.retried)
	dead := slices.Clone(q.dead)
	q.mu.Unlock()

	slices.Sort(completed)
	if !slices.Equal(completed, []int64{101, 102}) {
		t.Fatalf("completed job IDs = %v, want [101 102]", completed)
	}
	if len(retried) != 0 || len(dead) != 0 {
		t.Fatalf("disabled jobs must not retry or dead: retried=%v dead=%v", retried, dead)
	}

	for _, kind := range []string{job.KindWebhookDeliver, job.KindLinkRiskScan} {
		if got := counterValue(t, registry, "shortlink_jobs_processed_total", map[string]string{"kind": kind, "result": "skipped"}); got != 1 {
			t.Errorf("shortlink_jobs_processed_total{kind=%q,result=skipped} = %v, want 1", kind, got)
		}
		if got := counterValue(t, registry, "shortlink_jobs_processed_total", map[string]string{"kind": kind, "result": "dead"}); got != 0 {
			t.Errorf("shortlink_jobs_processed_total{kind=%q,result=dead} = %v, want 0", kind, got)
		}
		if got := counterValue(t, registry, "shortlink_jobs_processed_total", map[string]string{"kind": kind, "result": "retry"}); got != 0 {
			t.Errorf("shortlink_jobs_processed_total{kind=%q,result=retry} = %v, want 0", kind, got)
		}
	}

	logs := logBuf.String()
	for _, want := range []string{"job skipped because feature is disabled", "feature=webhooks", "feature=risk_scan", "job_id=101", "job_id=102"} {
		if !strings.Contains(logs, want) {
			t.Errorf("runner logs missing %q:\n%s", want, logs)
		}
	}
}

func counterValue(t *testing.T, reg *prometheus.Registry, metricName string, wantLabels map[string]string) float64 {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}
	for _, mf := range families {
		if mf.GetName() != metricName {
			continue
		}
		for _, m := range mf.GetMetric() {
			if labelsMatch(m.GetLabel(), wantLabels) {
				return m.GetCounter().GetValue()
			}
		}
	}
	return 0
}

func labelsMatch(pairs []*dto.LabelPair, want map[string]string) bool {
	if len(pairs) != len(want) {
		return false
	}
	for _, lp := range pairs {
		if v, ok := want[lp.GetName()]; !ok || v != lp.GetValue() {
			return false
		}
	}
	return true
}
