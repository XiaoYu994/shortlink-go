#!/bin/sh
# 准备 TEST_DATABASE_URL 指向的测试库：库不存在就创建，然后清掉 public
# 并执行 migrations/。先 up、再 down、再 up，确认回滚 SQL 也能用。
# 只动测试库：库名必须以 _test 结尾。
#
# 不负责启动 PostgreSQL/Redis：用项目自带的 compose 时先 make deps-up，
# 用自己的实例时直接配置 .env。没有本机 psql 时用 postgres 镜像里的客户端，
# --network host 让容器里的 127.0.0.1 指向宿主机的数据库。
set -eu

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$ROOT"

# make 已经把 .env 导出给子进程；直接运行时自己加载一份。
# 显式传入的 TEST_* 优先于 .env，方便临时指向别的测试库。
explicit_db=${TEST_DATABASE_URL:-}
explicit_redis=${TEST_REDIS_URL:-}
if [ -f .env ]; then
	set -a
	. ./.env
	set +a
fi
if [ -n "$explicit_db" ]; then TEST_DATABASE_URL=$explicit_db; fi
if [ -n "$explicit_redis" ]; then TEST_REDIS_URL=$explicit_redis; fi

[ -n "${TEST_DATABASE_URL:-}" ] || { echo "TEST_DATABASE_URL 未设置，先 cp .env.example .env" >&2; exit 1; }
[ -n "${TEST_REDIS_URL:-}" ] || { echo "TEST_REDIS_URL 未设置，先 cp .env.example .env" >&2; exit 1; }

# 查询参数里的 dbname 会改掉实际连上的库，只检查路径拦不住。
case $TEST_DATABASE_URL in
	*\?*)
		rest=${TEST_DATABASE_URL#*\?}
		case "&${rest}&" in
			*"&dbname="*)
				echo "TEST_DATABASE_URL 的库名要写在路径里，不要用 dbname 查询参数" >&2
				exit 1
				;;
		esac
		;;
esac

# 从 URL 里取库名：postgres://user:pass@host:port/dbname?params
db=${TEST_DATABASE_URL#*://}
db=${db#*/}
db=${db%%\?*}
case "$db" in
	*_test) ;;
	*) echo "TEST_DATABASE_URL 的库名必须以 _test 结尾，当前是 '$db'" >&2; exit 1 ;;
esac
case "$db" in
	*[!0-9A-Za-z_]*) echo "测试库名只能包含字母、数字和下划线，当前是 '$db'" >&2; exit 1 ;;
esac

# 同一实例的维护库连接串，用来幂等创建测试库。
url_base=${TEST_DATABASE_URL%%\?*}
url_query=
case "$TEST_DATABASE_URL" in
	*\?*) url_query="?${TEST_DATABASE_URL#*\?}" ;;
esac
admin_url=${url_base%/*}/postgres$url_query

psql_cmd() { # url [psql args...]
	url=$1
	shift
	if command -v psql >/dev/null 2>&1; then
		psql "$url" "$@"
	else
		docker run --rm -i --network host postgres:17-alpine psql "$url" "$@"
	fi
}

exists=$(psql_cmd "$admin_url" -v ON_ERROR_STOP=1 -Atc \
	"SELECT 1 FROM pg_database WHERE datname = '$db'") || {
	echo "连接 PostgreSQL 失败：检查 TEST_DATABASE_URL 和数据库是否可连" >&2
	exit 1
}
if [ "$exists" != 1 ]; then
	psql_cmd "$admin_url" -v ON_ERROR_STOP=1 -q -c "CREATE DATABASE \"$db\"" || {
		echo "创建测试库 '$db' 失败：确认数据库用户有 CREATEDB 权限" >&2
		exit 1
	}
fi

# heredoc 必须紧跟命令。失败处理写在后面，避免 } 被吃进 SQL。
if ! psql_cmd "$TEST_DATABASE_URL" -v ON_ERROR_STOP=1 -q -1 <<'SQL'
DROP SCHEMA IF EXISTS public CASCADE;
CREATE SCHEMA public;
SQL
then
	echo "准备测试库失败：检查 TEST_DATABASE_URL 和数据库是否可连" >&2
	exit 1
fi

migrate() { # up|down
	go run ./cmd/migrate "$1" -database "$TEST_DATABASE_URL" || {
		echo "测试库迁移失败：$1" >&2
		exit 1
	}
}
migrate up
migrate down
migrate up
