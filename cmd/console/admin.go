package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"shortlink/internal/cache"
	"shortlink/internal/identity"
	"shortlink/internal/link"
	"shortlink/internal/postgres"
)

// errUsage 表示参数不对。main 用它选择退出码 2，并且不再重复打印错误。
var errUsage = errors.New("usage")

const usageText = `用法:
  console serve
  console key issue --name NAME [--external-id ID] [--expires RFC3339] [--scope SCOPE]...
  console key issue --tenant ID [--expires RFC3339] [--scope SCOPE]...
  console key list --tenant ID
  console key revoke --id ID
  console tenant show --tenant ID
  console tenant set --tenant ID [--link-quota N] [--rate-limit N]
  console member invite --tenant ID --email EMAIL --role ROLE [--expires RFC3339]
  console link ban --code CODE --reason REASON
  console link unban --code CODE

serve 启动管理 HTTP（短链增删改查和统计），需要 DATABASE_URL 和 REDIS_URL。
issue 成功时把明文打印到标准输出，只此一次。--expires 省略表示不过期；
--scope 省略表示拥有全部密钥权限（links:read、links:write、stats:read），
可重复传入或用逗号分隔。
--name 会新建租户；已有租户用 --tenant。密钥、租户和成员命令只读 DATABASE_URL，不连接 Redis。
member invite 预先登记组织邀请，--role 取 owner、admin、member、viewer；
用户使用已验证的同一邮箱通过 OIDC 登录时自动兑现。
link ban/unban 封禁和解封短链，需要 DATABASE_URL 和 REDIS_URL，先写数据库再写缓存；
--reason 取 phishing、malware、adult、gambling、drugs、spam、other。
tenant show 看本月用量、配额和限流；set 至少给一个选项。--link-quota 是每个 UTC
自然月最多能创建的链接数，0 表示不能创建；--rate-limit 是管理 API 每秒请求数，
0 表示不限制。
`

func writeUsage(w io.Writer) {
	_, _ = fmt.Fprint(w, usageText)
}

// adminStore 是运维命令用到的存储能力。接口放在使用方，测试用替身替换数据库。
type adminStore interface {
	IssueAPIKey(ctx context.Context, p postgres.IssueAPIKeyParams) (postgres.IssuedAPIKey, error)
	ListAPIKeys(ctx context.Context, tenantID int64) ([]postgres.APIKey, error)
	RevokeAPIKey(ctx context.Context, keyID int64) error
	GetTenantLimits(ctx context.Context, tenantID int64) (postgres.TenantLimits, error)
	SetTenantLimits(ctx context.Context, tenantID int64, p postgres.SetTenantLimitsParams) (postgres.TenantLimits, error)
	CreateInvitation(ctx context.Context, p postgres.CreateInvitationParams) (identity.Invitation, error)
}

/*
linkStore 是 link 命令用到的下架能力。它背后是 cache.Store：
先写 PostgreSQL，再维护 Redis，缓存写失败会返回 cache.ErrNotUpdated。
*/
type linkStore interface {
	Ban(ctx context.Context, code string, reason link.BanReason, source link.BanSource) (*link.Link, error)
	Unban(ctx context.Context, code string) (*link.Link, error)
}

type command func(ctx context.Context, store adminStore, stdout io.Writer) error

type linkCommand func(ctx context.Context, links linkStore, stdout io.Writer) error

func parseCommand(args []string, stderr io.Writer) (command, error) {
	if len(args) == 0 {
		writeUsage(stderr)
		return nil, errUsage
	}
	if args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		writeUsage(stderr)
		return nil, nil
	}
	if len(args) < 2 {
		writeUsage(stderr)
		return nil, errUsage
	}
	switch args[0] {
	case "key":
		switch args[1] {
		case "issue":
			return parseIssue(args[2:], stderr)
		case "list":
			return parseList(args[2:], stderr)
		case "revoke":
			return parseRevoke(args[2:], stderr)
		}
	case "tenant":
		switch args[1] {
		case "show":
			return parseTenantShow(args[2:], stderr)
		case "set":
			return parseTenantSet(args[2:], stderr)
		}
	case "member":
		switch args[1] {
		case "invite":
			return parseMemberInvite(args[2:], stderr)
		}
	}
	writeUsage(stderr)
	return nil, errUsage
}

func newFlagSet(name string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { writeUsage(stderr) }
	return fs
}

/*
parseFlags 解析子命令参数并拒绝多余位置参数。
返回 (false, nil) 表示用户请求了 -h/--help；返回 (false, errUsage) 表示参数非法。
*/
func parseFlags(fs *flag.FlagSet, args []string, stderr io.Writer) (bool, error) {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return false, nil
		}
		return false, errUsage
	}
	if fs.NArg() != 0 {
		writeUsage(stderr)
		return false, errUsage
	}
	return true, nil
}

