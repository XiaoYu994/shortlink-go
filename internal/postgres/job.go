package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"shortlink/internal/job"
)

const (
	enqueueJobSQL = `INSERT INTO jobs (kind, tenant_id, payload, dedup_key, max_attempts, run_at)
VALUES ($1, $2, $3::jsonb, $4, $5, now() + ($6::bigint * interval '1 microsecond'))
RETURNING id`

	enqueueJobDedupSQL = `INSERT INTO jobs (kind, tenant_id, payload, dedup_key, max_attempts, run_at)
VALUES ($1, $2, $3::jsonb, $4, $5, now() + ($6::bigint * interval '1 microsecond'))
ON CONFLICT (kind, dedup_key) WHERE dedup_key IS NOT NULL AND status IN ('pending', 'running')
DO NOTHING
RETURNING id`

	/*
		两段式抢占 CTE（ADR 0020）：
		1. running_per_tenant 统计当前每个租户在途的 running 任务数（走 idx_jobs_running_tenant）。
		2. eligible 给每个租户的待执行任务按 run_at, id 编号，筛掉已经达到软上限的租户。
		3. candidate 再对 jobs 表本身执行 FOR UPDATE OF j SKIP LOCKED，保证多 worker 并发不重复抢占，
		   并再次校验 status = 'pending' AND run_at <= now()，避免并发修改的可见性窗口。
		按租户限并发是防饿死的软上限：多个 worker 同一毫秒并发抢占时可能短暂超出 1-2 个，
		换取不在抢占路径锁 tenants 行的低竞争开销。
	*/
	claimJobsSQL = `WITH running_per_tenant AS (
    SELECT tenant_id, count(*) AS cnt
    FROM jobs
    WHERE status = 'running' AND tenant_id IS NOT NULL
    GROUP BY tenant_id
),
eligible AS (
    SELECT j.id,
           CASE
               WHEN j.tenant_id IS NULL THEN 1
               ELSE row_number() OVER (PARTITION BY j.tenant_id ORDER BY j.run_at, j.id)
           END AS tenant_rn,
           COALESCE(r.cnt, 0) AS running_cnt
    FROM jobs j
    LEFT JOIN running_per_tenant r ON r.tenant_id = j.tenant_id
    WHERE j.status = 'pending'
      AND j.run_at <= now()
      AND ($2::int <= 0 OR j.tenant_id IS NULL OR COALESCE(r.cnt, 0) < $2)
),
candidate AS (
    SELECT j.id
    FROM jobs j
    JOIN eligible e ON e.id = j.id
    WHERE j.status = 'pending'
      AND j.run_at <= now()
      AND ($2::int <= 0 OR j.tenant_id IS NULL OR (e.running_cnt + e.tenant_rn) <= $2)
    ORDER BY j.run_at, j.id
    LIMIT $1
    FOR UPDATE OF j SKIP LOCKED
)
UPDATE jobs j
SET status = 'running',
    attempts = j.attempts + 1,
    locked_at = now(),
    locked_until = now() + ($3::bigint * interval '1 microsecond'),
    locked_by = $4,
    updated_at = now()
FROM candidate c
WHERE j.id = c.id
RETURNING j.id, j.kind, j.tenant_id, j.payload, j.dedup_key, j.status, j.attempts, j.max_attempts,
          j.run_at, j.locked_at, j.locked_until, j.locked_by, j.last_error,
          j.created_at, j.updated_at, j.finished_at`

	completeJobSQL = `UPDATE jobs
SET status = 'done',
    locked_at = NULL,
    locked_until = NULL,
    locked_by = NULL,
    last_error = '',
    finished_at = now(),
    updated_at = now()
WHERE id = $1 AND status = 'running' AND locked_by = $2`

	retryJobSQL = `UPDATE jobs
SET status = 'pending',
    attempts = CASE WHEN $5 THEN attempts ELSE GREATEST(attempts - 1, 0) END,
    run_at = now() + ($3::bigint * interval '1 microsecond'),
    locked_at = NULL,
    locked_until = NULL,
    locked_by = NULL,
    last_error = $4,
    updated_at = now()
WHERE id = $1 AND status = 'running' AND locked_by = $2`

	deadJobSQL = `UPDATE jobs
SET status = 'dead',
    locked_at = NULL,
    locked_until = NULL,
    locked_by = NULL,
    last_error = $3,
    finished_at = now(),
    updated_at = now()
WHERE id = $1 AND status = 'running' AND locked_by = $2`

	reapExpiredJobsSQL = `WITH expired AS (
    SELECT id
    FROM jobs
    WHERE status = 'running' AND locked_until < now()
    FOR UPDATE SKIP LOCKED
)
UPDATE jobs j
SET status = CASE WHEN j.attempts >= j.max_attempts THEN 'dead' ELSE 'pending' END,
    run_at = CASE WHEN j.attempts >= j.max_attempts THEN j.run_at ELSE now() + ($1::bigint * interval '1 microsecond') END,
    locked_at = NULL,
    locked_until = NULL,
    locked_by = NULL,
    last_error = CASE WHEN j.last_error = '' THEN 'lease expired' ELSE j.last_error END,
    finished_at = CASE WHEN j.attempts >= j.max_attempts THEN now() ELSE NULL END,
    updated_at = now()
FROM expired e
WHERE j.id = e.id
RETURNING j.kind, j.status`

	oldestPendingAgesSQL = `SELECT kind, GREATEST(EXTRACT(EPOCH FROM (now() - min(run_at))), 0)
FROM jobs
WHERE status = 'pending' AND run_at <= now()
GROUP BY kind`

	cleanupDoneJobsSQL = `DELETE FROM jobs
WHERE id IN (
    SELECT id
    FROM jobs
    WHERE status = 'done'
      AND finished_at < now() - ($1::bigint * interval '1 microsecond')
    ORDER BY finished_at, id
    LIMIT $2
)`
)

