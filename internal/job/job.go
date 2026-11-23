/*
Package job 实现基于数据库 Outbox 的通用可靠后台任务引擎（阶段 12、ADR 0020）。

本包不依赖 PostgreSQL 或 Redis，只通过 Queue 和 Handler 接口描述任务抢占、
状态流转、指数退避重试、租约回收、周期维护任务与优雅退出。
*/
package job

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"
)

const (
	StatusPending = "pending"
	StatusRunning = "running"
	StatusDone    = "done"
	StatusDead    = "dead"

	// KindLinkRiskScan 是创建或修改短链 URL 后触发的异步风控扫描任务。
	KindLinkRiskScan = "link.risk_scan"

	DefaultMaxAttempts = 5
)

// Job 是队列中的一条后台任务记录。
type Job struct {
	ID          int64
	Kind        string
	TenantID    *int64
	Payload     []byte
	DedupKey    string
	Status      string
	Attempts    int
	MaxAttempts int
	RunAt       time.Time
	LockedAt    *time.Time
	LockedUntil *time.Time
	LockedBy    string
	LastError   string
	CreatedAt   time.Time
	UpdatedAt   time.Time
	FinishedAt  *time.Time
}

// Handler 处理单条已抢占的任务。返回值通过 Permanent、Skip、Defer 区分终态和重试语义。
type Handler func(ctx context.Context, j Job) error

// ClaimParams 控制一次批量抢占的条数、单租户并发软上限与租约参数。
type ClaimParams struct {
	Limit        int
	MaxPerTenant int
	Lease        time.Duration
	LockedBy     string
}

/*
RetryParams 描述一次失败或延迟后的任务归还。
ConsumeAttempt 为 false 时把本次抢占增加的 attempts 退回，用于目标 host 限速等非故障延迟，
避免正常任务因短暂限速耗尽重试次数进入 dead。
*/
type RetryParams struct {
	ID             int64
	LockedBy       string
	Delay          time.Duration
	LastError      string
	ConsumeAttempt bool
}

// ReapedRecord 记录一条被 Reaper 回收的超时租约任务，用于统计指标。
type ReapedRecord struct {
	Kind   string
	Status string // pending 或 dead
}

// Queue 是 Runner 依赖的持久化任务队列能力，由存储层实现。
type Queue interface {
	Claim(ctx context.Context, p ClaimParams) ([]Job, error)
	Complete(ctx context.Context, id int64, lockedBy string) error
	Retry(ctx context.Context, p RetryParams) error
	Dead(ctx context.Context, id int64, lockedBy, lastError string) error
	ReapExpired(ctx context.Context, retryDelay time.Duration) ([]ReapedRecord, error)
	OldestPendingAges(ctx context.Context) (map[string]time.Duration, error)
}

// PeriodicTask 描述随 worker 进程运行的周期性维护任务（例如分批清理过期会话与已完成任务）。
type PeriodicTask struct {
	Name     string
	Interval time.Duration
	Run      func(ctx context.Context) (int64, error)
}

type permanentError struct {
	err error
}

func (e permanentError) Error() string { return e.err.Error() }
func (e permanentError) Unwrap() error { return e.err }

// Permanent 标记不可重试错误；Runner 会直接把任务送进 dead 终态。
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return permanentError{err: err}
}

// IsPermanent 报告 err 是否被标记为不可重试。
func IsPermanent(err error) bool {
	_, ok := errors.AsType[permanentError](err)
	return ok
}

type skipError struct {
	reason string
}

func (e skipError) Error() string {
	if e.reason == "" {
		return "job skipped"
	}
	return "job skipped: " + e.reason
}

// Skip 表示任务已失去执行前提（如链接被删除或 URL 已变更），按 done 结束并记为 skipped。
func Skip(reason string) error {
	return skipError{reason: reason}
}

// IsSkipped 报告 err 是否表示显式跳过。
func IsSkipped(err error) bool {
	_, ok := errors.AsType[skipError](err)
	return ok
}

type deferError struct {
	delay  time.Duration
	reason string
}

func (e deferError) Error() string {
	if e.reason == "" {
		return fmt.Sprintf("job deferred for %s", e.delay)
	}
	return fmt.Sprintf("job deferred for %s: %s", e.delay, e.reason)
}

/*
Defer 请求把任务延迟指定时长后重新排队，且不消耗重试次数。
用于目标 host 限速等流控场景，避免短暂流控把健康任务打入 dead。
*/
func Defer(delay time.Duration, reason string) error {
	if delay < 0 {
		delay = 0
	}
	return deferError{delay: delay, reason: reason}
}

// DeferDelay 取出 Defer 指定的延迟时长。
func DeferDelay(err error) (time.Duration, bool) {
	de, ok := errors.AsType[deferError](err)
	if !ok {
		return 0, false
	}
	return de.delay, true
}

/*
Backoff 计算第 attempt 次尝试失败后的指数退避时间，并在 [0.8, 1.0] 区间内向下随机抖动，
防止大批同时失败的任务在下一轮同一秒齐刷刷重试。
*/
func Backoff(attempt int, base, maxDelay time.Duration) time.Duration {
	if base <= 0 {
		base = 2 * time.Second
	}
	if maxDelay < base {
		maxDelay = base
	}
	if attempt < 1 {
		attempt = 1
	}

	delay := base
	for range attempt - 1 {
		if delay >= maxDelay/2 {
			delay = maxDelay
			break
		}
		delay *= 2
	}
	if delay > maxDelay {
		delay = maxDelay
	}

	jitterWindow := delay / 5
	if jitterWindow <= 0 {
		return delay
	}
	return delay - rand.N(jitterWindow)
}