/*
parseLinkCommand 解析 link 子命令。link 需要 PostgreSQL 和 Redis，
和 key、tenant 的存储分开打开，见 run 里的分派。
*/
func parseLinkCommand(args []string, stderr io.Writer) (linkCommand, error) {
	if len(args) == 0 {
		writeUsage(stderr)
		return nil, errUsage
	}
	if args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		writeUsage(stderr)
		return nil, nil
	}
	switch args[0] {
	case "ban":
		return parseLinkBan(args[1:], stderr)
	case "unban":
		return parseLinkUnban(args[1:], stderr)
	}
	writeUsage(stderr)
	return nil, errUsage
}

// parseLinkBan 要求短码和合法的封禁原因；来源固定是平台运维。
func parseLinkBan(args []string, stderr io.Writer) (linkCommand, error) {
	fs := newFlagSet("link ban", stderr)
	code := fs.String("code", "", "短码")
	reason := fs.String("reason", "", "封禁原因")
	if ok, err := parseFlags(fs, args, stderr); !ok {
		return nil, err
	}
	banReason := link.BanReason(*reason)
	if *code == "" || !banReason.Valid() {
		writeUsage(stderr)
		return nil, errUsage
	}
	codeValue := *code
	return func(ctx context.Context, links linkStore, stdout io.Writer) error {
		banned, err := links.Ban(ctx, codeValue, banReason, link.BanSourceOperator)
		if err != nil {
			return explainTakedownError("封禁", codeValue, err)
		}
		return printLinkState(stdout, banned)
	}, nil
}

func parseLinkUnban(args []string, stderr io.Writer) (linkCommand, error) {
	fs := newFlagSet("link unban", stderr)
	code := fs.String("code", "", "短码")
	if ok, err := parseFlags(fs, args, stderr); !ok {
		return nil, err
	}
	if *code == "" {
		writeUsage(stderr)
		return nil, errUsage
	}
	codeValue := *code
	return func(ctx context.Context, links linkStore, stdout io.Writer) error {
		unbanned, err := links.Unban(ctx, codeValue)
		if err != nil {
			return explainTakedownError("解封", codeValue, err)
		}
		return printLinkState(stdout, unbanned)
	}, nil
}

/*
explainTakedownError 把「库写成功、缓存没更新」翻译成操作提示。
其他错误原样返回：短码不存在、数据库错误由 main 打印。
*/
func explainTakedownError(action, code string, err error) error {
	if !errors.Is(err, cache.ErrNotUpdated) {
		return err
	}
	return fmt.Errorf("%s %s 已经写入数据库，但缓存没有更新：旧的正缓存最长还能跳转 1 小时；"+
		"重跑同一条命令即可，操作幂等: %w", action, code, err)
}

/*
printLinkState 输出一条链接的下架结果，风格同 tenant show：表头一行、数据一行。
解封后状态可能是 active，也可能仍然是 disabled 或 expired；封禁字段为空时输出 -。
*/
func printLinkState(w io.Writer, l *link.Link) error {
	if _, err := fmt.Fprint(w, "code\tstatus\tbanned_at\tban_reason\tban_source\n"); err != nil {
		return err
	}
	bannedAt, reason, source := "-", "-", "-"
	if l.Ban != nil {
		bannedAt = l.Ban.At.UTC().Format(time.RFC3339)
		reason = string(l.Ban.Reason)
		source = string(l.Ban.Source)
	}
	_, err := fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", l.Code, l.Status(time.Now()), bannedAt, reason, source)
	return err
}

// scopeFlag 收集命令行里的 --scope 选项，支持重复传入和逗号分隔。
type scopeFlag struct {
	set bool
	raw []string
}

func (f *scopeFlag) String() string {
	return strings.Join(f.raw, ",")
}

func (f *scopeFlag) Set(v string) error {
	f.set = true
	f.raw = append(f.raw, v)
	return nil
}