// EnqueueJobParams 是显式入队一条通用后台任务的参数。
type EnqueueJobParams struct {
	Kind        string
	TenantID    *int64
	Payload     any
	DedupKey    string
	MaxAttempts int
	Delay       time.Duration
}

/*
EnqueueJob 显式插入一条后台任务并返回任务编号。
当 DedupKey 非空且已存在同 kind 的未完成任务时，利用 uq_jobs_dedup_active 跳过插入并返回 (0, nil)。
*/
func (s *Store) EnqueueJob(ctx context.Context, p EnqueueJobParams) (int64, error) {
	if p.Kind == "" {
		return 0, errors.New("enqueue job: kind is required")
	}
	maxAttempts := p.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = job.DefaultMaxAttempts
	}
	if p.Delay < 0 {
		p.Delay = 0
	}

	rawPayload := []byte("{}")
	if p.Payload != nil {
		var err error
		rawPayload, err = json.Marshal(p.Payload)
		if err != nil {
			return 0, fmt.Errorf("enqueue job: marshal payload: %w", err)
		}
	}

	var dedup any
	query := enqueueJobSQL
	if p.DedupKey != "" {
		dedup = p.DedupKey
		query = enqueueJobDedupSQL
	}

	var id int64
	err := s.db.QueryRowContext(ctx, query, p.Kind, p.TenantID, rawPayload, dedup, maxAttempts, p.Delay.Microseconds()).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) && p.DedupKey != "" {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("enqueue job: %w", err)
	}
	return id, nil
}

// Claim 批量抢占可执行任务，返回已标记为 running 且递增 attempts 的任务列表。
func (s *Store) Claim(ctx context.Context, p job.ClaimParams) ([]job.Job, error) {
	if p.Limit <= 0 {
		return nil, nil
	}
	if p.Lease <= 0 {
		p.Lease = job.DefaultLeaseDuration
	}
	if p.LockedBy == "" {
		return nil, errors.New("claim jobs: locked_by is required")
	}

	rows, err := s.db.QueryContext(ctx, claimJobsSQL, p.Limit, p.MaxPerTenant, p.Lease.Microseconds(), p.LockedBy)
	if err != nil {
		return nil, fmt.Errorf("claim jobs: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]job.Job, 0, p.Limit)
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, fmt.Errorf("claim jobs: %w", err)
		}
		out = append(out, j)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("claim jobs: %w", err)
	}
	return out, nil
}

// Complete 将当前 worker 持有的任务标记为 done。租约已易主时静默不覆盖。
func (s *Store) Complete(ctx context.Context, id int64, lockedBy string) error {
	if _, err := s.db.ExecContext(ctx, completeJobSQL, id, lockedBy); err != nil {
		return fmt.Errorf("complete job %d: %w", id, err)
	}
	return nil
}

