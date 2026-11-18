#!/bin/sh
# 冒烟测试：启动 redirector 和 console 两个真实进程，按 docs/api.md 的契约走一遍
# 健康检查、创建、跳转、HEAD、404、非法参数、过期、路由隔离、访客去重、配额、限流和指标，
# 最后检查 SIGTERM 优雅退出。
#
# 由 make smoke 调用；也可以直接 ./scripts/smoke.sh，前提是 bin/redirector、bin/console
# 已构建、测试库已准备。脚本用独立端口、测试库和测试 Redis，不碰本机正在跑的服务和开发库。
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

REDIRECTOR_LOG=$(mktemp)
CONSOLE_LOG=$(mktemp)
TMP=$(mktemp -d)
REDIRECTOR_PID=
CONSOLE_PID=
WATCHDOG_PID=

cleanup() {
	# 先停看门狗，避免它在脚本退出后拿旧 PID 去 kill。
	if [ -n "${WATCHDOG_PID:-}" ]; then
		kill "$WATCHDOG_PID" 2>/dev/null || true
		wait "$WATCHDOG_PID" 2>/dev/null || true
	fi
	for pid in "${REDIRECTOR_PID:-}" "${CONSOLE_PID:-}"; do
		if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
			kill -TERM "$pid" 2>/dev/null || true
			i=0
			while kill -0 "$pid" 2>/dev/null && [ "$i" -lt 100 ]; do
				i=$((i + 1))
				sleep 0.1
			done
			kill -KILL "$pid" 2>/dev/null || true
		fi
		if [ -n "$pid" ]; then
			wait "$pid" 2>/dev/null || true
		fi
	done
	rm -f "$REDIRECTOR_LOG" "$CONSOLE_LOG"
	rm -rf "$TMP"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

fail() {
	printf 'smoke: %s\n' "$*" >&2
	if [ -s "$REDIRECTOR_LOG" ]; then
		printf '%s\n' '--- redirector log (last 20 lines) ---' >&2
		tail -n 20 "$REDIRECTOR_LOG" >&2
	fi
	if [ -s "$CONSOLE_LOG" ]; then
		printf '%s\n' '--- console log (last 20 lines) ---' >&2
		tail -n 20 "$CONSOLE_LOG" >&2
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

# seed_key 往测试库写入一把固定 API Key，并给它一个独立租户。
# 明文只留在这个脚本里。准备脚本会清掉租户表，所以每次冒烟都要重新种，
# 不能提前写进迁移。
seed_key() { # tenant-name key
	name=$1
	key=$2
	hash=$(printf '%s' "$key" | sha256sum | awk '{print $1}')
	psql_cmd "$TEST_DATABASE_URL" -v ON_ERROR_STOP=1 -q <<SQL || fail "写入冒烟 API Key 失败: $name"
INSERT INTO tenants (name)
SELECT '${name}'
WHERE NOT EXISTS (
    SELECT 1 FROM api_keys WHERE key_hash = decode('${hash}', 'hex')
);
INSERT INTO api_keys (tenant_id, key_hash)
SELECT t.id, decode('${hash}', 'hex')
FROM tenants t
WHERE t.name = '${name}'
  AND NOT EXISTS (SELECT 1 FROM api_keys WHERE key_hash = decode('${hash}', 'hex'))
ORDER BY t.id
LIMIT 1;
SQL
}

# request 用冒烟租户的密钥发请求，状态码写到 stdout，
# 响应体和响应头分别放进 $TMP/body、$TMP/headers。
request() { # base method path [json]
	request_as "$SMOKE_API_KEY" "$@"
}

# request_as 用指定密钥发请求，用来验证租户隔离。
request_as() { # key base method path [json]
	key=$1
	base=$2
	method=$3
	path=$4
	body=${5:-}
	if [ -n "$body" ]; then
		curl -sS --max-time 10 -o "$TMP/body" -D "$TMP/headers" -w '%{http_code}' \
			-X "$method" -H 'Content-Type: application/json' \
			-H "Authorization: Bearer $key" \
			-d "$body" "$base$path"
	else
		curl -sS --max-time 10 -o "$TMP/body" -D "$TMP/headers" -w '%{http_code}' \
			-X "$method" -H "Authorization: Bearer $key" "$base$path"
	fi
}

# request_redirect 发一次带 Referer/User-Agent 的跳转，用来验证点击元数据。
# 和上面两个 helper 共用输出文件、超时和 NO_PROXY 设置。
request_redirect() { # base path referer user-agent
	curl -sS --max-time 10 -o "$TMP/body" -D "$TMP/headers" -w '%{http_code}' \
		-H "Referer: $3" -A "$4" "$1$2"
}

header() { # name
	grep -i "^$1:" "$TMP/headers" | head -n 1 | sed 's/^[^:]*:[[:space:]]*//' | tr -d '\r'
}

random_code() {
	# 8 位 Base62，和服务的短码格式一致。
	LC_ALL=C tr -dc '0-9A-Za-z' </dev/urandom | head -c 8
}

# wait_ready 等一个进程的 healthz 就绪，最多 15 秒。进程提前退出时把日志带出来。
wait_ready() { # base pid name
	base=$1
	pid=$2
	name=$3
	i=0
	while ! curl -fsS --max-time 1 "$base/healthz" >/dev/null 2>&1; do
		if ! kill -0 "$pid" 2>/dev/null; then
			fail "$name 启动后立即退出"
		fi
		i=$((i + 1))
		[ "$i" -ge 150 ] && fail "$name 15 秒内没有就绪"
		sleep 0.1
	done
	# healthz 可能打到遗留的旧进程；确认本次启动的进程仍然活着。
	if ! kill -0 "$pid" 2>/dev/null; then
		fail "$name 启动后退出（端口可能被旧进程占用）"
	fi
}

[ -x bin/redirector ] || fail 'bin/redirector 不存在，先运行 make build'
[ -x bin/console ] || fail 'bin/console 不存在，先运行 make build'

# 只允许打在测试库和测试 Redis 上，避免把冒烟数据写进开发库。
[ -n "${TEST_DATABASE_URL:-}" ] || fail 'TEST_DATABASE_URL 未设置，冒烟只允许使用测试库'
[ -n "${TEST_REDIS_URL:-}" ] || fail 'TEST_REDIS_URL 未设置'
DATABASE_URL=$TEST_DATABASE_URL
REDIS_URL=$TEST_REDIS_URL
export DATABASE_URL REDIS_URL

redirector_addr=${SMOKE_REDIRECTOR_ADDR:-127.0.0.1:18080}
console_addr=${SMOKE_CONSOLE_ADDR:-127.0.0.1:18081}
REDIRECTOR_BASE=http://$redirector_addr
CONSOLE_BASE=http://$console_addr
export LOG_LEVEL=error
# redirector 没有盐会拒绝启动。CI 从 .env.example 生成 .env，本机可能还没配。
VISITOR_SALT=${VISITOR_SALT:-smoke-visitor-salt}
export VISITOR_SALT

# 64 位小写十六进制。只存在于测试库，和开发库无关。
SMOKE_API_KEY=slk_00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff
SMOKE_OTHER_KEY=slk_ffeeddccbbaa99887766554433221100ffeeddccbbaa99887766554433221100
seed_key smoke "$SMOKE_API_KEY"
seed_key smoke-other "$SMOKE_OTHER_KEY"

# 前缀赋值只影响这一条命令，盖过 .env 里的监听地址。
ADDR=$redirector_addr ./bin/redirector >"$REDIRECTOR_LOG" 2>&1 &
REDIRECTOR_PID=$!
CONSOLE_ADDR=$console_addr ./bin/console serve >"$CONSOLE_LOG" 2>&1 &
CONSOLE_PID=$!

wait_ready "$REDIRECTOR_BASE" "$REDIRECTOR_PID" redirector
wait_ready "$CONSOLE_BASE" "$CONSOLE_PID" console

echo 'smoke: healthz'
for base in "$REDIRECTOR_BASE" "$CONSOLE_BASE"; do
	status=$(request "$base" GET /healthz)
	assert_eq "$status" 200 "GET $base/healthz 状态码"
	assert_eq "$(cat "$TMP/body")" 'ok' "GET $base/healthz 响应体"
done

echo 'smoke: readyz'
for base in "$REDIRECTOR_BASE" "$CONSOLE_BASE"; do
	status=$(request "$base" GET /readyz)
	assert_eq "$status" 200 "GET $base/readyz 状态码"
	assert_eq "$(cat "$TMP/body")" 'ok' "GET $base/readyz 响应体"
done

echo 'smoke: unauthorized'
status=$(curl -sS --max-time 10 -o "$TMP/body" -w '%{http_code}' \
	-X POST -H 'Content-Type: application/json' \
	-d '{"url":"https://example.com/no-key"}' "$CONSOLE_BASE/api/v1/links")
assert_eq "$status" 401 '没有 API Key 的创建'
grep -q '"unauthorized"' "$TMP/body" || fail '401 响应体不是 unauthorized'

echo 'smoke: create'
url="https://example.com/smoke-$$-$(date +%s)"
status=$(request "$CONSOLE_BASE" POST /api/v1/links "{\"url\":\"$url\"}")
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
# 固定 User-Agent，后面两次 Chrome 跳转才能和这一次分成两个访客。
status=$(curl -sS --max-time 10 -o "$TMP/body" -D "$TMP/headers" -w '%{http_code}' \
	-A 'smoke-client-a' "$REDIRECTOR_BASE/r/$code")
assert_eq "$status" 302 'GET /r/{code} 状态码'
assert_eq "$(header Location)" "$url" '跳转 Location'

echo 'smoke: route isolation'
status=$(request "$CONSOLE_BASE" GET "/r/$code")
assert_eq "$status" 404 'console 不应提供跳转路由'
status=$(request "$REDIRECTOR_BASE" POST /api/v1/links '{"url":"https://example.com/isolated"}')
assert_eq "$status" 404 'redirector 不应提供管理路由'

echo 'smoke: head'
status=$(curl -sS --max-time 10 -I -o /dev/null -w '%{http_code}' "$REDIRECTOR_BASE/r/$code")
assert_eq "$status" 302 'HEAD /r/{code} 状态码'

echo 'smoke: not found'
status=$(request "$REDIRECTOR_BASE" GET "/r/$(random_code)")
assert_eq "$status" 404 '不存在的短码'
grep -q '"error"' "$TMP/body" || fail '404 响应体不是 JSON error'

echo 'smoke: invalid url'
status=$(request "$CONSOLE_BASE" POST /api/v1/links '{"url":"ftp://example.com"}')
assert_eq "$status" 400 '非法 URL'
grep -q '"error"' "$TMP/body" || fail '400 响应体不是 JSON error'

echo 'smoke: admin list and detail'
status=$(request "$CONSOLE_BASE" GET "/api/v1/links?limit=10")
assert_eq "$status" 200 'GET /api/v1/links 状态码'
grep -q "\"code\":\"$code\"" "$TMP/body" || fail '列表里没有刚创建的短码'
grep -q '"clicks_today"' "$TMP/body" || fail '列表项没有 clicks_today'
status=$(request "$CONSOLE_BASE" GET "/api/v1/links/$code")
assert_eq "$status" 200 'GET /api/v1/links/{code} 状态码'
grep -q "\"code\":\"$code\"" "$TMP/body" || fail '详情里没有 short code'
grep -q '"created_at"' "$TMP/body" || fail '详情里没有 created_at'
grep -q '"clicks"' "$TMP/body" || fail '详情里没有 clicks'

echo 'smoke: tenant isolation'
status=$(request_as "$SMOKE_OTHER_KEY" "$CONSOLE_BASE" GET "/api/v1/links/$code")
assert_eq "$status" 404 '其他租户查询详情'
status=$(request_as "$SMOKE_OTHER_KEY" "$CONSOLE_BASE" DELETE "/api/v1/links/$code")
assert_eq "$status" 404 '其他租户删除链接'
status=$(request "$CONSOLE_BASE" GET "/api/v1/links/$code")
assert_eq "$status" 200 '其他租户的删除不应影响链接'

echo 'smoke: update and cache write-through'
newurl="https://example.com/smoke-updated-$$-$(date +%s)"
status=$(request "$CONSOLE_BASE" PATCH "/api/v1/links/$code" "{\"url\":\"$newurl\"}")
assert_eq "$status" 200 'PATCH /api/v1/links/{code} 状态码'
grep -q "\"url\":\"$newurl\"" "$TMP/body" || fail 'PATCH 响应里没有新 URL'
status=$(request_redirect "$REDIRECTOR_BASE" "/r/$code" \
	'https://news.ycombinator.com/item?id=1' \
	'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0 Safari/537.36')
assert_eq "$status" 302 '修改后跳转状态码'
assert_eq "$(header Location)" "$newurl" '修改后跳转 Location（console 应写穿缓存）'

echo 'smoke: visitor dedup'
# 与上一次相同的 IP、UTC 日和 User-Agent，应合成同一个访客。
status=$(request_redirect "$REDIRECTOR_BASE" "/r/$code" \
	'https://news.ycombinator.com/item?id=1' \
	'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0 Safari/537.36')
assert_eq "$status" 302 '相同客户端再次跳转'

echo 'smoke: sorting'
for sort in id clicks clicks_today; do
	status=$(request "$CONSOLE_BASE" GET "/api/v1/links?limit=10&sort=$sort")
	assert_eq "$status" 200 "GET /api/v1/links?sort=$sort 状态码"
	grep -q "\"code\":\"$code\"" "$TMP/body" || fail "sort=$sort 的列表里没有短码"
done

echo 'smoke: stats'
# ClickRecorder 每 1 秒或满批写出，等这批点击落库再查统计。
sleep 2
status=$(request "$CONSOLE_BASE" GET "/api/v1/links/$code/stats")
assert_eq "$status" 200 'GET /api/v1/links/{code}/stats 状态码'
# 三次 GET：smoke-client-a 一次，Chrome 两次。同一 IP 和同一天里，Chrome 两次合成一个访客。
grep -E -q '"total_clicks":3[,}]' "$TMP/body" || fail "统计里 total_clicks 不是 3: $(cat "$TMP/body")"
grep -E -q '"unique_visitors":2[,}]' "$TMP/body" || fail "统计里 unique_visitors 不是 2: $(cat "$TMP/body")"
grep -q '"referrer":"news.ycombinator.com"' "$TMP/body" || fail '统计里没有 Referer host'
grep -q '"device":"desktop"' "$TMP/body" || fail '统计里没有设备类别'
grep -q '"client":"browser"' "$TMP/body" || fail '统计里没有客户端类别'
grep -q '"daily_visitors"' "$TMP/body" || fail '统计里没有每日访客'

echo 'smoke: delete'
status=$(request "$CONSOLE_BASE" DELETE "/api/v1/links/$code")
assert_eq "$status" 204 'DELETE /api/v1/links/{code} 状态码'
status=$(request "$REDIRECTOR_BASE" GET "/r/$code")
assert_eq "$status" 404 '删除后跳转'
status=$(request "$CONSOLE_BASE" GET "/api/v1/links/$code")
assert_eq "$status" 404 '删除后详情'
status=$(request_as "$SMOKE_OTHER_KEY" "$CONSOLE_BASE" GET "/api/v1/links/$code")
assert_eq "$status" 404 '删除后其他租户详情'

echo 'smoke: expiry'
# GNU date；CI 和 WSL2 都是 Linux。留 3 秒余量，再等 4 秒确保越过到期时刻。
expires_at=$(date -u -d '+3 seconds' '+%Y-%m-%dT%H:%M:%SZ')
status=$(request "$CONSOLE_BASE" POST /api/v1/links "{\"url\":\"https://example.com/expired\",\"expires_at\":\"$expires_at\"}")
assert_eq "$status" 201 '创建带过期时间的链接'
grep -q '"expires_at"' "$TMP/body" || fail '创建响应里没有 expires_at'
location=$(header Location)
expired_code=${location#/r/}
status=$(request "$REDIRECTOR_BASE" GET "/r/$expired_code")
assert_eq "$status" 302 '过期前跳转'
sleep 4
status=$(request "$REDIRECTOR_BASE" GET "/r/$expired_code")
assert_eq "$status" 404 '过期后不跳转'

echo 'smoke: quota'
# 把冒烟租户的配额压到本月已创建数，下一次创建应被拒绝。
# 配额按每月创建数计：删除不返还，跨月（UTC 自然月）才重新从 0 计。
psql_cmd "$TEST_DATABASE_URL" -v ON_ERROR_STOP=1 -q -c \
	"UPDATE tenants SET link_quota = link_usage WHERE id = (SELECT min(id) FROM tenants WHERE name = 'smoke')" \
	|| fail '设置冒烟租户配额失败'
status=$(request "$CONSOLE_BASE" POST /api/v1/links '{"url":"https://example.com/over-quota"}')
assert_eq "$status" 403 '超过本月链接配额'
grep -q '"link quota exceeded"' "$TMP/body" || fail "配额不足的响应体不对: $(cat "$TMP/body")"

status=$(request "$CONSOLE_BASE" DELETE "/api/v1/links/$expired_code")
assert_eq "$status" 204 '删除链接'
status=$(request "$CONSOLE_BASE" POST /api/v1/links '{"url":"https://example.com/after-delete"}')
assert_eq "$status" 403 '删除链接不返还配额'

# 把用量所属月份改成上个月，创建语句懒重置后应恢复额度。
psql_cmd "$TEST_DATABASE_URL" -v ON_ERROR_STOP=1 -q -c \
	"UPDATE tenants SET link_usage_month = (date_trunc('month', now() AT TIME ZONE 'UTC') - interval '1 month')::date WHERE id = (SELECT min(id) FROM tenants WHERE name = 'smoke')" \
	|| fail '把冒烟租户用量改到上个月失败'
status=$(request "$CONSOLE_BASE" POST /api/v1/links '{"url":"https://example.com/new-month"}')
assert_eq "$status" 201 '跨月后恢复链接配额'
psql_cmd "$TEST_DATABASE_URL" -v ON_ERROR_STOP=1 -q -c \
	"UPDATE tenants SET link_quota = 1000 WHERE id = (SELECT min(id) FROM tenants WHERE name = 'smoke')" \
	|| fail '恢复冒烟租户配额失败'

echo 'smoke: rate limit'
# 速率 1/s、突发 1：第一次放行，紧接着的第二次应 429 并带 Retry-After。
psql_cmd "$TEST_DATABASE_URL" -v ON_ERROR_STOP=1 -q -c \
	"UPDATE tenants SET rate_limit_per_second = 1 WHERE id = (SELECT min(id) FROM tenants WHERE name = 'smoke')" \
	|| fail '设置冒烟租户限流失败'
status=$(request "$CONSOLE_BASE" POST /api/v1/links '{"url":"https://example.com/rate-limit-1"}')
assert_eq "$status" 201 '限流前的第一次创建'
status=$(request "$CONSOLE_BASE" POST /api/v1/links '{"url":"https://example.com/rate-limit-2"}')
assert_eq "$status" 429 '超过速率限制'
grep -q '"rate limit exceeded"' "$TMP/body" || fail "限流的响应体不对: $(cat "$TMP/body")"
retry_after=$(header Retry-After)
case "$retry_after" in
	''|*[!0-9]*) fail "Retry-After 不是整数秒: '$retry_after'" ;;
esac
[ "$retry_after" -ge 1 ] || fail "Retry-After 小于 1: $retry_after"
psql_cmd "$TEST_DATABASE_URL" -v ON_ERROR_STOP=1 -q -c \
	"UPDATE tenants SET rate_limit_per_second = 10 WHERE id = (SELECT min(id) FROM tenants WHERE name = 'smoke')" \
	|| fail '恢复冒烟租户限流失败'
status=$(request "$CONSOLE_BASE" POST /api/v1/links '{"url":"https://example.com/rate-limit-restored"}')
assert_eq "$status" 201 '限流改回后立即生效'

echo 'smoke: metrics'
status=$(request "$REDIRECTOR_BASE" GET /metrics)
assert_eq "$status" 200 'redirector GET /metrics 状态码'
grep -q 'shortlink_http_requests_total' "$TMP/body" || fail 'redirector 指标里没有 shortlink_http_requests_total'
grep -q 'shortlink_cache_gets_total{result="hit"}' "$TMP/body" || fail 'redirector 没有缓存命中，创建可能没有写穿'
# 正常短码的首次 GET 必须命中 console 写穿的正缓存，不能留下成功的数据库查询。
# 否则 shortlink_cache_gets_total{hit} 可能只是 redirector 自己回源后写缓存造成的。
if grep -q 'shortlink_store_operations_total{operation="get",result="success"}' "$TMP/body"; then
	fail 'redirector 首次跳转回源了数据库，console 可能没有写穿缓存'
fi
grep -q 'shortlink_clicks_' "$TMP/body" || fail 'redirector 指标里没有点击指标'

status=$(request "$CONSOLE_BASE" GET /metrics)
assert_eq "$status" 200 'console GET /metrics 状态码'
grep -q 'shortlink_http_requests_total' "$TMP/body" || fail 'console 指标里没有 shortlink_http_requests_total'
grep -q 'shortlink_cache_writes_total{result="success"}' "$TMP/body" || fail 'console 没有成功写缓存，创建可能没有写穿'
grep -q 'shortlink_store_operations_total{operation="create",result="success"}' "$TMP/body" || fail 'console 指标里没有创建存储操作'
if grep -q 'shortlink_clicks_' "$TMP/body"; then
	fail 'console 不应启动 ClickRecorder'
fi

echo 'smoke: shutdown'
# 看门狗：服务卡在关停流程里时最多等 20 秒，避免冒烟把 CI 拖到 job 超时。
( sleep 20; kill -KILL "$REDIRECTOR_PID" "$CONSOLE_PID" 2>/dev/null ) &
WATCHDOG_PID=$!
# 进程可能在上一次请求后已经退出；这里失败不能触发 set -e，交给后面的 wait 报状态。
kill -TERM "$REDIRECTOR_PID" "$CONSOLE_PID" 2>/dev/null || true
redirector_status=0
console_status=0
wait "$REDIRECTOR_PID" || redirector_status=$?
wait "$CONSOLE_PID" || console_status=$?
kill "$WATCHDOG_PID" 2>/dev/null || true
wait "$WATCHDOG_PID" 2>/dev/null || true
WATCHDOG_PID=
REDIRECTOR_PID=
CONSOLE_PID=
assert_eq "$redirector_status" 0 'redirector SIGTERM 后的退出码'
assert_eq "$console_status" 0 'console SIGTERM 后的退出码'

echo 'smoke: ok'
