/*
Package webhook 实现租户生命周期 Webhook 订阅校验、密钥静态加密、HMAC-SHA256 签名与后台可靠投递（阶段 15、ADR 0027）。

所有业务状态事件（link.created / link.updated / link.deleted / link.banned / link.unbanned）
经 PostgreSQL Outbox 在业务写事务内原子扇出到 jobs，并由 console worker 消费。
*/
package webhook

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"
	"unicode"
)

const (
	MaxPerTenant        = 5
	MaxDescriptionBytes = 200
	MaxURLLength        = 2048

	DefaultMaxAttempts            = 6
	DefaultTimeout                = 5 * time.Second
	DefaultHostRateLimit          = 5
	DefaultRetryBaseDelay         = 10 * time.Second
	DefaultRetryMaxDelay          = 10 * time.Minute
	DefaultMaxConsecutiveFailures = 10
	DefaultDeliveryRetention      = 7 * 24 * time.Hour
	DefaultReplayTolerance        = 5 * time.Minute

	MaxResponseReadBytes = 4096
	MaxErrorBytes        = 512
	MaxSnippetBytes      = 1024
)

var (
	// ErrNotFound 表示 Webhook 订阅不存在或不属于当前组织。
	ErrNotFound = errors.New("webhook not found")
	// ErrLimitExceeded 表示该组织已达到最大 Webhook 订阅数量上限。
	ErrLimitExceeded = errors.New("webhook limit exceeded")
	// ErrInvalidURL 表示 Webhook 目标地址不合法、使用了受禁端口或指向受禁地址。
	ErrInvalidURL = errors.New("invalid webhook url")
	// ErrInvalidDescription 表示 Webhook 备注过长或包含非法控制字符。
	ErrInvalidDescription = errors.New("invalid webhook description")
	// ErrInvalidEvents 表示订阅事件列表为空或包含不允许的事件名。
	ErrInvalidEvents = errors.New("invalid webhook events")
	// ErrInvalidSignature 表示 Webhook 请求签名格式非法或摘要不匹配。
	ErrInvalidSignature = errors.New("invalid webhook signature")
	// ErrSignatureExpired 表示 Webhook 签名时间戳超出重放容忍窗口。
	ErrSignatureExpired = errors.New("webhook signature timestamp outside tolerance")
	// ErrDecryptSecret 表示无法使用当前主密钥解密订阅签名密钥。
	ErrDecryptSecret = errors.New("decrypt webhook secret failed")
	// ErrTenantNotFound 表示目标组织不存在。
	ErrTenantNotFound = errors.New("organization not found")
)

// EventType 表示 Webhook 投递事件类型。
type EventType string

const (
	EventLinkCreated  EventType = "link.created"
	EventLinkUpdated  EventType = "link.updated"
	EventLinkDeleted  EventType = "link.deleted"
	EventLinkBanned   EventType = "link.banned"
	EventLinkUnbanned EventType = "link.unbanned"
	EventPing         EventType = "webhook.ping"
)

// AllSubscribableEvents 返回全部可订阅的业务生命周期事件列表（固定规范顺序，不含 webhook.ping）。
func AllSubscribableEvents() []EventType {
	return []EventType{
		EventLinkCreated,
		EventLinkUpdated,
		EventLinkDeleted,
		EventLinkBanned,
		EventLinkUnbanned,
	}
}

// Subscribable 报告 e 是否允许写入订阅配置的 events 列表。
func (e EventType) Subscribable() bool {
	return slices.Contains(AllSubscribableEvents(), e)
}

// Valid 报告 e 是否属于所有已知 Webhook 事件（含 webhook.ping）。
func (e EventType) Valid() bool {
	return e == EventPing || e.Subscribable()
}

// NormalizeSubscribableEvents 校验并去重订阅事件列表，按固定顺序输出。
func NormalizeSubscribableEvents(events []EventType) ([]EventType, error) {
	if len(events) == 0 {
		return nil, errors.Join(ErrInvalidEvents, errors.New("at least one event is required"))
	}
	seen := make(map[EventType]bool, len(events))
	for _, ev := range events {
		if !ev.Subscribable() {
			return nil, ErrInvalidEvents
		}
		seen[ev] = true
	}
	out := make([]EventType, 0, len(seen))
	for _, ev := range AllSubscribableEvents() {
		if seen[ev] {
			out = append(out, ev)
		}
	}
	return out, nil
}

