package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"shortlink/internal/job"
)

const (
	jobColumns = `j.id, j.kind, j.tenant_id, j.payload, j.dedup_key, j.traceparent, j.status, j.attempts, j.max_attempts,
          j.run_at, j.locked_at, j.locked_until, j.locked_by, j.lock_version, j.last_error,
          j.created_at, j.updated_at, j.finished_at`

	enqueueJobSQL = `INSERT INTO jobs (kind, tenant_id, payload, dedup_key, max_attempts, run_at, traceparent)
VALUES ($1, $2, $3::jsonb, $4, $5, now() + ($6::bigint * interval '1 microsecond'), $7)
RETURNING id`

	enqueueJobDedupSQL = `INSERT INTO jobs (kind, tenant_id, payload, dedup_key, max_attempts, run_at, traceparent)
VALUES ($1, $2, $3::jsonb, $4, $5, now() + ($6::bigint * interval '1 microsecond'), $7)
ON CONFLICT (kind, dedup_key) WHERE dedup_key IS NOT NULL AND status IN ('pending', 'running')
DO NOTHING
RETURNING id`

	insertJobTxSQL = `INSERT INTO jobs (kind, tenant_id, payload, traceparent)
VALUES ($1, NULLIF($2::bigint, 0), $3::jsonb, $4)`

	/*
		关闭单租户限并发（MaxPerTenant <= 0）时按注册 kind 走极简快路径：
		经 unnest($1::text[]) + LATERAL 分别沿 idx_jobs_pending_kind_run (kind, run_at, id) 取最老到期行并加锁，
		再全局排序截断到 $2，复杂度与无关 kind 的历史积压解耦（ADR 0033）。
	*/
	claimJobsNoFairnessSQL = `WITH per_kind_candidates AS (
    SELECT picked.id, picked.run_at
    FROM unnest($1::text[]) AS k(kind)
    CROSS JOIN LATERAL (
        SELECT j.id, j.run_at
        FROM jobs j
        WHERE j.status = 'pending'
          AND j.kind = k.kind
          AND j.run_at <= now()
        ORDER BY j.run_at, j.id
        LIMIT $2
        FOR UPDATE OF j SKIP LOCKED
    ) AS picked
),
candidate AS (
    SELECT p.id
    FROM per_kind_candidates p
    ORDER BY p.run_at, p.id
    LIMIT $2
)
UPDATE jobs j
SET status = 'running',
    attempts = j.attempts + 1,
    lock_version = j.lock_version + 1,
    locked_at = now(),
    locked_until = now() + ($3::bigint * interval '1 microsecond'),
    locked_by = $4,
    updated_at = now()
FROM candidate c
WHERE j.id = c.id
RETURNING ` + jobColumns

	/*
		开启单租户公平软上限（MaxPerTenant > 0）时的按 kind 递归松散索引扫描 + LATERAL 抢占（ADR 0022、ADR 0033）：
		1. active_kind_tenants 对 unnest($1::text[]) 沿 idx_jobs_pending_kind_tenant_run (kind, tenant_id, run_at, id)
		   做按 kind 隔离的递归跳扫（严格带 tenant_id IS NOT NULL 命中部分索引），绝不扫无关/新版本未知 kind；
		2. per_tenant_candidates 对每个未满软上限的活跃租户按 kind 分别用 LATERAL 取至多 (max_per_tenant - running_cnt) 条，
		   合并裁剪后再通过 idx_jobs_pending_kind_platform_run 合并 tenant_id IS NULL 平台任务；
		3. ordered_candidates AS MATERIALIZED 先在内存中固定候选有序集，避免 PG 在高估行数时退化为全表 Hash Join，
		   最后经 candidate 用 LATERAL 主键点查 + FOR UPDATE OF j SKIP LOCKED 复核可见性并递增 lock_version。
	*/
	claimJobsFairSQL = `WITH RECURSIVE running_per_tenant AS (
    SELECT tenant_id, count(*) AS cnt
    FROM jobs
    WHERE status = 'running' AND tenant_id IS NOT NULL
    GROUP BY tenant_id
),
active_kind_tenants(kind, tenant_id) AS (
    SELECT k.kind, first_t.tenant_id
    FROM unnest($1::text[]) AS k(kind)
    CROSS JOIN LATERAL (
        SELECT j.tenant_id
        FROM jobs j
        WHERE j.status = 'pending'
          AND j.kind = k.kind
          AND j.tenant_id IS NOT NULL
        ORDER BY j.tenant_id
        LIMIT 1
    ) AS first_t
    UNION ALL
    SELECT a.kind, next_t.tenant_id
    FROM active_kind_tenants a
    CROSS JOIN LATERAL (
        SELECT j.tenant_id
        FROM jobs j
        WHERE j.status = 'pending'
          AND j.kind = a.kind
          AND j.tenant_id IS NOT NULL
          AND j.tenant_id > a.tenant_id
        ORDER BY j.tenant_id
        LIMIT 1
    ) AS next_t
),
active_tenants AS (
    SELECT DISTINCT tenant_id FROM active_kind_tenants
),
per_tenant_candidates AS (
    SELECT picked.id, picked.run_at
    FROM active_tenants t
    LEFT JOIN running_per_tenant r ON r.tenant_id = t.tenant_id
    CROSS JOIN LATERAL (
        SELECT merged.id, merged.run_at
        FROM (
            SELECT kp.id, kp.run_at
            FROM unnest($1::text[]) AS k(kind)
            CROSS JOIN LATERAL (
                SELECT j.id, j.run_at
                FROM jobs j
                WHERE j.status = 'pending'
                  AND j.kind = k.kind
                  AND j.tenant_id = t.tenant_id
                  AND j.run_at <= now()
                ORDER BY j.run_at, j.id
                LIMIT GREATEST($2::bigint - COALESCE(r.cnt, 0), 0)
            ) AS kp
        ) AS merged
        ORDER BY merged.run_at, merged.id
        LIMIT GREATEST($2::bigint - COALESCE(r.cnt, 0), 0)
    ) AS picked
    WHERE COALESCE(r.cnt, 0) < $2::bigint
    UNION ALL
    SELECT p.id, p.run_at
    FROM unnest($1::text[]) AS k(kind)
    CROSS JOIN LATERAL (
        SELECT j.id, j.run_at
        FROM jobs j
        WHERE j.status = 'pending'
          AND j.kind = k.kind
          AND j.tenant_id IS NULL
          AND j.run_at <= now()
        ORDER BY j.run_at, j.id
        LIMIT $3
    ) AS p
),
ordered_candidates AS MATERIALIZED (
    SELECT id, run_at
    FROM per_tenant_candidates
    ORDER BY run_at, id
),
candidate AS (
    SELECT locked.id
    FROM ordered_candidates o
    CROSS JOIN LATERAL (
        SELECT j.id
        FROM jobs j
        WHERE j.id = o.id
          AND j.status = 'pending'
          AND j.run_at <= now()
        FOR UPDATE OF j SKIP LOCKED
    ) AS locked
    LIMIT $3
)
UPDATE jobs j
SET status = 'running',
    attempts = j.attempts + 1,
    lock_version = j.lock_version + 1,
    locked_at = now(),
    locked_until = now() + ($4::bigint * interval '1 microsecond'),
    locked_by = $5,
    updated_at = now()
FROM candidate c
WHERE j.id = c.id
RETURNING ` + jobColumns

	completeJobSQL = `UPDATE jobs
SET status = 'done',
    locked_at = NULL,
    locked_until = NULL,
    locked_by = NULL,
    last_error = '',
    finished_at = now(),
    updated_at = now()
WHERE id = $1 AND status = 'running' AND locked_by = $2 AND lock_version = $3`

	retryJobSQL = `UPDATE jobs
SET status = 'pending',
    attempts = CASE WHEN $6 THEN attempts ELSE GREATEST(attempts - 1, 0) END,
    run_at = now() + ($4::bigint * interval '1 microsecond'),
    locked_at = NULL,
    locked_until = NULL,
    locked_by = NULL,
    last_error = $5,
    updated_at = now()
WHERE id = $1 AND status = 'running' AND locked_by = $2 AND lock_version = $3`

	deadJobSQL = `UPDATE jobs
SET status = 'dead',
    locked_at = NULL,
    locked_until = NULL,
    locked_by = NULL,
    last_error = $4,
    finished_at = now(),
    updated_at = now()
WHERE id = $1 AND status = 'running' AND locked_by = $2 AND lock_version = $3`

	// 分批回收租约到期任务（<= now() 与其他过期边界一致），避免崩溃恢复时单事务锁全量行。
	reapExpiredJobsSQL = `WITH expired AS (
    SELECT id
    FROM jobs
    WHERE status = 'running' AND locked_until <= now()
    ORDER BY locked_until
    LIMIT $2
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

	/*
		沿 idx_jobs_pending_kind_run (kind, run_at) 做松散索引扫描：
		只做 O(kind 数) 次索引查找，积压数十万条时不再全表扫回表 GROUP BY（ADR 0022）。
	*/
	oldestPendingAgesSQL = `WITH RECURSIVE kinds(kind) AS (
    (
        SELECT kind
        FROM jobs
        WHERE status = 'pending'
        ORDER BY kind
        LIMIT 1
    )
    UNION ALL
    SELECT next_k.kind
    FROM kinds k
    CROSS JOIN LATERAL (
        SELECT j.kind
        FROM jobs j
        WHERE j.status = 'pending'
          AND j.kind > k.kind
        ORDER BY j.kind
        LIMIT 1
    ) AS next_k
)
SELECT k.kind, GREATEST(EXTRACT(EPOCH FROM (now() - first_due.run_at)), 0)
FROM kinds k
CROSS JOIN LATERAL (
    SELECT j.run_at
    FROM jobs j
    WHERE j.status = 'pending'
      AND j.kind = k.kind
      AND j.run_at <= now()
    ORDER BY j.run_at
    LIMIT 1
) AS first_due`

	cleanupDoneJobsSQL = `DELETE FROM jobs
WHERE id IN (
    SELECT id
    FROM jobs
    WHERE status = 'done'
      AND finished_at < now() - ($1::bigint * interval '1 microsecond')
    ORDER BY finished_at, id
    LIMIT $2
)`

	cleanupDeadJobsSQL = `DELETE FROM jobs
WHERE id IN (
    SELECT id
    FROM jobs
    WHERE status = 'dead'
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
	err := s.db.QueryRowContext(ctx, query, p.Kind, p.TenantID, rawPayload, dedup, maxAttempts, p.Delay.Microseconds(), traceparentFromContext(ctx)).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) && p.DedupKey != "" {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("enqueue job: %w", err)
	}
	return id, nil
}