// Retry 将当前 worker 持有的任务退回 pending 并设置下次运行时间。
func (s *Store) Retry(ctx context.Context, p job.RetryParams) error {
	if p.Delay < 0 {
		p.Delay = 0
	}
	if _, err := s.db.ExecContext(ctx, retryJobSQL,
		p.ID, p.LockedBy, p.Delay.Microseconds(), p.LastError, p.ConsumeAttempt); err != nil {
		return fmt.Errorf("retry job %d: %w", p.ID, err)
	}
	return nil
}

// Dead 将当前 worker 持有的任务标记为 dead 终态。
func (s *Store) Dead(ctx context.Context, id int64, lockedBy, lastError string) error {
	if _, err := s.db.ExecContext(ctx, deadJobSQL, id, lockedBy, lastError); err != nil {
		return fmt.Errorf("mark job %d dead: %w", id, err)
	}
	return nil
}

// ReapExpired 回收所有租约已过期的 running 任务，返回每条被回收任务的 kind 和新状态。
func (s *Store) ReapExpired(ctx context.Context, retryDelay time.Duration) ([]job.ReapedRecord, error) {
	if retryDelay < 0 {
		retryDelay = 0
	}
	rows, err := s.db.QueryContext(ctx, reapExpiredJobsSQL, retryDelay.Microseconds())
	if err != nil {
		return nil, fmt.Errorf("reap expired jobs: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var records []job.ReapedRecord
	for rows.Next() {
		var rec job.ReapedRecord
		if err := rows.Scan(&rec.Kind, &rec.Status); err != nil {
			return nil, fmt.Errorf("reap expired jobs: %w", err)
		}
		records = append(records, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reap expired jobs: %w", err)
	}
	return records, nil
}

// OldestPendingAges 返回每种任务当前最老到期 pending 任务的等待时长，供积压告警指标使用。
func (s *Store) OldestPendingAges(ctx context.Context) (map[string]time.Duration, error) {
	rows, err := s.db.QueryContext(ctx, oldestPendingAgesSQL)
	if err != nil {
		return nil, fmt.Errorf("oldest pending ages: %w", err)
	}
	defer func() { _ = rows.Close() }()

	ages := make(map[string]time.Duration)
	for rows.Next() {
		var (
			kind    string
			seconds float64
		)
		if err := rows.Scan(&kind, &seconds); err != nil {
			return nil, fmt.Errorf("oldest pending ages: %w", err)
		}
		ages[kind] = time.Duration(seconds * float64(time.Second))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("oldest pending ages: %w", err)
	}
	return ages, nil
}

// CleanupDoneJobs 分批删除早于 retention 的已完成任务，控制表膨胀；dead 任务保留供排障。
func (s *Store) CleanupDoneJobs(ctx context.Context, retention time.Duration, limit int) (int64, error) {
	if retention < 0 {
		return 0, errors.New("cleanup done jobs: retention must not be negative")
	}
	if limit <= 0 {
		return 0, errors.New("cleanup done jobs: limit must be positive")
	}
	res, err := s.db.ExecContext(ctx, cleanupDoneJobsSQL, retention.Microseconds(), limit)
	if err != nil {
		return 0, fmt.Errorf("cleanup done jobs: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("cleanup done jobs: %w", err)
	}
	return n, nil
}

func scanJob(scanner rowScanner) (job.Job, error) {
	var (
		j           job.Job
		tenantID    sql.NullInt64
		dedupKey    sql.NullString
		lockedAt    sql.NullTime
		lockedUntil sql.NullTime
		lockedBy    sql.NullString
		finishedAt  sql.NullTime
	)
	if err := scanner.Scan(
		&j.ID, &j.Kind, &tenantID, &j.Payload, &dedupKey, &j.Status,
		&j.Attempts, &j.MaxAttempts, &j.RunAt, &lockedAt, &lockedUntil,
		&lockedBy, &j.LastError, &j.CreatedAt, &j.UpdatedAt, &finishedAt,
	); err != nil {
		return job.Job{}, err
	}
	if tenantID.Valid {
		id := tenantID.Int64
		j.TenantID = &id
	}
	if dedupKey.Valid {
		j.DedupKey = dedupKey.String
	}
	if lockedBy.Valid {
		j.LockedBy = lockedBy.String
	}
	j.RunAt = j.RunAt.UTC()
	j.LockedAt = nullTimeUTC(lockedAt)
	j.LockedUntil = nullTimeUTC(lockedUntil)
	j.CreatedAt = j.CreatedAt.UTC()
	j.UpdatedAt = j.UpdatedAt.UTC()
	j.FinishedAt = nullTimeUTC(finishedAt)
	return j, nil
}