// NormalizeDescription 规范化并校验可选 Webhook 备注。
func NormalizeDescription(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if len(trimmed) > MaxDescriptionBytes {
		return "", ErrInvalidDescription
	}
	if strings.ContainsFunc(trimmed, unicode.IsControl) {
		return "", ErrInvalidDescription
	}
	return trimmed, nil
}

// DeliveryStatus 表示单次投递尝试的结果状态。
type DeliveryStatus string

const (
	StatusSuccess DeliveryStatus = "success"
	StatusRetry   DeliveryStatus = "retry"
	StatusFailed  DeliveryStatus = "failed"
)

// DisabledReason 区分订阅被停用的原因。
type DisabledReason string

const (
	DisabledReasonNone                DisabledReason = ""
	DisabledReasonManual              DisabledReason = "manual"
	DisabledReasonConsecutiveFailures DisabledReason = "consecutive_failures"
)

// Subscription 是租户 Webhook 订阅的公开视图（不含密文，仅含秘密前缀）。
type Subscription struct {
	ID                  int64
	TenantID            int64
	URL                 string
	Description         string
	Events              []EventType
	SecretPrefix        string
	Enabled             bool
	ConsecutiveFailures int
	DisabledReason      DisabledReason
	DisabledAt          *time.Time
	CreatedBy           *int64
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

// Delivery 是单次 Webhook 投递尝试的历史记录。
type Delivery struct {
	ID              int64
	TenantID        int64
	WebhookID       int64
	JobID           int64
	DeliveryID      string
	Event           EventType
	Attempt         int
	Status          DeliveryStatus
	HTTPStatus      *int
	DurationMs      int
	ErrorMessage    string
	ResponseSnippet string
	CreatedAt       time.Time
}

// DeliveryTarget 是 worker 投递前从数据库加载的订阅当前状态与加密密钥。
type DeliveryTarget struct {
	ID               int64
	TenantID         int64
	URL              string
	Enabled          bool
	SecretCiphertext []byte
}

// RecordDeliveryParams 描述一次投递尝试落库及熔断计数更新所需参数。
type RecordDeliveryParams struct {
	TenantID               int64
	WebhookID              int64
	JobID                  int64
	DeliveryID             string
	Event                  EventType
	Attempt                int
	Status                 DeliveryStatus
	HTTPStatus             *int
	DurationMs             int
	ErrorMessage           string
	ResponseSnippet        string
	TerminalFailure        bool
	IsPing                 bool
	MaxConsecutiveFailures int
}

// DeliveryListCursor 是投递历史列表的 Keyset 游标。
type DeliveryListCursor struct {
	ID int64
}

// DeliveryListOptions 控制投递历史查询参数。
type DeliveryListOptions struct {
	TenantID  int64
	WebhookID int64
	Limit     int
	Cursor    *DeliveryListCursor
}

// CreateParams 是创建 Webhook 订阅的存储参数。
type CreateParams struct {
	TenantID         int64
	URL              string
	Description      string
	Events           []EventType
	SecretCiphertext []byte
	SecretPrefix     string
	CreatedBy        *int64
}

// UpdateParams 是局部修改 Webhook 订阅的存储参数。
type UpdateParams struct {
	TenantID    int64
	WebhookID   int64
	URL         *string
	Description *string
	Events      []EventType
	Enabled     *bool
}

// TestEnqueueResult 是入队测试 webhook.ping 任务的标识。
type TestEnqueueResult struct {
	JobID      int64
	DeliveryID string
}

// DeliverPayload 是 jobs.payload 中存放的精简事件事实（不含 URL 和密钥）。
type DeliverPayload struct {
	WebhookID  int64           `json:"webhook_id"`
	EventID    string          `json:"event_id"`
	Event      EventType       `json:"event"`
	OccurredAt time.Time       `json:"occurred_at"`
	Data       json.RawMessage `json:"data"`
}

// Envelope 是实际发往订阅端点的统一 JSON 信封格式。
type Envelope struct {
	ID         string          `json:"id"`
	Event      EventType       `json:"event"`
	APIVersion string          `json:"api_version"`
	CreatedAt  time.Time       `json:"created_at"`
	TenantID   int64           `json:"tenant_id"`
	Data       json.RawMessage `json:"data"`
}