func parseIssue(args []string, stderr io.Writer) (command, error) {
	fs := newFlagSet("issue", stderr)
	name := fs.String("name", "", "新租户名称")
	externalID := fs.String("external-id", "", "外部租户编号，只在新建租户时写入")
	tenantID := fs.Int64("tenant", 0, "已有租户 ID")
	expires := fs.String("expires", "", "RFC3339 到期时间，省略表示不过期")
	var scopes scopeFlag
	fs.Var(&scopes, "scope", "权限范围（links:read、links:write、stats:read），可重复传入或用逗号分隔")
	if ok, err := parseFlags(fs, args, stderr); !ok {
		return nil, err
	}

	params := postgres.IssueAPIKeyParams{
		TenantID:   *tenantID,
		Name:       *name,
		ExternalID: *externalID,
	}
	if *expires != "" {
		t, err := time.Parse(time.RFC3339, *expires)
		if err != nil {
			return nil, fmt.Errorf("parse expires: %w", err)
		}
		params.ExpiresAt = &t
	}
	if scopes.set {
		parsed, err := parseScopeFlags(scopes.raw)
		if err != nil {
			return nil, fmt.Errorf("parse scope: %w", err)
		}
		params.Scopes = parsed
	} else {
		// 省略 --scope 仍是全部密钥权限，但要显式传入。存储层不把 nil 当成全权限。
		params.Scopes = identity.AllAPIKeyScopes()
	}
	return func(ctx context.Context, store adminStore, stdout io.Writer) error {
		issued, err := store.IssueAPIKey(ctx, params)
		if err != nil {
			return err
		}
		return printIssued(stdout, issued)
	}, nil
}

func parseScopeFlags(values []string) ([]identity.Perm, error) {
	var perms []identity.Perm
	for _, value := range values {
		for part := range strings.SplitSeq(value, ",") {
			perms = append(perms, identity.Perm(strings.TrimSpace(part)))
		}
	}
	return identity.NormalizeAPIKeyScopes(perms)
}

func parseList(args []string, stderr io.Writer) (command, error) {
	fs := newFlagSet("list", stderr)
	tenantID := fs.Int64("tenant", 0, "租户 ID")
	if ok, err := parseFlags(fs, args, stderr); !ok {
		return nil, err
	}
	if *tenantID <= 0 {
		writeUsage(stderr)
		return nil, errUsage
	}
	id := *tenantID
	return func(ctx context.Context, store adminStore, stdout io.Writer) error {
		keys, err := store.ListAPIKeys(ctx, id)
		if err != nil {
			return err
		}
		return printKeys(stdout, keys)
	}, nil
}

func parseRevoke(args []string, stderr io.Writer) (command, error) {
	fs := newFlagSet("revoke", stderr)
	keyID := fs.Int64("id", 0, "密钥 ID")
	if ok, err := parseFlags(fs, args, stderr); !ok {
		return nil, err
	}
	if *keyID <= 0 {
		writeUsage(stderr)
		return nil, errUsage
	}
	id := *keyID
	return func(ctx context.Context, store adminStore, stdout io.Writer) error {
		if err := store.RevokeAPIKey(ctx, id); err != nil {
			return err
		}
		_, err := fmt.Fprintf(stdout, "revoked: %d\n", id)
		return err
	}, nil
}

func parseTenantShow(args []string, stderr io.Writer) (command, error) {
	fs := newFlagSet("tenant show", stderr)
	tenantID := fs.Int64("tenant", 0, "租户 ID")
	if ok, err := parseFlags(fs, args, stderr); !ok {
		return nil, err
	}
	if *tenantID <= 0 {
		writeUsage(stderr)
		return nil, errUsage
	}
	id := *tenantID
	return func(ctx context.Context, store adminStore, stdout io.Writer) error {
		limits, err := store.GetTenantLimits(ctx, id)
		if err != nil {
			return err
		}
		return printTenantLimits(stdout, limits)
	}, nil
}

/*
parseTenantSet 至少要求一个限制选项。
用 flag.Visit 判断选项有没有出现：默认值和显式传 0 要能区分开，
把 --rate-limit 0 当成「没传」会改不掉已有的值。
*/
func parseTenantSet(args []string, stderr io.Writer) (command, error) {
	fs := newFlagSet("tenant set", stderr)
	tenantID := fs.Int64("tenant", 0, "租户 ID")
	linkQuota := fs.Int64("link-quota", 0, "每个 UTC 自然月最多创建的链接数")
	rateLimit := fs.Int("rate-limit", 0, "管理 API 每秒请求数，0 表示不限制")
	if ok, err := parseFlags(fs, args, stderr); !ok {
		return nil, err
	}
	if *tenantID <= 0 {
		writeUsage(stderr)
		return nil, errUsage
	}

	set := make(map[string]bool)
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	if !set["link-quota"] && !set["rate-limit"] {
		writeUsage(stderr)
		return nil, errUsage
	}
	if *linkQuota < 0 || *rateLimit < 0 {
		writeUsage(stderr)
		return nil, errUsage
	}

	params := postgres.SetTenantLimitsParams{}
	if set["link-quota"] {
		params.LinkQuota = linkQuota
	}
	if set["rate-limit"] {
		params.RateLimit = rateLimit
	}
	id := *tenantID
	return func(ctx context.Context, store adminStore, stdout io.Writer) error {
		limits, err := store.SetTenantLimits(ctx, id, params)
		if err != nil {
			return err
		}
		return printTenantLimits(stdout, limits)
	}, nil
}