// Claim 按 Kinds 白名单批量抢占可执行任务，返回已标记为 running 且递增 attempts、lock_version 的任务列表。
// Kinds 过滤掉空串后若为空，直接返回 (nil, nil)，绝不抢占未声明的任务类型。
func (s *Store) Claim(ctx context.Context, p job.ClaimParams) ([]job.Job, error) {
	if p.Limit <= 0 {
		return nil, nil
	}
	kinds := normalizeClaimKinds(p.Kinds)
	if len(kinds) == 0 {
		return nil, nil
	}
	if p.Lease <= 0 {
		p.Lease = job.DefaultLeaseDuration
	}
	if p.LockedBy == "" {
		return nil, errors.New("claim jobs: locked_by is required")
	}

	var (
		rows *sql.Rows
		err  error
	)
	if p.MaxPerTenant <= 0 {
		rows, err = s.db.QueryContext(ctx, claimJobsNoFairnessSQL, kinds, p.Limit, p.Lease.Microseconds(), p.LockedBy)
	} else {
		rows, err = s.db.QueryContext(ctx, claimJobsFairSQL, kinds, p.MaxPerTenant, p.Limit, p.Lease.Microseconds(), p.LockedBy)
	}
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

func normalizeClaimKinds(raw []string) []string {
	out := make([]string, 0, len(raw))
	for _, k := range raw {
		if k != "" {
			out = append(out, k)
		}
	}
	if len(out) <= 1 {
		return out
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// Complete 将当前 worker 持有的任务标记为 done。租约已易主或代次过期时返回 job.ErrLeaseLost。
func (s *Store) Complete(ctx context.Context, id int64, lockedBy string, lockVersion int64) error {
	res, err := s.db.ExecContext(ctx, completeJobSQL, id, lockedBy, lockVersion)
	if err != nil {
		return fmt.Errorf("complete job %d: %w", id, err)
	}
	return checkJobLeaseAffected(res, "complete", id)
}

// Retry 将当前 worker 持有的任务退回 pending 并设置下次运行时间。租约失守时返回 job.ErrLeaseLost。
func (s *Store) Retry(ctx context.Context, p job.RetryParams) error {
	if p.Delay < 0 {
		p.Delay = 0
	}
	res, err := s.db.ExecContext(ctx, retryJobSQL,
		p.ID, p.LockedBy, p.LockVersion, p.Delay.Microseconds(), p.LastError, p.ConsumeAttempt)
	if err != nil {
		return fmt.Errorf("retry job %d: %w", p.ID, err)
	}
	return checkJobLeaseAffected(res, "retry", p.ID)
}

// Dead 将当前 worker 持有的任务标记为 dead 终态。租约失守时返回 job.ErrLeaseLost。
func (s *Store) Dead(ctx context.Context, id int64, lockedBy string, lockVersion int64, lastError string) error {
	res, err := s.db.ExecContext(ctx, deadJobSQL, id, lockedBy, lockVersion, lastError)
	if err != nil {
		return fmt.Errorf("mark job %d dead: %w", id, err)
	}
	return checkJobLeaseAffected(res, "mark dead", id)
}

func checkJobLeaseAffected(res sql.Result, op string, id int64) error {
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("%s job %d: rows affected: %w", op, id, err)
	}
	if n == 0 {
		return job.ErrLeaseLost
	}
	return nil
}

// ReapExpired 分批回收至多 limit 条租约已过期的 running 任务，返回每条被回收任务的 kind 和新状态。
func (s *Store) ReapExpired(ctx context.Context, retryDelay time.Duration, limit int) ([]job.ReapedRecord, error) {
	if retryDelay < 0 {
		retryDelay = 0
	}
	if limit <= 0 {
		limit = job.DefaultReapBatchSize
	}
	rows, err := s.db.QueryContext(ctx, reapExpiredJobsSQL, retryDelay.Microseconds(), limit)
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

// CleanupDoneJobs 分批删除早于 retention 的已完成任务，控制表膨胀。
func (s *Store) CleanupDoneJobs(ctx context.Context, retention time.Duration, limit int) (int64, error) {
	return s.execRetentionCleanup(ctx, "cleanup done jobs", cleanupDoneJobsSQL, retention, limit)
}

// CleanupDeadJobs 分批删除超过长保留期的死信任务，防止长期运行后 jobs 表无限膨胀。
func (s *Store) CleanupDeadJobs(ctx context.Context, retention time.Duration, limit int) (int64, error) {
	return s.execRetentionCleanup(ctx, "cleanup dead jobs", cleanupDeadJobsSQL, retention, limit)
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
		&j.ID, &j.Kind, &tenantID, &j.Payload, &dedupKey, &j.Traceparent, &j.Status,
		&j.Attempts, &j.MaxAttempts, &j.RunAt, &lockedAt, &lockedUntil,
		&lockedBy, &j.LockVersion, &j.LastError, &j.CreatedAt, &j.UpdatedAt, &finishedAt,
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

// insertJobTx 在当前事务内按 Go 结构体载荷入队一条异步任务，并写入可选 W3C traceparent，供控制面多步写事务复用。
func insertJobTx(ctx context.Context, tx *sql.Tx, kind string, tenantID int64, payload any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("enqueue job %s: marshal payload: %w", kind, err)
	}
	if _, err := tx.ExecContext(ctx, insertJobTxSQL, kind, tenantID, string(raw), traceparentFromContext(ctx)); err != nil {
		return fmt.Errorf("enqueue job %s: %w", kind, err)
	}
	return nil
}

func traceparentFromContext(ctx context.Context) string {
	if ctx == nil || !trace.SpanContextFromContext(ctx).IsValid() {
		return ""
	}
	carrier := propagation.MapCarrier{}
	propagation.TraceContext{}.Inject(ctx, carrier)
	return carrier.Get("traceparent")
}
