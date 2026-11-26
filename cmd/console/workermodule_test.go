package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"shortlink/internal/console"
	"shortlink/internal/identity"
	"shortlink/internal/job"
	"shortlink/internal/link"
	"shortlink/internal/mailer"
	"shortlink/internal/sms"
	"shortlink/internal/webhook"
)

const testWebhookSecretHex = "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"

type stubWorkerStore struct {
	purgeCalls int
	statsCalls int
	auditCalls int
	whClean    int
	otpClean   int
	banCalls   int
	whReads    int
	emailReads int
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
func (s *stubWorkerStore) CleanupExpiredPhoneOTPChallenges(context.Context, time.Duration, int) (int64, error) {
	s.otpClean++
	return 0, nil
}
func (s *stubWorkerStore) CleanupWebhookDeliveries(context.Context, time.Duration, int) (int64, error) {
	s.whClean++
	return 0, nil
}
func (s *stubWorkerStore) GetWebhookForDelivery(context.Context, int64) (webhook.DeliveryTarget, error) {
	s.whReads++
	return webhook.DeliveryTarget{}, webhook.ErrNotFound
}
func (s *stubWorkerStore) RecordWebhookDelivery(context.Context, webhook.RecordDeliveryParams) (bool, error) {
	return false, nil
}
func (s *stubWorkerStore) GetInvitationEmailTarget(context.Context, int64) (mailer.InvitationEmailTarget, error) {
	s.emailReads++
	return mailer.InvitationEmailTarget{}, identity.ErrInvitationNotFound
}
func (s *stubWorkerStore) BanIfURL(context.Context, string, string, link.BanReason, link.BanSource) (*link.Link, error) {
	s.banCalls++
	return nil, nil
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

type stubPhoneOTPConsoleStore struct{}

func (stubPhoneOTPConsoleStore) CreatePhoneOTPChallenge(context.Context, identity.PhoneOTPChallengeParams) (int64, error) {
	return 1, nil
}
func (stubPhoneOTPConsoleStore) DeleteUnsentPhoneOTPChallenge(context.Context, int64) error {
	return nil
}
func (stubPhoneOTPConsoleStore) VerifyPhoneOTPChallenge(context.Context, string, string) error {
	return nil
}

func TestAssembleWorkerModulesAllEnabled(t *testing.T) {
	store := &stubWorkerStore{}
	reconciler := &stubReconciler{}
	reg := prometheus.NewRegistry()

	assembled, err := assembleWorkerModules(
		nil,
		reg,
		coreWorkerModule{
			store:               store,
			cleanupInterval:     time.Hour,
			sessionRetention:    time.Hour,
			doneJobRetention:    time.Hour,
			deadJobRetention:    time.Hour,
			invitationRetention: time.Hour,
			auditRetentionDays:  180,
		},
		phoneOTPWorkerModule{
			cleaner:         store,
			cleanupInterval: time.Hour,
			retention:       24 * time.Hour,
		},
		riskWorkerModule{
			enabled:      true,
			banner:       store,
			fetchTimeout: time.Second,
			blockedHosts: map[string]link.BanReason{"evil.example": link.BanReasonPhishing},
		},
		webhookWorkerModule{
			secretKey:       testWebhookSecretHex,
			store:           store,
			timeout:         time.Second,
			cleanupInterval: time.Hour,
			retention:       time.Hour,
			registry:        reg,
		},
		emailWorkerModule{
			smtp: mailer.SMTPConfig{
				Host:    "smtp.example.com",
				Port:    587,
				From:    "noreply@example.com",
				TLSMode: mailer.TLSModeStartTLS,
				Timeout: time.Second,
			},
			store: store,
		},
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
	handlers, periodic := assembled.handlers, assembled.periodic

	if err := assembled.runStartup(context.Background()); err != nil {
		t.Fatalf("runStartup: %v", err)
	}
	if store.auditCalls != 1 || store.statsCalls != 1 {
		t.Fatalf("startup calls = audit:%d stats:%d, want 1/1", store.auditCalls, store.statsCalls)
	}

	if err := handlers[job.KindLinkRiskScan](context.Background(), job.Job{Payload: []byte(`{"link_id":1,"code":"AbC12345","url":"https://evil.example/a"}`)}); err != nil {
		t.Fatalf("risk handler: %v", err)
	}
	if err := handlers[job.KindWebhookDeliver](context.Background(), job.Job{Payload: []byte(`{"webhook_id":1,"event_id":"evt-1","event":"link.created","data":{}}`)}); !job.IsSkipped(err) {
		t.Fatalf("webhook handler on missing target = %v, want job.Skip", err)
	}
	if err := handlers[job.KindInvitationEmail](context.Background(), job.Job{Payload: []byte(`{"invitation_id":1}`)}); !job.IsSkipped(err) {
		t.Fatalf("email handler on missing invitation = %v, want job.Skip", err)
	}
	if err := handlers[job.KindCacheReconcile](context.Background(), job.Job{Payload: []byte(`{"code":"AbC12345"}`)}); err != nil {
		t.Fatalf("cache handler: %v", err)
	}
	if err := handlers[job.KindLinkPurgeStats](context.Background(), job.Job{Payload: []byte(`{"link_id":7}`)}); err != nil {
		t.Fatalf("purge stats handler: %v", err)
	}
	if store.banCalls != 1 || store.whReads != 1 || store.emailReads != 1 || reconciler.calls != 1 || store.purgeCalls != 1 {
		t.Fatalf("handler calls = ban:%d wh:%d email:%d cache:%d purge:%d, want all 1",
			store.banCalls, store.whReads, store.emailReads, reconciler.calls, store.purgeCalls)
	}

	names := make([]string, 0, len(periodic))
	for _, p := range periodic {
		names = append(names, p.Name)
		if p.Name == "phone_otp.cleanup" {
			if _, err := p.Run(context.Background()); err != nil {
				t.Fatalf("phone_otp.cleanup: %v", err)
			}
		}
	}
	if store.otpClean != 1 {
		t.Fatalf("otpClean = %d, want 1", store.otpClean)
	}
	for _, want := range []string{
		"sessions.cleanup_expired",
		"sessions.cleanup_revoked",
		"jobs.cleanup_done",
		"jobs.cleanup_dead",
		"invitations.cleanup",
		"audit_partitions.maintain",
		"phone_otp.cleanup",
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
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))

	assembled, err := assembleWorkerModules(
		logger,
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
		phoneOTPWorkerModule{cleaner: store, cleanupInterval: time.Hour, retention: 24 * time.Hour},
		riskWorkerModule{enabled: false, banner: store},
		webhookWorkerModule{secretKey: "", store: store, cleanupInterval: time.Hour, retention: time.Hour},
		emailWorkerModule{smtp: mailer.SMTPConfig{}, store: store},
		cacheWorkerModule{reconciler: nil},
		analyticsWorkerModule{enabled: false, store: store, cleanupInterval: time.Hour, retentionDays: 90},
	)
	if err != nil {
		t.Fatalf("assembleWorkerModules: %v", err)
	}
	handlers, periodic := assembled.handlers, assembled.periodic

	if err := assembled.runStartup(context.Background()); err != nil {
		t.Fatalf("runStartup with analytics disabled: %v", err)
	}
	if store.auditCalls != 1 || store.statsCalls != 0 {
		t.Fatalf("startup calls with analytics disabled = audit:%d stats:%d, want 1/0", store.auditCalls, store.statsCalls)
	}

	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := assembled.runStartup(canceledCtx); !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "audit_partitions.ensure") {
		t.Fatalf("runStartup(canceled) = %v, want context.Canceled wrapped with hook name", err)
	}
	if store.auditCalls != 1 || store.statsCalls != 0 {
		t.Fatalf("runStartup(canceled) ran hooks: audit:%d stats:%d, want 1/0", store.auditCalls, store.statsCalls)
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
	if store.banCalls != 0 || store.whReads != 0 || store.emailReads != 0 || store.purgeCalls != 0 || store.whClean != 0 || store.statsCalls != 0 {
		t.Fatalf("disabled modules produced unexpected side effects: ban=%d wh=%d email=%d purge=%d whClean=%d stats=%d",
			store.banCalls, store.whReads, store.emailReads, store.purgeCalls, store.whClean, store.statsCalls)
	}

	// 关闭 webhooks 与 analytics 后，不再注册它们的周期维护任务，但审计分区与 OTP 清理保持启用。
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
	if !slices.Contains(names, "audit_partitions.maintain") || !slices.Contains(names, "phone_otp.cleanup") {
		t.Errorf("core maintenance tasks missing: %v", names)
	}
}

func TestAssembleWorkerModulesValidation(t *testing.T) {
	store := &stubWorkerStore{}

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
			name:    "nil phone otp cleaner",
			modules: []WorkerModule{phoneOTPWorkerModule{cleanupInterval: time.Hour, retention: time.Hour}},
		},
		{
			name:    "enabled risk module with nil banner",
			modules: []WorkerModule{riskWorkerModule{enabled: true}},
		},
		{
			name:    "enabled webhook module with nil store",
			modules: []WorkerModule{webhookWorkerModule{secretKey: testWebhookSecretHex, cleanupInterval: time.Hour}},
		},
		{
			name:    "enabled webhook module with invalid secret key",
			modules: []WorkerModule{webhookWorkerModule{secretKey: "not-hex", store: store, cleanupInterval: time.Hour}},
		},
		{
			name:    "enabled email module with nil store",
			modules: []WorkerModule{emailWorkerModule{smtp: mailer.SMTPConfig{Host: "smtp.example.com", From: "noreply@example.com"}}},
		},
		{
			name:    "enabled email module with invalid smtp config",
			modules: []WorkerModule{emailWorkerModule{smtp: mailer.SMTPConfig{Host: "smtp.example.com"}, store: store}},
		},
		{
			name:    "enabled analytics module with nil store",
			modules: []WorkerModule{analyticsWorkerModule{enabled: true, cleanupInterval: time.Hour, retentionDays: 90}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := assembleWorkerModules(nil, nil, tt.modules...); err == nil {
				t.Fatalf("assembleWorkerModules(%s) = nil, want error", tt.name)
			}
		})
	}
}