func parseMemberInvite(args []string, stderr io.Writer) (command, error) {
	fs := newFlagSet("member invite", stderr)
	tenantID := fs.Int64("tenant", 0, "租户 ID")
	email := fs.String("email", "", "受邀人邮箱")
	roleStr := fs.String("role", "", "成员角色（owner、admin、member、viewer）")
	expires := fs.String("expires", "", "RFC3339 到期时间，省略表示不过期")
	if ok, err := parseFlags(fs, args, stderr); !ok {
		return nil, err
	}
	if *tenantID <= 0 || strings.TrimSpace(*email) == "" || *roleStr == "" {
		writeUsage(stderr)
		return nil, errUsage
	}
	role := identity.Role(*roleStr)
	if !role.Valid() {
		writeUsage(stderr)
		return nil, errUsage
	}
	normalizedEmail, err := identity.NormalizeEmail(*email)
	if err != nil {
		return nil, fmt.Errorf("parse email: %w", err)
	}
	params := postgres.CreateInvitationParams{
		TenantID: *tenantID,
		Email:    normalizedEmail,
		Role:     role,
	}
	if *expires != "" {
		t, err := time.Parse(time.RFC3339, *expires)
		if err != nil {
			return nil, fmt.Errorf("parse expires: %w", err)
		}
		params.ExpiresAt = &t
	}
	return func(ctx context.Context, store adminStore, stdout io.Writer) error {
		inv, err := store.CreateInvitation(ctx, params)
		if err != nil {
			return err
		}
		return printInvitation(stdout, inv)
	}, nil
}

func printInvitation(w io.Writer, inv identity.Invitation) error {
	if _, err := fmt.Fprint(w, "id\ttenant_id\temail\trole\tcreated_at\texpires_at\n"); err != nil {
		return err
	}
	_, err := fmt.Fprintf(w, "%d\t%d\t%s\t%s\t%s\t%s\n",
		inv.ID, inv.TenantID, inv.Email, inv.Role,
		inv.CreatedAt.UTC().Format(time.RFC3339),
		formatOptional(inv.ExpiresAt),
	)
	return err
}

func printTenantLimits(w io.Writer, limits postgres.TenantLimits) error {
	if _, err := fmt.Fprint(w, "id\tname\tlink_usage\tlink_quota\trate_limit_per_second\n"); err != nil {
		return err
	}
	_, err := fmt.Fprintf(w, "%d\t%s\t%d\t%d\t%d\n",
		limits.ID, limits.Name, limits.LinkUsage, limits.LinkQuota, limits.RateLimit)
	return err
}

func printIssued(w io.Writer, issued postgres.IssuedAPIKey) error {
	if _, err := fmt.Fprintf(w, "tenant_id: %d\nkey_id: %d\nscopes: %s\n",
		issued.Tenant.ID, issued.Key.ID, formatScopes(issued.Key.Scopes)); err != nil {
		return err
	}
	if issued.Key.ExpiresAt != nil {
		if _, err := fmt.Fprintf(w, "expires_at: %s\n", issued.Key.ExpiresAt.UTC().Format(time.RFC3339)); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintf(w, "api_key: %s\n", issued.Plaintext)
	return err
}

func printKeys(w io.Writer, keys []postgres.APIKey) error {
	if _, err := fmt.Fprint(w, "id\ttenant_id\tcreated_at\texpires_at\trevoked_at\tscopes\n"); err != nil {
		return err
	}
	for _, key := range keys {
		if _, err := fmt.Fprintf(w, "%d\t%d\t%s\t%s\t%s\t%s\n",
			key.ID, key.TenantID,
			key.CreatedAt.UTC().Format(time.RFC3339),
			formatOptional(key.ExpiresAt),
			formatOptional(key.RevokedAt),
			formatScopes(key.Scopes),
		); err != nil {
			return err
		}
	}
	return nil
}

func formatScopes(scopes []identity.Perm) string {
	if len(scopes) == 0 {
		return "-"
	}
	parts := make([]string, len(scopes))
	for i, scope := range scopes {
		parts[i] = string(scope)
	}
	return strings.Join(parts, ",")
}

func formatOptional(t *time.Time) string {
	if t == nil {
		return "-"
	}
	return t.UTC().Format(time.RFC3339)
}
