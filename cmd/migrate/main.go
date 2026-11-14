/*
Command migrate 把 PostgreSQL 迁到最新结构。

相当于 Java 里的 Flyway：每个版本一对 up/down SQL，执行记录在 schema_migrations。
不在 HTTP 进程启动时跑。redirector 和 console 以后会同时启动，改表是部署步骤。

连接用 golang-migrate 的 postgres 驱动（lib/pq），这样 DATABASE_URL 保持
postgres://，不用改成 pgx5://。业务进程仍然用 pgx。
*/
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"
)

// errUsage 表示参数不对。main 用它选择退出码 2，并且不再重复打印错误。
var errUsage = errors.New("usage")

const usageText = `用法:
  migrate up [-database URL]
  migrate down [-database URL]
  migrate version [-database URL]

只允许库名 shortlink 或以 shortlink_ 开头的库：
shortlink 是默认库，shortlink_test 是测试库，部署可用 shortlink_ 前缀区分环境。
up 把库迁到最新。down 只允许库名以 _test 结尾，会把业务表一路降到没有。
未写 -database 时用 DATABASE_URL。在仓库根目录执行，SQL 在 migrations/。
`

type direction int

const (
	dirUp direction = iota
	dirDown
	dirVersion
)

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr, applyMigrations); err != nil {
		if !errors.Is(err, errUsage) {
			_, _ = fmt.Fprintf(os.Stderr, "migrate: %v\n", err)
		}
		if errors.Is(err, errUsage) {
			os.Exit(2)
		}
		os.Exit(1)
	}
}

func writeUsage(w io.Writer) {
	_, _ = fmt.Fprint(w, usageText)
}

func run(args []string, stdout, stderr io.Writer, apply func(direction, string) (string, error)) error {
	if len(args) == 0 {
		writeUsage(stderr)
		return errUsage
	}
	dir, ok := parseDirection(args[0])
	if !ok {
		writeUsage(stderr)
		return errUsage
	}

	fs := flag.NewFlagSet("migrate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dsnFlag := fs.String("database", "", "覆盖 DATABASE_URL")
	fs.Usage = func() { writeUsage(stderr) }
	if err := fs.Parse(args[1:]); err != nil {
		return errUsage
	}
	if fs.NArg() != 0 {
		writeUsage(stderr)
		return errUsage
	}

	dsn := *dsnFlag
	if dsn == "" {
		dsn = os.Getenv("DATABASE_URL")
	}
	if dsn == "" {
		return errors.New("DATABASE_URL is required")
	}
	name, err := databaseName(dsn)
	if err != nil {
		return err
	}
	if err := allow(dir, name); err != nil {
		return err
	}

	version, err := apply(dir, dsn)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "schema version: %s\n", version)
	return err
}

func parseDirection(name string) (direction, bool) {
	switch name {
	case "up":
		return dirUp, true
	case "down":
		return dirDown, true
	case "version":
		return dirVersion, true
	default:
		return 0, false
	}
}

/*
databaseName 取出真正会连上的库名。
lib/pq 同时接受路径和查询参数里的 dbname，只看路径时，
查询参数可以把连接指到另一个库。解析失败时不回显原始字符串，避免密码被打出来。
*/
func databaseName(dsn string) (string, error) {
	u, err := url.Parse(dsn)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		return "", errors.New("database url is invalid")
	}
	pathName := strings.TrimPrefix(u.Path, "/")
	if strings.Contains(pathName, "/") {
		return "", errors.New("database url has no database name")
	}

	queryName := ""
	seenQuery := false
	for _, n := range u.Query()["dbname"] {
		if n == "" {
			continue
		}
		if seenQuery && n != queryName {
			return "", errors.New("database url has conflicting dbname")
		}
		queryName = n
		seenQuery = true
	}
	if seenQuery && pathName != "" && queryName != pathName {
		return "", errors.New("database url has conflicting dbname")
	}
	name := pathName
	if seenQuery {
		name = queryName
	}
	if name == "" {
		return "", errors.New("database url has no database name")
	}
	return name, nil
}

/*
allow 只放行本项目的库。
项目库叫 shortlink，测试库叫 shortlink_test，部署环境用 shortlink_ 前缀区分。
down 会删表，只留给名字以 _test 结尾的测试库。
*/
func allow(dir direction, name string) error {
	if name != "shortlink" && !strings.HasPrefix(name, "shortlink_") {
		return fmt.Errorf("refusing to migrate %q: not a shortlink database", name)
	}
	if dir == dirDown && !strings.HasSuffix(name, "_test") {
		return fmt.Errorf("down only allows a database name ending in _test, got %q", name)
	}
	return nil
}

func applyMigrations(dir direction, dsn string) (string, error) {
	abs, err := filepath.Abs("migrations")
	if err != nil {
		return "", err
	}
	info, err := os.Stat(abs)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("migrations directory %s not found; run from the repository root", abs)
	}

	source := (&url.URL{Scheme: "file", Path: abs}).String()
	m, err := migrate.New(source, dsn)
	if err != nil {
		return "", fmt.Errorf("open migrations: %w", err)
	}

	var opErr error
	switch dir {
	case dirUp:
		opErr = m.Up()
	case dirDown:
		opErr = m.Down()
	case dirVersion:
	}
	if opErr != nil && !errors.Is(opErr, migrate.ErrNoChange) {
		_, _ = m.Close()
		return "", opErr
	}

	version, dirty, err := m.Version()
	srcErr, dbErr := m.Close()
	if err != nil && !errors.Is(err, migrate.ErrNilVersion) {
		return "", errors.Join(err, srcErr, dbErr)
	}
	if closeErr := errors.Join(srcErr, dbErr); closeErr != nil {
		return "", closeErr
	}
	if errors.Is(err, migrate.ErrNilVersion) {
		return "none", nil
	}
	if dirty {
		return "", fmt.Errorf("schema version %d is dirty", version)
	}
	return fmt.Sprintf("%d", version), nil
}
