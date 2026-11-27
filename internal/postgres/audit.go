package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"shortlink/internal/audit"
)

const (
	insertAuditLogSQL = `INSERT INTO audit_logs (tenant_id, actor_kind, actor_id, action, target_type, target_id, request_id, metadata)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8::jsonb)`

	auditLogColumns = `a.id, a.tenant_id, a.actor_kind, a.actor_id,
       COALESCE(u.email, ''), COALESCE(u.name, ''),
       a.action, a.target_type, a.target_id, a.request_id, a.metadata, a.created_at`

	/*
		四条查询按是否有 action 与是否有 cursor 显式拆开，绝不写成 ($2 = '' OR a.action = $2)，
		确保分别精确命中 idx_audit_logs_tenant_id 与 idx_audit_logs_tenant_action_id。
	*/
	listAuditLogsFirstSQL = `SELECT ` + auditLogColumns + `
FROM audit_logs a
LEFT JOIN users u ON a.actor_kind = 'user' AND u.id = a.actor_id
WHERE a.tenant_id = $1
ORDER BY a.id DESC
LIMIT $2`

	listAuditLogsAfterSQL = `SELECT ` + auditLogColumns + `
FROM audit_logs a
LEFT JOIN users u ON a.actor_kind = 'user' AND u.id = a.actor_id
WHERE a.tenant_id = $1 AND a.id < $2
ORDER BY a.id DESC
LIMIT $3`

	listAuditLogsFirstByActionSQL = `SELECT ` + auditLogColumns + `
FROM audit_logs a
LEFT JOIN users u ON a.actor_kind = 'user' AND u.id = a.actor_id
WHERE a.tenant_id = $1 AND a.action = $2
ORDER BY a.id DESC
LIMIT $3`

	listAuditLogsAfterByActionSQL = `SELECT ` + auditLogColumns + `
FROM audit_logs a
LEFT JOIN users u ON a.actor_kind = 'user' AND u.id = a.actor_id
WHERE a.tenant_id = $1 AND a.action = $2 AND a.id < $3
ORDER BY a.id DESC
LIMIT $4`
)

func auditMetaTuple(m audit.Meta) (string, sql.NullInt64, string) {
	var actorID sql.NullInt64
	if m.ActorID != nil {
		actorID = sql.NullInt64{Int64: *m.ActorID, Valid: true}
	}
	return string(m.ActorKind), actorID, audit.SanitizeRequestID(m.RequestID)
}

// requireAuditMeta 强制校验 ctx 携带合法审计元数据（fail-closed，见 ADR 0025）。
func requireAuditMeta(ctx context.Context) (string, sql.NullInt64, string, error) {
	m, ok := audit.MetaFrom(ctx)
	if !ok {
		return "", sql.NullInt64{}, "", audit.ErrMissingMeta
	}
	if err := audit.ValidateMeta(m); err != nil {
		return "", sql.NullInt64{}, "", err
	}
	kind, actorID, reqID := auditMetaTuple(m)
	return kind, actorID, reqID, nil
}

/*
requireAuditMetaOrUser 优先校验 ctx 中的审计元数据；
当方法签名已显式传入操作者 userID（如成员变更、自助建组织、带发起人的邀请、邀请兑现）
且 ctx 未注入审计元数据时，回退到显式 userID，绝不伪造 system 或 operator。
*/
func requireAuditMetaOrUser(ctx context.Context, userID int64) (string, sql.NullInt64, string, error) {
	if m, ok := audit.MetaFrom(ctx); ok {
		if err := audit.ValidateMeta(m); err != nil {
			return "", sql.NullInt64{}, "", err
		}
		kind, actorID, reqID := auditMetaTuple(m)
		return kind, actorID, reqID, nil
	}
	if userID > 0 {
		kind, actorID, reqID := auditMetaTuple(audit.UserMeta(userID, ""))
		return kind, actorID, reqID, nil
	}
	return "", sql.NullInt64{}, "", audit.ErrMissingMeta
}

func marshalAuditMetadata(fields map[string]any) ([]byte, error) {
	if len(fields) == 0 {
		return []byte("{}"), nil
	}
	return json.Marshal(fields)
}