func TestInitPhoneOTPAuthMethod(t *testing.T) {
	store := stubPhoneOTPConsoleStore{}

	m, err := initPhoneOTPAuthMethod(PhoneOTPConfig{}, store, nil, nil)
	if err != nil || m != nil {
		t.Fatalf("disabled initPhoneOTPAuthMethod = (%v, %v), want (nil, nil)", m, err)
	}

	m, err = initPhoneOTPAuthMethod(PhoneOTPConfig{Provider: sms.ProviderLog}, store, nil, nil)
	if err != nil || m == nil || m.Descriptor().Kind != console.AuthFlowPhoneOTP {
		t.Fatalf("log initPhoneOTPAuthMethod = (%v, %v)", m, err)
	}

	// webhook 指向回环地址且无 devtest allowlist 时必须在启动期拒绝。
	if _, err := initPhoneOTPAuthMethod(PhoneOTPConfig{
		Provider:   sms.ProviderWebhook,
		WebhookURL: "http://127.0.0.1:18092/sms",
	}, store, nil, nil); err == nil {
		t.Fatal("webhook loopback without allowlist = nil, want error")
	}

	// 显式 allowTestHosts 放行后构造成功。
	m, err = initPhoneOTPAuthMethod(PhoneOTPConfig{
		Provider:   sms.ProviderWebhook,
		WebhookURL: "http://127.0.0.1:18092/sms",
	}, store, []string{"127.0.0.1:18092"}, nil)
	if err != nil || m == nil || m.Descriptor().SendCodeURL != "/api/v1/auth/phone/send-code" {
		t.Fatalf("webhook with allowlist = (%v, %v)", m, err)
	}
}

