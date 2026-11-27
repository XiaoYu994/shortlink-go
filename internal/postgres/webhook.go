package postgres

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"shortlink/internal/audit"
	"shortlink/internal/job"
	"shortlink/internal/webhook"
)

const (
	webhookColumns = `id, tenant_id, url, description, events, secret_prefix, enabled,
       consecutive_failures, disabled_reason, disabled_at, created_by, created_at, updated_at`

	listWebhooksSQL = `SELECT ` + webhookColumns + `
FROM webhooks
WHERE tenant_id = $1
ORDER BY id DESC`

	lockTenantForWebhookCreateSQL = `SELECT id FROM tenants WHERE id = $1 FOR UPDATE`

	countWebhooksByTenantSQL = `SELECT count(*) FROM webhooks WHERE tenant_id = $1`

	insertWebhookSQL = `INSERT INTO webhooks (tenant_id, url, description, events, secret_ciphertext, secret_prefix, created_by)
VALUES ($1, $2, $3, $4::text[], $5, $6, $7)
RETURNING ` + webhookColumns

	lockWebhookSQL = `SELECT ` + webhookColumns + `
FROM webhooks
WHERE id = $1 AND tenant_id = $2
FOR UPDATE`

	updateWebhookSQL = `UPDATE webhooks
SET url = $3,
    description = $4,
    events = $5::text[],
    enabled = $6,
    consecutive_failures = $7,
    disabled_reason = $8,
    disabled_at = $9,
    updated_at = now()
WHERE id = $1 AND tenant_id = $2
RETURNING ` + webhookColumns

	deleteWebhookSQL = `DELETE FROM webhooks WHERE id = $1 AND tenant_id = $2`

	rotateWebhookSecretSQL = `UPDATE webhooks
SET secret_ciphertext = $3,
    secret_prefix = $4,
    updated_at = now()
WHERE id = $1 AND tenant_id = $2
RETURNING ` + webhookColumns

	webhookExistsForTenantSQL = `SELECT EXISTS (SELECT 1 FROM webhooks WHERE id = $1 AND tenant_id = $2)`

	insertWebhookTestJobSQL = `INSERT INTO jobs (kind, tenant_id, payload, max_attempts, traceparent)
VALUES ($1, $2, $3::jsonb, 1, $4)
RETURNING id`

	enqueueWebhookEventSQL = `INSERT INTO jobs (kind, tenant_id, payload, max_attempts, traceparent)
SELECT $1,
       w.tenant_id,
       jsonb_build_object(
           'webhook_id', w.id,
           'event_id', $2::text,
           'event', $3::text,
           'occurred_at', $4::timestamptz,
           'data', $5::jsonb
       ),
       $6,
       $7
FROM webhooks w
WHERE w.tenant_id = $8
  AND w.enabled = TRUE
  AND $3::text = ANY(w.events)`

	deliveryColumns = `id, tenant_id, webhook_id, job_id, delivery_id, event, attempt, status,
       http_status, duration_ms, error_message, response_snippet, created_at`

	listWebhookDeliveriesFirstSQL = `SELECT ` + deliveryColumns + `
FROM webhook_deliveries
WHERE tenant_id = $1 AND webhook_id = $2
ORDER BY id DESC
LIMIT $3`

	listWebhookDeliveriesAfterSQL = `SELECT ` + deliveryColumns + `
FROM webhook_deliveries
WHERE tenant_id = $1 AND webhook_id = $2 AND id < $3
ORDER BY id DESC
LIMIT $4`

	getWebhookForDeliverySQL = `SELECT id, tenant_id, url, enabled, secret_ciphertext
FROM webhooks
WHERE id = $1`

	insertWebhookDeliverySQL = `INSERT INTO webhook_deliveries (
    tenant_id, webhook_id, job_id, delivery_id, event, attempt, status,
    http_status, duration_ms, error_message, response_snippet
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`

	// 条件清零仅在 consecutive_failures > 0 时更新，无二级索引，触发 PostgreSQL HOT Update。
	resetWebhookFailuresSQL = `UPDATE webhooks
SET consecutive_failures = 0,
    updated_at = now()
WHERE id = $1 AND tenant_id = $2 AND consecutive_failures > 0`

	lockWebhookForDeliveryRecordSQL = `SELECT enabled, consecutive_failures
FROM webhooks
WHERE id = $1 AND tenant_id = $2
FOR UPDATE`

	incrementWebhookFailureSQL = `UPDATE webhooks
SET consecutive_failures = $3,
    enabled = $4,
    disabled_reason = $5,
    disabled_at = $6,
    updated_at = now()
WHERE id = $1 AND tenant_id = $2`

	cleanupWebhookDeliveriesSQL = `DELETE FROM webhook_deliveries
WHERE id IN (
    SELECT id
    FROM webhook_deliveries
    WHERE created_at < now() - ($1::bigint * interval '1 microsecond')
    ORDER BY created_at, id
    LIMIT $2
)`
)

