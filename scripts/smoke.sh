#!/bin/sh
# 冒烟测试：启动真实进程，按 docs/api.md 的契约走一遍健康检查、创建、跳转、
# HEAD、404、非法参数、过期和指标，最后检查 SIGTERM 优雅退出。
#
# 由 make smoke 调用；也可以直接 ./scripts/smoke.sh，前提是 bin/server 已构建、
# 测试库已准备。脚本用独立端口、测试库和测试 Redis，不碰本机正在跑的服务和开发库。
set -eu

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$ROOT"

# 有的开发环境给 curl 配了 HTTP 代理，访问 127.0.0.1 也会被转走。冒烟只打本机。
NO_PROXY=127.0.0.1,localhost
no_proxy=$NO_PROXY
export NO_PROXY no_proxy

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

LOG=$(mktemp)
TMP=$(mktemp -d)
SERVER_PID=

cleanup() {
	if [ -n "${SERVER_PID:-}" ] && kill -0 "$SERVER_PID" 2>/dev/null; then
		kill -TERM "$SERVER_PID" 2>/dev/null || true
		i=0
		while kill -0 "$SERVER_PID" 2>/dev/null && [ "$i" -lt 100 ]; do
			i=$((i + 1))
			sleep 0.1
		done
		kill -KILL "$SERVER_PID" 2>/dev/null || true
	fi
	wait "$SERVER_PID" 2>/dev/null || true
	rm -f "$LOG"
	rm -rf "$TMP"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

fail() {
	printf 'smoke: %s\n' "$*" >&2
	if [ -s "$LOG" ]; then
		printf '%s\n' '--- server log (last 20 lines) ---' >&2
		tail -n 20 "$LOG" >&2
	fi
	exit 1
}

assert_eq() { # got want message
	[ "$1" = "$2" ] || fail "$3: got '$1', want '$2'"
}

psql_cmd() { # url [psql args...]
	url=$1
	shift
	if command -v psql >/dev/null 2>&1; then
		psql "$url" "$@"
	else
		docker run --rm -i --network host postgres:17-alpine psql "$url" "$@"
	fi
}

# seed_smoke_key 往测试库写入一把固定 API Key。明文只留在这个脚本里。
# 准备脚本会清掉租户表，所以每次冒烟都要重新种，不能提前写进迁移。
seed_smoke_key() {
	hash=$(printf '%s' "$SMOKE_API_KEY" | sha256sum | awk '{print $1}')
	psql_cmd "$TEST_DATABASE_URL" -v ON_ERROR_STOP=1 -q <<SQL || fail '写入冒烟 API Key 失败'
INSERT INTO tenants (name)
SELECT 'smoke'
WHERE NOT EXISTS (
    SELECT 1 FROM api_keys WHERE key_hash = decode('${hash}', 'hex')
);
INSERT INTO api_keys (tenant_id, key_hash)
SELECT id, decode('${hash}', 'hex')
FROM tenants
WHERE name = 'smoke'
ORDER BY id
LIMIT 1
ON CONFLICT (key_hash) DO NOTHING;
SQL
}

# request 把状态码写到 stdout，响应体和响应头分别放进 $TMP/body、$TMP/headers。
request() { # method path [json]
	method=$1
	path=$2
	body=${3:-}
	if [ -n "$body" ]; then
		curl -sS --max-time 10 -o "$TMP/body" -D "$TMP/headers" -w '%{http_code}' \
			-X "$method" -H 'Content-Type: application/json' \
			-H "Authorization: Bearer $SMOKE_API_KEY" \
			-d "$body" "$BASE$path"
	else
		curl -sS --max-time 10 -o "$TMP/body" -D "$TMP/headers" -w '%{http_code}' \
			-X "$method" "$BASE$path"
	fi
}

header() { # name
	grep -i "^$1:" "$TMP/headers" | head -n 1 | sed 's/^[^:]*:[[:space:]]*//' | tr -d '\r'
}

random_code() {
	# 8 位 Base62，和服务的短码格式一致。
	LC_ALL=C tr -dc '0-9A-Za-z' </dev/urandom | head -c 8
}

[ -x bin/server ] || fail 'bin/server 不存在，先运行 make build'

# 只允许打在测试库和测试 Redis 上，避免把冒烟数据写进开发库。
[ -n "${TEST_DATABASE_URL:-}" ] || fail 'TEST_DATABASE_URL 未设置，冒烟只允许使用测试库'
[ -n "${TEST_REDIS_URL:-}" ] || fail 'TEST_REDIS_URL 未设置'
DATABASE_URL=$TEST_DATABASE_URL
REDIS_URL=$TEST_REDIS_URL
export DATABASE_URL REDIS_URL

ADDR=${SMOKE_ADDR:-127.0.0.1:18080}
BASE=http://$ADDR
export ADDR
export LOG_LEVEL=error

# 64 位小写十六进制。只存在于测试库，和开发库无关。
SMOKE_API_KEY=slk_00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff
seed_smoke_key

./bin/server >"$LOG" 2>&1 &
SERVER_PID=$!

# 等 healthz 就绪，最多 15 秒。进程提前退出时把日志带出来。
i=0
while ! curl -fsS --max-time 1 "$BASE/healthz" >/dev/null 2>&1; do
	if ! kill -0 "$SERVER_PID" 2>/dev/null; then
		fail '服务启动后立即退出'
	fi
	i=$((i + 1))
	[ "$i" -ge 150 ] && fail '服务 15 秒内没有就绪'
	sleep 0.1
done

echo 'smoke: healthz'
status=$(request GET /healthz)
assert_eq "$status" 200 'GET /healthz 状态码'
assert_eq "$(cat "$TMP/body")" 'ok' 'GET /healthz 响应体'

echo 'smoke: unauthorized'
status=$(curl -sS --max-time 10 -o "$TMP/body" -w '%{http_code}' \
	-X POST -H 'Content-Type: application/json' \
	-d '{"url":"https://example.com/no-key"}' "$BASE/api/v1/links")
assert_eq "$status" 401 '没有 API Key 的创建'
grep -q '"unauthorized"' "$TMP/body" || fail '401 响应体不是 unauthorized'

echo 'smoke: create'
url="https://example.com/smoke-$$-$(date +%s)"
status=$(request POST /api/v1/links "{\"url\":\"$url\"}")
assert_eq "$status" 201 'POST /api/v1/links 状态码'
location=$(header Location)
case "$location" in
	/r/*) ;;
	*) fail "Location 格式不对: $location" ;;
esac
code=${location#/r/}
grep -q "\"code\":\"$code\"" "$TMP/body" || fail '创建响应里没有 code'
grep -q "\"url\":\"$url\"" "$TMP/body" || fail '创建响应里没有 url'

echo 'smoke: redirect'
status=$(request GET "/r/$code")
assert_eq "$status" 302 'GET /r/{code} 状态码'
assert_eq "$(header Location)" "$url" '跳转 Location'

echo 'smoke: head'
status=$(curl -sS --max-time 10 -I -o /dev/null -w '%{http_code}' "$BASE/r/$code")
assert_eq "$status" 302 'HEAD /r/{code} 状态码'

echo 'smoke: not found'
status=$(request GET "/r/$(random_code)")
assert_eq "$status" 404 '不存在的短码'
grep -q '"error"' "$TMP/body" || fail '404 响应体不是 JSON error'

echo 'smoke: invalid url'
status=$(request POST /api/v1/links '{"url":"ftp://example.com"}')
assert_eq "$status" 400 '非法 URL'
grep -q '"error"' "$TMP/body" || fail '400 响应体不是 JSON error'

echo 'smoke: expiry'
# GNU date；CI 和 WSL2 都是 Linux。留 3 秒余量，再等 4 秒确保越过到期时刻。
expires_at=$(date -u -d '+3 seconds' '+%Y-%m-%dT%H:%M:%SZ')
status=$(request POST /api/v1/links "{\"url\":\"https://example.com/expired\",\"expires_at\":\"$expires_at\"}")
assert_eq "$status" 201 '创建带过期时间的链接'
grep -q '"expires_at"' "$TMP/body" || fail '创建响应里没有 expires_at'
location=$(header Location)
expired_code=${location#/r/}
status=$(request GET "/r/$expired_code")
assert_eq "$status" 302 '过期前跳转'
sleep 4
status=$(request GET "/r/$expired_code")
assert_eq "$status" 404 '过期后不跳转'

echo 'smoke: metrics'
status=$(request GET /metrics)
assert_eq "$status" 200 'GET /metrics 状态码'
grep -q 'shortlink_http_requests_total' "$TMP/body" || fail '指标里没有 shortlink_http_requests_total'

echo 'smoke: shutdown'
# 看门狗：服务卡在关停流程里时最多等 20 秒，避免冒烟把 CI 拖到 job 超时。
( sleep 20; kill -KILL "$SERVER_PID" 2>/dev/null ) &
watchdog=$!
kill -TERM "$SERVER_PID"
status=0
wait "$SERVER_PID" || status=$?
kill "$watchdog" 2>/dev/null || true
wait "$watchdog" 2>/dev/null || true
SERVER_PID=
assert_eq "$status" 0 'SIGTERM 后的退出码'

echo 'smoke: ok'
