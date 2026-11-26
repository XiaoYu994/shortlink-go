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
	// KindLinkPurgeStats 是删除短链后异步分批清理三张日聚合统计表的任务（ADR 0025）。
	KindLinkPurgeStats = "link.purge_stats"
	// KindWebhookDeliver 是向租户订阅端点投递生命周期事件或 ping 的异步任务（ADR 0027）。
	KindWebhookDeliver = "webhook.deliver"
	// KindInvitationEmail 是创建组织成员邀请后异步发送通知邮件的任务（ADR 0027）。
	KindInvitationEmail = "invitation.email"

	DefaultMaxAttempts   = 5
	DefaultReapBatchSize = 500
)

// ErrLeaseLost 表示任务租约已过期、被回收或被其他抢占接管，当前回写被 Fencing Token 拒绝。
var ErrLeaseLost = errors.New("job lease lost")

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
	LockVersion int64
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
LockVersion 是抢占时获得的单调递增 Fencing Token，防止陈旧执行者覆盖新租约。
*/
type RetryParams struct {
	ID             int64
	LockedBy       string
	LockVersion    int64
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
	Complete(ctx context.Context, id int64, lockedBy string, lockVersion int64) error
	Retry(ctx context.Context, p RetryParams) error
	Dead(ctx context.Context, id int64, lockedBy string, lockVersion int64, lastError string) error
	ReapExpired(ctx context.Context, retryDelay time.Duration, limit int) ([]ReapedRecord, error)
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

type retryAfterError struct {
	delay time.Duration
	err   error
}

func (e retryAfterError) Error() string { return e.err.Error() }
func (e retryAfterError) Unwrap() error { return e.err }

/*
RetryAfter 为失败任务指定自定义下次退避时长，同时照常消耗一次重试次数（与 Defer 区分）。
用于 webhook.deliver 等拥有独立重试节拍的任务（ADR 0027）。
*/
func RetryAfter(delay time.Duration, err error) error {
	if err == nil {
		return nil
	}
	if delay < 0 {
		delay = 0
	}
	return retryAfterError{delay: delay, err: err}
}

// RetryAfterDelay 取出 RetryAfter 指定的退避时长。
func RetryAfterDelay(err error) (time.Duration, bool) {
	re, ok := errors.AsType[retryAfterError](err)
	if !ok {
		return 0, false
	}
	return re.delay, true
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