type stubStartupModule struct {
	name     string
	hookName string
	hookFn   func(context.Context) error
}

func (m stubStartupModule) Name() string { return m.name }
func (m stubStartupModule) RegisterWorker(r WorkerRegistrar) error {
	r.OnStart(m.hookName, m.hookFn)
	return nil
}

type stubDisabledModule struct {
	name    string
	kind    string
	feature string
}

func (m stubDisabledModule) Name() string { return m.name }
func (m stubDisabledModule) RegisterWorker(r WorkerRegistrar) error {
	r.HandleDisabled(m.kind, m.feature)
	return nil
}

func TestAssembleWorkerModulesStartupValidation(t *testing.T) {
	noop := func(context.Context) error { return nil }
	for _, tt := range []struct {
		name    string
		modules []WorkerModule
	}{
		{
			name:    "empty startup hook name",
			modules: []WorkerModule{stubStartupModule{name: "s1", hookName: "", hookFn: noop}},
		},
		{
			name:    "nil startup hook fn",
			modules: []WorkerModule{stubStartupModule{name: "s1", hookName: "init", hookFn: nil}},
		},
		{
			name: "duplicate startup hook name",
			modules: []WorkerModule{
				stubStartupModule{name: "s1", hookName: "dup", hookFn: noop},
				stubStartupModule{name: "s2", hookName: "dup", hookFn: noop},
			},
		},
		{
			name:    "empty disabled feature name",
			modules: []WorkerModule{stubDisabledModule{name: "d1", kind: "demo.job", feature: ""}},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			reg := prometheus.NewRegistry()
			if _, err := assembleWorkerModules(nil, reg, tt.modules...); err == nil {
				t.Fatalf("assembleWorkerModules(%s) = nil, want error", tt.name)
			}
			families, err := reg.Gather()
			if err != nil {
				t.Fatalf("gather registry: %v", err)
			}
			if len(families) != 0 {
				t.Fatalf("failed assembly polluted prometheus registry with %d metric families", len(families))
			}
		})
	}

	// 同一个 registry 重复装配时应返回 error 而不是 panic。
	reg := prometheus.NewRegistry()
	if _, err := assembleWorkerModules(nil, reg, riskWorkerModule{enabled: false}); err != nil {
		t.Fatalf("first assembleWorkerModules: %v", err)
	}
	if _, err := assembleWorkerModules(nil, reg, riskWorkerModule{enabled: false}); err == nil {
		t.Fatal("duplicate registry registration = nil, want error")
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

	assembled, err := assembleWorkerModules(
		logger,
		registry,
		riskWorkerModule{enabled: false},
		webhookWorkerModule{secretKey: ""},
	)
	if err != nil {
		t.Fatalf("assembleWorkerModules: %v", err)
	}
	handlers := assembled.handlers

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

	for _, tc := range []struct {
		kind    string
		feature string
	}{
		{job.KindWebhookDeliver, "webhooks"},
		{job.KindLinkRiskScan, "risk_scan"},
	} {
		if got := counterValue(t, registry, "shortlink_jobs_processed_total", map[string]string{"kind": tc.kind, "result": "skipped"}); got != 1 {
			t.Errorf("shortlink_jobs_processed_total{kind=%q,result=skipped} = %v, want 1", tc.kind, got)
		}
		if got := counterValue(t, registry, "shortlink_jobs_processed_total", map[string]string{"kind": tc.kind, "result": "dead"}); got != 0 {
			t.Errorf("shortlink_jobs_processed_total{kind=%q,result=dead} = %v, want 0", tc.kind, got)
		}
		if got := counterValue(t, registry, "shortlink_jobs_processed_total", map[string]string{"kind": tc.kind, "result": "retry"}); got != 0 {
			t.Errorf("shortlink_jobs_processed_total{kind=%q,result=retry} = %v, want 0", tc.kind, got)
		}
		if got := counterValue(t, registry, featureDisabledSkippedMetricName, map[string]string{"kind": tc.kind, "feature": tc.feature}); got != 1 {
			t.Errorf("%s{kind=%q,feature=%q} = %v, want 1", featureDisabledSkippedMetricName, tc.kind, tc.feature, got)
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