func insertAuditLogTx(
	ctx context.Context,
	tx *sql.Tx,
	tenantID int64,
	explicitUserID int64,
	action audit.Action,
	targetType audit.TargetType,
	targetID string,
	metadata map[string]any,
) error {
	if tenantID <= 0 {
		return fmt.Errorf("insert audit log: invalid tenant_id %d", tenantID)
	}
	if !action.Valid() {
		return fmt.Errorf("insert audit log: invalid action %q", action)
	}
	if !targetType.Valid() {
		return fmt.Errorf("insert audit log: invalid target_type %q", targetType)
	}
	if targetID == "" {
		return fmt.Errorf("insert audit log: empty target_id")
	}
	actorKind, actorID, reqID, err := requireAuditMetaOrUser(ctx, explicitUserID)
	if err != nil {
		return fmt.Errorf("insert audit log: %w", err)
	}
	metaJSON, err := marshalAuditMetadata(metadata)
	if err != nil {
		return fmt.Errorf("insert audit log: marshal metadata: %w", err)
	}
	if _, err := tx.ExecContext(ctx, insertAuditLogSQL,
		tenantID, actorKind, actorID, string(action), string(targetType), targetID, reqID, string(metaJSON),
	); err != nil {
		return fmt.Errorf("insert audit log: %w", err)
	}
	return nil
}

// ListAuditLogs 按 id 倒序返回指定租户的审计日志，支持按 action 筛选与 Keyset 游标分页。
func (s *Store) ListAuditLogs(ctx context.Context, opts audit.ListOptions) ([]audit.Event, error) {
	if opts.TenantID <= 0 {
		return nil, fmt.Errorf("list audit logs: tenant id must be positive")
	}
	if opts.Limit <= 0 {
		return nil, fmt.Errorf("list audit logs: limit must be positive")
	}
	if opts.Action != "" && !opts.Action.Valid() {
		return nil, fmt.Errorf("list audit logs: invalid action %q", opts.Action)
	}
	if opts.Cursor != nil && opts.Cursor.ID <= 0 {
		return nil, fmt.Errorf("list audit logs: cursor id must be positive")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	var (
		query string
		args  []any
	)
	switch {
	case opts.Action == "" && opts.Cursor == nil:
		query = listAuditLogsFirstSQL
		args = []any{opts.TenantID, opts.Limit}
	case opts.Action == "" && opts.Cursor != nil:
		query = listAuditLogsAfterSQL
		args = []any{opts.TenantID, opts.Cursor.ID, opts.Limit}
	case opts.Action != "" && opts.Cursor == nil:
		query = listAuditLogsFirstByActionSQL
		args = []any{opts.TenantID, string(opts.Action), opts.Limit}
	default:
		query = listAuditLogsAfterByActionSQL
		args = []any{opts.TenantID, string(opts.Action), opts.Cursor.ID, opts.Limit}
	}
	var events []audit.Event
	if err := s.withTenantReadTx(ctx, opts.TenantID, func(tx *sql.Tx) error {
		got, err := queryAuditLogs(ctx, tx, opts.Limit, query, args...)
		if err != nil {
			return err
		}
		events = got
		return nil
	}); err != nil {
		return nil, err
	}
	return events, nil
}

func queryAuditLogs(ctx context.Context, q queryContexter, capHint int, query string, args ...any) ([]audit.Event, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list audit logs: %w", err)
	}
	defer func() { _ = rows.Close() }()

	events := make([]audit.Event, 0, capHint)
	for rows.Next() {
		ev, err := scanAuditEvent(rows)
		if err != nil {
			return nil, fmt.Errorf("list audit logs: %w", err)
		}
		events = append(events, ev)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list audit logs: %w", err)
	}
	return events, nil
}

func scanAuditEvent(scanner rowScanner) (audit.Event, error) {
	var (
		ev          audit.Event
		actorKind   string
		actorID     sql.NullInt64
		action      string
		targetType  string
		rawMetadata []byte
	)
	if err := scanner.Scan(
		&ev.ID, &ev.TenantID, &actorKind, &actorID,
		&ev.ActorEmail, &ev.ActorName,
		&action, &targetType, &ev.TargetID, &ev.RequestID, &rawMetadata, &ev.CreatedAt,
	); err != nil {
		return audit.Event{}, err
	}
	ev.ActorKind = audit.ActorKind(actorKind)
	if actorID.Valid {
		v := actorID.Int64
		ev.ActorID = &v
	}
	ev.Action = audit.Action(action)
	ev.TargetType = audit.TargetType(targetType)
	if len(rawMetadata) == 0 {
		ev.Metadata = json.RawMessage("{}")
	} else {
		ev.Metadata = json.RawMessage(rawMetadata)
	}
	ev.CreatedAt = ev.CreatedAt.UTC()
	return ev, nil
}
