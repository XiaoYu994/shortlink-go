/*
Package tenant 表示短链的租户（组织）。

API Key 与会话凭证的格式、生成与哈希定义在 identity 包，密钥的签发和吊销在 postgres 包（ADR 0019）。
*/
package tenant

/*
Tenant 是组织（租户）的基本信息与策略快照，鉴权后随 identity.Principal 放进 context。
ID 是本地主键；ExternalID 留给外部平台租户编号，没有时为空。
RateLimit 是管理 API 的每秒请求数，限流中间件据此调整进程内的令牌桶，
改库后下一次请求生效；0 表示不限制。
*/
type Tenant struct {
	ID         int64
	ExternalID string
	RateLimit  int
}
