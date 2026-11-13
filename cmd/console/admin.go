package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"time"

	"shortlink/internal/postgres"
)

// errUsage 表示参数不对。main 用它选择退出码 2，并且不再重复打印错误。
var errUsage = errors.New("usage")

const usageText = `用法:
  console key issue --name NAME [--external-id ID] [--expires RFC3339]
  console key issue --tenant ID [--expires RFC3339]
  console key list --tenant ID
  console key revoke --id ID

issue 成功时把明文打印到标准输出，只此一次。--expires 省略表示不过期。
--name 会新建租户；已有租户用 --tenant。DATABASE_URL 从环境变量读取。
`

func writeUsage(w io.Writer) {
	_, _ = fmt.Fprint(w, usageText)
}

// adminStore 是运维命令用到的存储能力。接口放在使用方，测试用替身替换数据库。
type adminStore interface {
	IssueAPIKey(ctx context.Context, p postgres.IssueAPIKeyParams) (postgres.IssuedAPIKey, error)
	ListAPIKeys(ctx context.Context, tenantID int64) ([]postgres.APIKey, error)
	RevokeAPIKey(ctx context.Context, keyID int64) error
}

type command func(ctx context.Context, store adminStore, stdout io.Writer) error

func parseCommand(args []string, stderr io.Writer) (command, error) {
	if len(args) == 0 {
		writeUsage(stderr)
		return nil, errUsage
	}
	if args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		writeUsage(stderr)
		return nil, nil
	}
	if len(args) < 2 || args[0] != "key" {
		writeUsage(stderr)
		return nil, errUsage
	}
	switch args[1] {
	case "issue":
		return parseIssue(args[2:], stderr)
	case "list":
		return parseList(args[2:], stderr)
	case "revoke":
		return parseRevoke(args[2:], stderr)
	default:
		writeUsage(stderr)
		return nil, errUsage
	}
}

func newFlagSet(name string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { writeUsage(stderr) }
	return fs
}

func parseIssue(args []string, stderr io.Writer) (command, error) {
	fs := newFlagSet("issue", stderr)
	name := fs.String("name", "", "新租户名称")
	externalID := fs.String("external-id", "", "外部租户编号，只在新建租户时写入")
	tenantID := fs.Int64("tenant", 0, "已有租户 ID")
	expires := fs.String("expires", "", "RFC3339 到期时间，省略表示不过期")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil, nil
		}
		return nil, errUsage
	}
	if fs.NArg() != 0 {
		writeUsage(stderr)
		return nil, errUsage
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
	return func(ctx context.Context, store adminStore, stdout io.Writer) error {
		issued, err := store.IssueAPIKey(ctx, params)
		if err != nil {
			return err
		}
		return printIssued(stdout, issued)
	}, nil
}

func parseList(args []string, stderr io.Writer) (command, error) {
	fs := newFlagSet("list", stderr)
	tenantID := fs.Int64("tenant", 0, "租户 ID")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil, nil
		}
		return nil, errUsage
	}
	if fs.NArg() != 0 || *tenantID <= 0 {
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
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil, nil
		}
		return nil, errUsage
	}
	if fs.NArg() != 0 || *keyID <= 0 {
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

func printIssued(w io.Writer, issued postgres.IssuedAPIKey) error {
	if _, err := fmt.Fprintf(w, "tenant_id: %d\nkey_id: %d\n", issued.Tenant.ID, issued.Key.ID); err != nil {
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
	if _, err := fmt.Fprint(w, "id\ttenant_id\tcreated_at\texpires_at\trevoked_at\n"); err != nil {
		return err
	}
	for _, key := range keys {
		if _, err := fmt.Fprintf(w, "%d\t%d\t%s\t%s\t%s\n",
			key.ID, key.TenantID,
			key.CreatedAt.UTC().Format(time.RFC3339),
			formatOptional(key.ExpiresAt),
			formatOptional(key.RevokedAt),
		); err != nil {
			return err
		}
	}
	return nil
}

func formatOptional(t *time.Time) string {
	if t == nil {
		return "-"
	}
	return t.UTC().Format(time.RFC3339)
}