// ListWebhooks 按 id 倒序返回指定组织的全部 Webhook 订阅（不含加密密文）。
func (s *Store) ListWebhooks(ctx context.Context, tenantID int64) ([]webhook.Subscription, error) {
	if tenantID <= 0 {
		return nil, webhook.ErrTenantNotFound
	}
	var out []webhook.Subscription
	err := s.withTenantReadTx(ctx, tenantID, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, listWebhooksSQL, tenantID)
		if err != nil {
			return fmt.Errorf("list webhooks: %w", err)
		}
		defer func() { _ = rows.Close() }()

		for rows.Next() {
			sub, err := scanWebhookSubscription(rows)
			if err != nil {
				return fmt.Errorf("list webhooks: %w", err)
			}
			out = append(out, sub)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("list webhooks: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

/*
CreateWebhook 在同一事务内锁住租户行校验单租户订阅上限（最多 MaxPerTenant 条），
写入加密后的订阅密钥与 webhook.create 审计日志。
*/
func (s *Store) CreateWebhook(ctx context.Context, p webhook.CreateParams) (webhook.Subscription, error) {
	if p.TenantID <= 0 {
		return webhook.Subscription{}, webhook.ErrTenantNotFound
	}
	if len(p.Events) == 0 {
		return webhook.Subscription{}, webhook.ErrInvalidEvents
	}
	if len(p.SecretCiphertext) == 0 || p.SecretPrefix == "" {
		return webhook.Subscription{}, errors.New("create webhook: secret ciphertext and prefix are required")
	}
	if err := ctx.Err(); err != nil {
		return webhook.Subscription{}, err
	}
	var explicitUserID int64
	if p.CreatedBy != nil {
		if *p.CreatedBy <= 0 {
			return webhook.Subscription{}, errors.New("create webhook: created_by must be positive")
		}
		explicitUserID = *p.CreatedBy
	}
	if _, _, _, err := requireAuditMetaOrUser(ctx, explicitUserID); err != nil {
		return webhook.Subscription{}, fmt.Errorf("create webhook: %w", err)
	}

	var sub webhook.Subscription
	err := s.withTenantTx(ctx, p.TenantID, nil, func(tx *sql.Tx) error {
		var lockedTenantID int64
		err := tx.QueryRowContext(ctx, lockTenantForWebhookCreateSQL, p.TenantID).Scan(&lockedTenantID)
		if errors.Is(err, sql.ErrNoRows) {
			return webhook.ErrTenantNotFound
		}
		if err != nil {
			return fmt.Errorf("create webhook: lock tenant: %w", err)
		}

		var count int
		if err := tx.QueryRowContext(ctx, countWebhooksByTenantSQL, p.TenantID).Scan(&count); err != nil {
			return fmt.Errorf("create webhook: count webhooks: %w", err)
		}
		if count >= webhook.MaxPerTenant {
			return webhook.ErrLimitExceeded
		}

		var createdBy sql.NullInt64
		if p.CreatedBy != nil {
			createdBy = sql.NullInt64{Int64: *p.CreatedBy, Valid: true}
		}
		eventStrings := eventsToStrings(p.Events)
		created, err := scanWebhookSubscription(tx.QueryRowContext(ctx, insertWebhookSQL,
			p.TenantID, p.URL, p.Description, eventStrings, p.SecretCiphertext, p.SecretPrefix, createdBy,
		))
		if err != nil {
			return fmt.Errorf("create webhook: insert: %w", err)
		}
		sub = created

		if err := insertAuditLogTx(ctx, tx, sub.TenantID, explicitUserID, audit.ActionWebhookCreate, audit.TargetWebhook, strconv.FormatInt(sub.ID, 10), map[string]any{
			"url":           sub.URL,
			"events":        eventStrings,
			"secret_prefix": sub.SecretPrefix,
			"description":   sub.Description,
		}); err != nil {
			return fmt.Errorf("create webhook: %w", err)
		}
		return nil
	})
	if err != nil {
		return webhook.Subscription{}, err
	}
	return sub, nil
}

// UpdateWebhook 局部更新订阅配置；重新启用时同步清零连续失败计数与停用原因。
func (s *Store) UpdateWebhook(ctx context.Context, p webhook.UpdateParams) (webhook.Subscription, error) {
	if p.TenantID <= 0 || p.WebhookID <= 0 {
		return webhook.Subscription{}, webhook.ErrNotFound
	}
	if p.URL == nil && p.Description == nil && p.Events == nil && p.Enabled == nil {
		return webhook.Subscription{}, errors.New("update webhook: no fields to update")
	}
	if err := ctx.Err(); err != nil {
		return webhook.Subscription{}, err
	}
	if _, _, _, err := requireAuditMeta(ctx); err != nil {
		return webhook.Subscription{}, fmt.Errorf("update webhook: %w", err)
	}

	var updated webhook.Subscription
	err := s.withTenantTx(ctx, p.TenantID, nil, func(tx *sql.Tx) error {
		cur, err := scanWebhookSubscription(tx.QueryRowContext(ctx, lockWebhookSQL, p.WebhookID, p.TenantID))
		if errors.Is(err, sql.ErrNoRows) {
			return webhook.ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("update webhook: lock: %w", err)
		}

		nextURL := cur.URL
		nextDesc := cur.Description
		nextEvents := eventsToStrings(cur.Events)
		nextEnabled := cur.Enabled
		nextFailures := cur.ConsecutiveFailures
		nextReason := cur.DisabledReason
		var nextDisabledAt sql.NullTime
		if cur.DisabledAt != nil {
			nextDisabledAt = sql.NullTime{Time: cur.DisabledAt.UTC(), Valid: true}
		}

		changedFields := make([]string, 0, 4)
		auditMeta := map[string]any{}
		if p.URL != nil {
			nextURL = *p.URL
			changedFields = append(changedFields, "url")
			auditMeta["url"] = nextURL
		}
		if p.Description != nil {
			nextDesc = *p.Description
			changedFields = append(changedFields, "description")
			auditMeta["description"] = nextDesc
		}
		if p.Events != nil {
			nextEvents = eventsToStrings(p.Events)
			changedFields = append(changedFields, "events")
			auditMeta["events"] = nextEvents
		}
		if p.Enabled != nil {
			changedFields = append(changedFields, "enabled")
			auditMeta["enabled"] = *p.Enabled
			if *p.Enabled {
				nextEnabled = true
				nextFailures = 0
				nextReason = webhook.DisabledReasonNone
				nextDisabledAt = sql.NullTime{}
			} else if cur.Enabled {
				// 仅在由启用切为停用时写成 manual；原本已熔断停用时保留 consecutive_failures 与原时间。
				nextEnabled = false
				nextReason = webhook.DisabledReasonManual
				nextDisabledAt = sql.NullTime{Time: time.Now().UTC(), Valid: true}
			}
		}
		auditMeta["fields"] = changedFields

		res, err := scanWebhookSubscription(tx.QueryRowContext(ctx, updateWebhookSQL,
			p.WebhookID, p.TenantID, nextURL, nextDesc, nextEvents,
			nextEnabled, nextFailures, string(nextReason), nextDisabledAt,
		))
		if err != nil {
			return fmt.Errorf("update webhook: %w", err)
		}
		updated = res
		if err := insertAuditLogTx(ctx, tx, p.TenantID, 0, audit.ActionWebhookUpdate, audit.TargetWebhook, strconv.FormatInt(p.WebhookID, 10), auditMeta); err != nil {
			return fmt.Errorf("update webhook: %w", err)
		}
		return nil
	})
	if err != nil {
		return webhook.Subscription{}, err
	}
	return updated, nil
}

// DeleteWebhook 按租户删除 Webhook 订阅并写审计日志。
func (s *Store) DeleteWebhook(ctx context.Context, tenantID, webhookID int64) error {
	if tenantID <= 0 || webhookID <= 0 {
		return webhook.ErrNotFound
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, _, _, err := requireAuditMeta(ctx); err != nil {
		return fmt.Errorf("delete webhook: %w", err)
	}

	return s.withTenantTx(ctx, tenantID, nil, func(tx *sql.Tx) error {
		cur, err := scanWebhookSubscription(tx.QueryRowContext(ctx, lockWebhookSQL, webhookID, tenantID))
		if errors.Is(err, sql.ErrNoRows) {
			return webhook.ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("delete webhook: lock: %w", err)
		}
		if _, err := tx.ExecContext(ctx, deleteWebhookSQL, webhookID, tenantID); err != nil {
			return fmt.Errorf("delete webhook: %w", err)
		}
		if err := insertAuditLogTx(ctx, tx, tenantID, 0, audit.ActionWebhookDelete, audit.TargetWebhook, strconv.FormatInt(webhookID, 10), map[string]any{
			"url": cur.URL,
		}); err != nil {
			return fmt.Errorf("delete webhook: %w", err)
		}
		return nil
	})
}

// RotateWebhookSecret 替换加密签名密钥与前缀，并写入审计日志。
func (s *Store) RotateWebhookSecret(ctx context.Context, tenantID, webhookID int64, ciphertext []byte, prefix string) (webhook.Subscription, error) {
	if tenantID <= 0 || webhookID <= 0 {
		return webhook.Subscription{}, webhook.ErrNotFound
	}
	if err := ctx.Err(); err != nil {
		return webhook.Subscription{}, err
	}
	if _, _, _, err := requireAuditMeta(ctx); err != nil {
		return webhook.Subscription{}, fmt.Errorf("rotate webhook secret: %w", err)
	}

	var updated webhook.Subscription
	err := s.withTenantTx(ctx, tenantID, nil, func(tx *sql.Tx) error {
		res, err := scanWebhookSubscription(tx.QueryRowContext(ctx, rotateWebhookSecretSQL, webhookID, tenantID, ciphertext, prefix))
		if errors.Is(err, sql.ErrNoRows) {
			return webhook.ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("rotate webhook secret: %w", err)
		}
		updated = res
		if err := insertAuditLogTx(ctx, tx, tenantID, 0, audit.ActionWebhookRotateSecret, audit.TargetWebhook, strconv.FormatInt(webhookID, 10), map[string]any{
			"secret_prefix": updated.SecretPrefix,
		}); err != nil {
			return fmt.Errorf("rotate webhook secret: %w", err)
		}
		return nil
	})
	if err != nil {
		return webhook.Subscription{}, err
	}
	return updated, nil
}

// EnqueueWebhookTest 为指定订阅同事务入队一条 max_attempts=1 的 webhook.ping 测试任务并记审计。
func (s *Store) EnqueueWebhookTest(ctx context.Context, tenantID, webhookID int64) (webhook.TestEnqueueResult, error) {
	if tenantID <= 0 || webhookID <= 0 {
		return webhook.TestEnqueueResult{}, webhook.ErrNotFound
	}
	if err := ctx.Err(); err != nil {
		return webhook.TestEnqueueResult{}, err
	}
	if _, _, _, err := requireAuditMeta(ctx); err != nil {
		return webhook.TestEnqueueResult{}, fmt.Errorf("enqueue webhook test: %w", err)
	}

	var out webhook.TestEnqueueResult
	err := s.withTenantTx(ctx, tenantID, nil, func(tx *sql.Tx) error {
		var exists bool
		if err := tx.QueryRowContext(ctx, webhookExistsForTenantSQL, webhookID, tenantID).Scan(&exists); err != nil {
			return fmt.Errorf("enqueue webhook test: check webhook: %w", err)
		}
		if !exists {
			return webhook.ErrNotFound
		}

		eventID, err := newWebhookEventID()
		if err != nil {
			return fmt.Errorf("enqueue webhook test: %w", err)
		}
		dataJSON, err := json.Marshal(struct {
			WebhookID int64 `json:"webhook_id"`
			Ping      bool  `json:"ping"`
		}{
			WebhookID: webhookID,
			Ping:      true,
		})
		if err != nil {
			return fmt.Errorf("enqueue webhook test: marshal data: %w", err)
		}
		payloadJSON, err := json.Marshal(webhook.DeliverPayload{
			WebhookID:  webhookID,
			EventID:    eventID,
			Event:      webhook.EventPing,
			OccurredAt: time.Now().UTC(),
			Data:       dataJSON,
		})
		if err != nil {
			return fmt.Errorf("enqueue webhook test: marshal payload: %w", err)
		}

		var jobID int64
		if err := tx.QueryRowContext(ctx, insertWebhookTestJobSQL,
			job.KindWebhookDeliver, tenantID, string(payloadJSON), traceparentFromContext(ctx),
		).Scan(&jobID); err != nil {
			return fmt.Errorf("enqueue webhook test: insert job: %w", err)
		}
		deliveryID := webhook.FormatDeliveryID(jobID)
		if err := insertAuditLogTx(ctx, tx, tenantID, 0, audit.ActionWebhookTest, audit.TargetWebhook, strconv.FormatInt(webhookID, 10), map[string]any{
			"job_id":      jobID,
			"delivery_id": deliveryID,
		}); err != nil {
			return fmt.Errorf("enqueue webhook test: %w", err)
		}
		out = webhook.TestEnqueueResult{
			JobID:      jobID,
			DeliveryID: deliveryID,
		}
		return nil
	})
	if err != nil {
		return webhook.TestEnqueueResult{}, err
	}
	return out, nil
}

func newWebhookEventID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate webhook event id: %w", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("evt_%08x-%04x-%04x-%04x-%012x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

/*
enqueueWebhookEventTx 在当前事务内为租户所有启用且订阅该事件的 Webhook 扇出入队投递任务。
单次事件的所有订阅共享同一个 event_id，业务数据由 Go 结构体序列化后传入，避免在 SQL 中多层手拼 JSON。
*/
func enqueueWebhookEventTx(
	ctx context.Context,
	tx *sql.Tx,
	tenantID int64,
	event webhook.EventType,
	occurredAt time.Time,
	data any,
) error {
	eventID, err := newWebhookEventID()
	if err != nil {
		return err
	}
	dataJSON, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("enqueue webhook %s: marshal data: %w", event, err)
	}
	if _, err := tx.ExecContext(ctx, enqueueWebhookEventSQL,
		job.KindWebhookDeliver, eventID, string(event), occurredAt.UTC(), string(dataJSON),
		webhook.DefaultMaxAttempts, traceparentFromContext(ctx), tenantID,
	); err != nil {
		return fmt.Errorf("enqueue webhook %s: %w", event, err)
	}
	return nil
}

// ListWebhookDeliveries 按 id 倒序游标分页返回单条订阅的投递尝试记录。
func (s *Store) ListWebhookDeliveries(ctx context.Context, opts webhook.DeliveryListOptions) ([]webhook.Delivery, error) {
	if opts.TenantID <= 0 || opts.WebhookID <= 0 {
		return nil, webhook.ErrNotFound
	}
	if opts.Limit <= 0 {
		return nil, errors.New("list webhook deliveries: limit must be positive")
	}
	if opts.Cursor != nil && opts.Cursor.ID <= 0 {
		return nil, errors.New("list webhook deliveries: cursor id must be positive")
	}

	out := make([]webhook.Delivery, 0, opts.Limit)
	err := s.withTenantReadTx(ctx, opts.TenantID, func(tx *sql.Tx) error {
		var exists bool
		if err := tx.QueryRowContext(ctx, webhookExistsForTenantSQL, opts.WebhookID, opts.TenantID).Scan(&exists); err != nil {
			return fmt.Errorf("list webhook deliveries: check webhook: %w", err)
		}
		if !exists {
			return webhook.ErrNotFound
		}

		var (
			rows *sql.Rows
			err  error
		)
		if opts.Cursor == nil {
			rows, err = tx.QueryContext(ctx, listWebhookDeliveriesFirstSQL, opts.TenantID, opts.WebhookID, opts.Limit)
		} else {
			rows, err = tx.QueryContext(ctx, listWebhookDeliveriesAfterSQL, opts.TenantID, opts.WebhookID, opts.Cursor.ID, opts.Limit)
		}
		if err != nil {
			return fmt.Errorf("list webhook deliveries: %w", err)
		}
		defer func() { _ = rows.Close() }()

		for rows.Next() {
			d, err := scanWebhookDelivery(rows)
			if err != nil {
				return fmt.Errorf("list webhook deliveries: %w", err)
			}
			out = append(out, d)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("list webhook deliveries: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// GetWebhookForDelivery 在 worker 投递前加载订阅当前 URL、启用状态与密文密钥。
func (s *Store) GetWebhookForDelivery(ctx context.Context, webhookID int64) (webhook.DeliveryTarget, error) {
	if webhookID <= 0 {
		return webhook.DeliveryTarget{}, webhook.ErrNotFound
	}
	var target webhook.DeliveryTarget
	err := s.db.QueryRowContext(ctx, getWebhookForDeliverySQL, webhookID).Scan(
		&target.ID, &target.TenantID, &target.URL, &target.Enabled, &target.SecretCiphertext,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return webhook.DeliveryTarget{}, webhook.ErrNotFound
	}
	if err != nil {
		return webhook.DeliveryTarget{}, fmt.Errorf("get webhook for delivery: %w", err)
	}
	return target, nil
}

/*
RecordWebhookDelivery 持久化单次投递尝试记录；
对非 webhook.ping 业务事件，成功时条件清零连续失败计数，终态失败时累计并在达到阈值时同事务自动停用并写系统审计日志。
*/
func (s *Store) RecordWebhookDelivery(ctx context.Context, p webhook.RecordDeliveryParams) (bool, error) {
	if p.TenantID <= 0 || p.WebhookID <= 0 || p.JobID <= 0 {
		return false, errors.New("record webhook delivery: invalid ids")
	}
	if p.TerminalFailure && !p.IsPing && p.MaxConsecutiveFailures > 0 {
		if _, _, _, err := requireAuditMeta(ctx); err != nil {
			return false, fmt.Errorf("record webhook delivery: %w", err)
		}
	}

	autoDisabled := false
	err := s.withTenantTx(ctx, p.TenantID, nil, func(tx *sql.Tx) error {
		var httpStatus sql.NullInt64
		if p.HTTPStatus != nil {
			httpStatus = sql.NullInt64{Int64: int64(*p.HTTPStatus), Valid: true}
		}
		if _, err := tx.ExecContext(ctx, insertWebhookDeliverySQL,
			p.TenantID, p.WebhookID, p.JobID, p.DeliveryID, string(p.Event),
			p.Attempt, string(p.Status), httpStatus, p.DurationMs, p.ErrorMessage, p.ResponseSnippet,
		); err != nil {
			return fmt.Errorf("record webhook delivery: insert log: %w", err)
		}

		switch {
		case p.IsPing:
			// webhook.ping 不修改熔断计数器，也不触发自动停用。
		case p.Status == webhook.StatusSuccess:
			if _, err := tx.ExecContext(ctx, resetWebhookFailuresSQL, p.WebhookID, p.TenantID); err != nil {
				return fmt.Errorf("record webhook delivery: reset failures: %w", err)
			}
		case p.TerminalFailure:
			var (
				wasEnabled   bool
				prevFailures int
			)
			err := tx.QueryRowContext(ctx, lockWebhookForDeliveryRecordSQL, p.WebhookID, p.TenantID).Scan(&wasEnabled, &prevFailures)
			if err == nil && wasEnabled {
				nextFailures := prevFailures + 1
				nextEnabled := true
				var nextReason webhook.DisabledReason
				var nextDisabledAt sql.NullTime
				if p.MaxConsecutiveFailures > 0 && nextFailures >= p.MaxConsecutiveFailures {
					autoDisabled = true
					nextEnabled = false
					nextReason = webhook.DisabledReasonConsecutiveFailures
					nextDisabledAt = sql.NullTime{Time: time.Now().UTC(), Valid: true}
				}
				if _, err := tx.ExecContext(ctx, incrementWebhookFailureSQL,
					p.WebhookID, p.TenantID, nextFailures, nextEnabled, string(nextReason), nextDisabledAt,
				); err != nil {
					return fmt.Errorf("record webhook delivery: update failure count: %w", err)
				}
				if autoDisabled {
					if err := insertAuditLogTx(ctx, tx, p.TenantID, 0, audit.ActionWebhookAutoDisable, audit.TargetWebhook, strconv.FormatInt(p.WebhookID, 10), map[string]any{
						"consecutive_failures": nextFailures,
						"disabled_reason":      string(webhook.DisabledReasonConsecutiveFailures),
						"delivery_id":          p.DeliveryID,
					}); err != nil {
						return fmt.Errorf("record webhook delivery: audit auto disable: %w", err)
					}
				}
			} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("record webhook delivery: lock webhook: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	return autoDisabled, nil
}

// CleanupWebhookDeliveries 分批删除早于 retention 的历史投递明细，控制表膨胀。
func (s *Store) CleanupWebhookDeliveries(ctx context.Context, retention time.Duration, limit int) (int64, error) {
	return s.execRetentionCleanup(ctx, "cleanup webhook deliveries", cleanupWebhookDeliveriesSQL, retention, limit)
}

func scanWebhookSubscription(scanner rowScanner) (webhook.Subscription, error) {
	var (
		sub        webhook.Subscription
		eventsRaw  []string
		reasonStr  string
		disabledAt sql.NullTime
		createdBy  sql.NullInt64
	)
	if err := scanner.Scan(
		&sub.ID, &sub.TenantID, &sub.URL, &sub.Description, &eventsRaw,
		&sub.SecretPrefix, &sub.Enabled, &sub.ConsecutiveFailures, &reasonStr,
		&disabledAt, &createdBy, &sub.CreatedAt, &sub.UpdatedAt,
	); err != nil {
		return webhook.Subscription{}, err
	}
	sub.Events = make([]webhook.EventType, len(eventsRaw))
	for i, ev := range eventsRaw {
		sub.Events[i] = webhook.EventType(ev)
	}
	sub.DisabledReason = webhook.DisabledReason(reasonStr)
	sub.DisabledAt = nullTimeUTC(disabledAt)
	if createdBy.Valid {
		v := createdBy.Int64
		sub.CreatedBy = &v
	}
	sub.CreatedAt = sub.CreatedAt.UTC()
	sub.UpdatedAt = sub.UpdatedAt.UTC()
	return sub, nil
}

func scanWebhookDelivery(scanner rowScanner) (webhook.Delivery, error) {
	var (
		d          webhook.Delivery
		eventStr   string
		statusStr  string
		httpStatus sql.NullInt64
	)
	if err := scanner.Scan(
		&d.ID, &d.TenantID, &d.WebhookID, &d.JobID, &d.DeliveryID,
		&eventStr, &d.Attempt, &statusStr, &httpStatus, &d.DurationMs,
		&d.ErrorMessage, &d.ResponseSnippet, &d.CreatedAt,
	); err != nil {
		return webhook.Delivery{}, err
	}
	d.Event = webhook.EventType(eventStr)
	d.Status = webhook.DeliveryStatus(statusStr)
	if httpStatus.Valid {
		code := int(httpStatus.Int64)
		d.HTTPStatus = &code
	}
	d.CreatedAt = d.CreatedAt.UTC()
	return d, nil
}

func eventsToStrings(events []webhook.EventType) []string {
	out := make([]string, len(events))
	for i, ev := range events {
		out[i] = string(ev)
	}
	return out
}
