#!/bin/sh
# 冒烟测试：启动 redirector 和 console 两个真实进程，按 docs/api.md 的契约走一遍
# 健康检查、创建、跳转、HEAD、停用和启用、封禁和解封、404、非法参数、过期、路由隔离、
# 访客去重、配额、限流、Dex OIDC 登录（邀请兑现、/me、成员列表、会话创建、CSRF 与登出失效）、
# Webhook 订阅/HMAC 签名/500 重试/全生命周期投递/投递记录、异步邀请邮件与指标，最后检查 SIGTERM 优雅退出。
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
WORKER_LOG=$(mktemp)
TMP=$(mktemp -d)
REDIRECTOR_PID=
CONSOLE_PID=
WORKER_PID=
STUB_PID=
WEBHOOK_STUB_PID=
SMTP_STUB_PID=
SMS_STUB_PID=
OTLP_STUB_PID=
WATCHDOG_PID=

cleanup() {
	# 先停看门狗，避免它在脚本退出后拿旧 PID 去 kill。
	if [ -n "${WATCHDOG_PID:-}" ]; then
		kill "$WATCHDOG_PID" 2>/dev/null || true
		wait "$WATCHDOG_PID" 2>/dev/null || true
	fi
	for pid in "${REDIRECTOR_PID:-}" "${CONSOLE_PID:-}" "${WORKER_PID:-}" "${STUB_PID:-}" "${WEBHOOK_STUB_PID:-}" "${SMTP_STUB_PID:-}" "${SMS_STUB_PID:-}" "${OTLP_STUB_PID:-}"; do
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
	rm -f "$REDIRECTOR_LOG" "$CONSOLE_LOG" "$WORKER_LOG"
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
	if [ -s "$WORKER_LOG" ]; then
		printf '%s\n' '--- worker log (last 20 lines) ---' >&2
		tail -n 20 "$WORKER_LOG" >&2
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
INSERT INTO api_keys (tenant_id, key_hash, scopes)
SELECT t.id, decode('${hash}', 'hex'), ARRAY['links:read', 'links:write', 'stats:read']
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
CONSOLE_BIN=${CONSOLE_BIN:-./bin/console}
[ -x "$CONSOLE_BIN" ] || fail "$CONSOLE_BIN 不存在"

# 隔离宿主机 .env / config.yaml 中的覆盖项，仅保留数据库连接与测试 Redis。
unset SHORTLINK_CONFIG ADDR OPS_ADDR CONSOLE_ADDR CONSOLE_OPS_ADDR WORKER_ADDR \
	LOG_LEVEL LOG_FORMAT DRAIN_DELAY TRUSTED_PROXIES OTEL_EXPORTER_OTLP_ENDPOINT \
	FEATURE_ANALYTICS FEATURE_RISK_SCAN OIDC_CLIENT_SECRET GITHUB_CLIENT_SECRET \
	SMS_WEBHOOK_TOKEN WEBHOOK_SECRET_KEY SMTP_PASSWORD RISK_GSB_API_KEY RISK_LLM_API_KEY

# 只允许打在测试库和测试 Redis 上，避免把冒烟数据写进开发库。
[ -n "${TEST_DATABASE_URL:-}" ] || fail 'TEST_DATABASE_URL 未设置，冒烟只允许使用测试库'
[ -n "${TEST_REDIS_URL:-}" ] || fail 'TEST_REDIS_URL 未设置'
DATABASE_URL=$TEST_DATABASE_URL
REDIS_URL=$TEST_REDIS_URL
export DATABASE_URL REDIS_URL

redirector_addr=${SMOKE_REDIRECTOR_ADDR:-127.0.0.1:18080}
redirector_ops_addr=${SMOKE_REDIRECTOR_OPS_ADDR:-127.0.0.1:19090}
console_addr=${SMOKE_CONSOLE_ADDR:-127.0.0.1:18081}
console_ops_addr=${SMOKE_CONSOLE_OPS_ADDR:-127.0.0.1:19091}
worker_addr=${SMOKE_WORKER_ADDR:-127.0.0.1:18082}
risk_stub_port=${SMOKE_RISK_STUB_PORT:-18089}
webhook_stub_port=${SMOKE_WEBHOOK_STUB_PORT:-18090}
smtp_stub_port=${SMOKE_SMTP_STUB_PORT:-18091}
sms_stub_port=${SMOKE_SMS_STUB_PORT:-18092}
otlp_stub_port=${SMOKE_OTLP_STUB_PORT:-18093}
REDIRECTOR_BASE=http://$redirector_addr
REDIRECTOR_OPS_BASE=http://$redirector_ops_addr
CONSOLE_BASE=http://$console_addr
CONSOLE_OPS_BASE=http://$console_ops_addr
WORKER_BASE=http://$worker_addr
RISK_STUB_HOST=127.0.0.1:$risk_stub_port
WEBHOOK_STUB_HOST=127.0.0.1:$webhook_stub_port
SMS_STUB_HOST=127.0.0.1:$sms_stub_port
SMOKE_OTLP_ENDPOINT=http://127.0.0.1:$otlp_stub_port
SMOKE_TRACE_ID=4bf92f3577b34da6a3ce929d0e0e4736
SMOKE_TRACEPARENT="00-${SMOKE_TRACE_ID}-00f067aa0ba902b7-01"
SMOKE_SMS_TOKEN=smoke-sms-token
SMOKE_WEBHOOK_KEY=00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff
# redirector 没有盐会拒绝启动。CI 从 .env.example 生成 .env，本机可能还没配。
VISITOR_SALT=${VISITOR_SALT:-smoke-visitor-salt}
export VISITOR_SALT

# 冒烟只让 console 连接本地 Dex 测试提供方（回调由 console.public_url 自动推导）。
SMOKE_OIDC_ISSUER=${SMOKE_OIDC_ISSUER:-http://127.0.0.1:5556/dex}
SMOKE_OIDC_CLIENT_ID=${SMOKE_OIDC_CLIENT_ID:-shortlink-console}
SMOKE_OIDC_CLIENT_SECRET=${SMOKE_OIDC_CLIENT_SECRET:-shortlink-console-secret}
SMOKE_OIDC_USER_EMAIL=${SMOKE_OIDC_USER_EMAIL:-alice@example.com}
SMOKE_OIDC_USER_PASSWORD=${SMOKE_OIDC_USER_PASSWORD:-password}

curl -fsS --max-time 5 "$SMOKE_OIDC_ISSUER/.well-known/openid-configuration" >/dev/null 2>&1 \
	|| fail "OIDC 服务不可达: $SMOKE_OIDC_ISSUER（使用项目自带 compose 时先运行 make deps-up）"

# 64 位小写十六进制。只存在于测试库，和开发库无关。
SMOKE_API_KEY=slk_00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff
SMOKE_OTHER_KEY=slk_ffeeddccbbaa99887766554433221100ffeeddccbbaa99887766554433221100
seed_key smoke "$SMOKE_API_KEY"
seed_key smoke-other "$SMOKE_OTHER_KEY"
# 主体阶段一次发出几十个管理请求，默认的 10/s 桶会在中途 429。
# 先调高，最后的 rate limit 段再临时改成 1/s 验证限制本身，然后恢复到这个值。
psql_cmd "$TEST_DATABASE_URL" -v ON_ERROR_STOP=1 -q -c \
	"UPDATE tenants SET rate_limit_per_second = 100 WHERE id = (SELECT min(id) FROM tenants WHERE name = 'smoke')" \
	|| fail '调高冒烟租户限流失败'

# 为冒烟构造最小前端构建目录，顺带验证 console 的 WEB_DIST_DIR 托管与路由隔离。
mkdir -p "$TMP/webdist"
printf '%s\n' '<!doctype html><html><body>smoke-web-console</body></html>' >"$TMP/webdist/index.html"

# 本地 HTTP 短信网关桩：校验 Bearer 令牌并把收到的验证码 JSON 落盘供冒烟读取。
cat >"$TMP/sms_stub.py" <<'EOF'
import http.server
import json
import sys

port = int(sys.argv[1])
expected_token = sys.argv[2]
out_path = sys.argv[3]


class SMSHandler(http.server.BaseHTTPRequestHandler):
    def log_message(self, fmt, *args):
        pass

    def do_GET(self):
        if self.path == "/health":
            self.send_response(200)
            self.end_headers()
            self.wfile.write(b"ok")
            return
        self.send_response(404)
        self.end_headers()

    def do_POST(self):
        if self.path != "/send":
            self.send_response(404)
            self.end_headers()
            return
        if self.headers.get("Authorization") != f"Bearer {expected_token}":
            self.send_response(401)
            self.end_headers()
            return
        length = int(self.headers.get("Content-Length", "0"))
        body = self.rfile.read(length)
        payload = json.loads(body.decode("utf-8"))
        with open(out_path, "a", encoding="utf-8") as f:
            f.write(json.dumps(payload) + "\n")
            f.flush()
        self.send_response(200)
        self.end_headers()
        self.wfile.write(b'{"ok":true}')


class Server(http.server.ThreadingHTTPServer):
    allow_reuse_address = True


Server(("127.0.0.1", port), SMSHandler).serve_forever()
EOF

: >"$TMP/sms_messages.jsonl"
python3 "$TMP/sms_stub.py" "$sms_stub_port" "$SMOKE_SMS_TOKEN" "$TMP/sms_messages.jsonl" >"$TMP/sms_stub.log" 2>&1 &
SMS_STUB_PID=$!
i=0
while ! curl -fsS --max-time 1 "http://$SMS_STUB_HOST/health" >/dev/null 2>&1; do
	kill -0 "$SMS_STUB_PID" 2>/dev/null || fail '本地 SMS 接收桩启动失败'
	i=$((i + 1))
	[ "$i" -ge 50 ] && fail '本地 SMS 接收桩 5 秒内未就绪'
	sleep 0.1
done

# 本地 OTLP/HTTP 接收桩：记录三进程导出的路径、载荷长度及识别到的 service.name，供停机后校验。
cat >"$TMP/otlp_stub.py" <<'EOF'
import http.server
import json
import sys

port = int(sys.argv[1])
out_path = sys.argv[2]


class OTLPHandler(http.server.BaseHTTPRequestHandler):
    def log_message(self, fmt, *args):
        pass

    def do_GET(self):
        if self.path == "/health":
            self.send_response(200)
            self.end_headers()
            self.wfile.write(b"ok")
            return
        self.send_response(404)
        self.end_headers()

    def do_POST(self):
        length = int(self.headers.get("Content-Length", "0"))
        body = self.rfile.read(length)
        services = []
        if b"redirector" in body:
            services.append("redirector")
        if b"console-worker" in body:
            services.append("console-worker")
        if b"console" in body.replace(b"console-worker", b""):
            services.append("console")
        with open(out_path, "a", encoding="utf-8") as f:
            f.write(json.dumps({"path": self.path, "bytes": len(body), "services": services}) + "\n")
            f.flush()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.end_headers()
        self.wfile.write(b"{}")


class Server(http.server.ThreadingHTTPServer):
    allow_reuse_address = True


Server(("127.0.0.1", port), OTLPHandler).serve_forever()
EOF

: >"$TMP/otlp_requests.jsonl"
python3 "$TMP/otlp_stub.py" "$otlp_stub_port" "$TMP/otlp_requests.jsonl" >"$TMP/otlp_stub.log" 2>&1 &
OTLP_STUB_PID=$!
i=0
while ! curl -fsS --max-time 1 "http://127.0.0.1:$otlp_stub_port/health" >/dev/null 2>&1; do
	kill -0 "$OTLP_STUB_PID" 2>/dev/null || fail '本地 OTLP 接收桩启动失败'
	i=$((i + 1))
	[ "$i" -ge 50 ] && fail '本地 OTLP 接收桩 5 秒内未就绪'
	sleep 0.1
done

cat >"$TMP/smoke.yaml" <<EOF
features:
  analytics: true
  risk_scan: true
log:
  level: info
  format: json
tracing:
  endpoint: $SMOKE_OTLP_ENDPOINT
  sample_rate: 1.0
console:
  public_url: $CONSOLE_BASE
  web_dist_dir: $TMP/webdist
  auth:
    oidc:
      issuer: $SMOKE_OIDC_ISSUER
      client_id: $SMOKE_OIDC_CLIENT_ID
    phone_otp:
      provider: webhook
      webhook_url: http://$SMS_STUB_HOST/send
      cooldown: 60s
worker:
  poll_interval: 50ms
  risk:
    host_rate_limit: 100
    blocked_keywords:
      smoke-phish-marker: phishing
  webhook:
    host_rate_limit: 100
    retry_base_delay: 200ms
    retry_max_delay: 500ms
smtp:
  host: 127.0.0.1
  port: $smtp_stub_port
  from: noreply@shortlink.local
  tls_mode: none
EOF

echo 'smoke: config dump'
OIDC_CLIENT_SECRET=$SMOKE_OIDC_CLIENT_SECRET \
SMS_WEBHOOK_TOKEN=$SMOKE_SMS_TOKEN \
WEBHOOK_SECRET_KEY=$SMOKE_WEBHOOK_KEY \
	"$CONSOLE_BIN" config dump --config "$TMP/smoke.yaml" >"$TMP/config_dump.json" \
	|| fail 'console config dump 失败'
for secret in "$SMOKE_OIDC_CLIENT_SECRET" "$SMOKE_SMS_TOKEN" "$SMOKE_WEBHOOK_KEY"; do
	if grep -Fq "$secret" "$TMP/config_dump.json"; then
		fail "console config dump 泄露了敏感密钥"
	fi
done
grep -Fq '[REDACTED]' "$TMP/config_dump.json" || fail "console config dump 缺少脱敏标记: $(cat "$TMP/config_dump.json")"

export SHORTLINK_CONFIG="$TMP/smoke.yaml"

# 前缀赋值只影响这一条命令，覆盖监听地址与各服务密钥。
ADDR=$redirector_addr \
OPS_ADDR=$redirector_ops_addr \
	./bin/redirector --config "$TMP/smoke.yaml" >"$REDIRECTOR_LOG" 2>&1 &
REDIRECTOR_PID=$!
CONSOLE_ADDR=$console_addr \
CONSOLE_OPS_ADDR=$console_ops_addr \
OIDC_CLIENT_SECRET=$SMOKE_OIDC_CLIENT_SECRET \
SMS_WEBHOOK_TOKEN=$SMOKE_SMS_TOKEN \
WEBHOOK_SECRET_KEY=$SMOKE_WEBHOOK_KEY \
	"$CONSOLE_BIN" serve --config "$TMP/smoke.yaml" --allow-test-host "$WEBHOOK_STUB_HOST" --allow-test-host "$SMS_STUB_HOST" >"$CONSOLE_LOG" 2>&1 &
CONSOLE_PID=$!

wait_ready "$REDIRECTOR_OPS_BASE" "$REDIRECTOR_PID" redirector
wait_ready "$CONSOLE_OPS_BASE" "$CONSOLE_PID" console

echo 'smoke: healthz'
for base in "$REDIRECTOR_OPS_BASE" "$CONSOLE_OPS_BASE"; do
	status=$(request "$base" GET /healthz)
	assert_eq "$status" 200 "GET $base/healthz 状态码"
	assert_eq "$(cat "$TMP/body")" 'ok' "GET $base/healthz 响应体"
done

echo 'smoke: readyz'
for base in "$REDIRECTOR_OPS_BASE" "$CONSOLE_OPS_BASE"; do
	status=$(request "$base" GET /readyz)
	assert_eq "$status" 200 "GET $base/readyz 状态码"
	assert_eq "$(cat "$TMP/body")" 'ok' "GET $base/readyz 响应体"
done

echo 'smoke: ops isolation'
for base in "$REDIRECTOR_BASE" "$CONSOLE_BASE"; do
	for ep in /healthz /readyz /metrics; do
		status=$(request "$base" GET "$ep")
		assert_eq "$status" 404 "业务端口不应暴露 $base$ep"
	done
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
status=$(request "$CONSOLE_BASE" GET "/api/v1/does-not-exist")
assert_eq "$status" 404 'console 未知 /api/* 不应回退到 SPA'
status=$(request "$CONSOLE_BASE" GET "/")
assert_eq "$status" 200 'console WEB_DIST_DIR 首页'
grep -q 'smoke-web-console' "$TMP/body" || fail 'console 首页未返回 index.html'
status=$(request "$REDIRECTOR_BASE" POST /api/v1/links '{"url":"https://example.com/isolated"}')
assert_eq "$status" 404 'redirector 不应提供管理路由'

echo 'smoke: head'
status=$(curl -sS --max-time 10 -I -o /dev/null -w '%{http_code}' "$REDIRECTOR_BASE/r/$code")
assert_eq "$status" 302 'HEAD /r/{code} 状态码'

echo 'smoke: disable and enable'
status=$(request "$CONSOLE_BASE" PATCH "/api/v1/links/$code" '{"disabled":true}')
assert_eq "$status" 200 'PATCH disabled=true 状态码'
grep -q '"status":"disabled"' "$TMP/body" || fail '停用响应里的 status 不是 disabled'
grep -q '"disabled_at"' "$TMP/body" || fail '停用响应里没有 disabled_at'
status=$(request "$REDIRECTOR_BASE" GET "/r/$code")
assert_eq "$status" 404 '停用后跳转'
status=$(request "$CONSOLE_BASE" GET "/api/v1/links/$code")
assert_eq "$status" 200 '停用后详情'
grep -q '"status":"disabled"' "$TMP/body" || fail '停用后详情的 status 不是 disabled'
status=$(request "$CONSOLE_BASE" PATCH "/api/v1/links/$code" '{"disabled":false}')
assert_eq "$status" 200 'PATCH disabled=false 状态码'
grep -q '"status":"active"' "$TMP/body" || fail '启用后 status 不是 active'
if grep -q '"disabled_at"' "$TMP/body"; then
	fail '启用后响应里仍然有 disabled_at'
fi
# HEAD 同样走跳转路径，但不记点击，不影响后面按点击数的统计断言。
status=$(curl -sS --max-time 10 -I -o /dev/null -w '%{http_code}' "$REDIRECTOR_BASE/r/$code")
assert_eq "$status" 302 '启用后跳转'

echo 'smoke: ban and unban'
./bin/console link ban --code "$code" --reason phishing || fail 'console link ban 失败'
status=$(request "$REDIRECTOR_BASE" GET "/r/$code")
assert_eq "$status" 404 '封禁后跳转'
status=$(request "$CONSOLE_BASE" GET "/api/v1/links/$code")
assert_eq "$status" 200 '封禁后详情'
grep -q '"status":"banned"' "$TMP/body" || fail '封禁后详情的 status 不是 banned'
grep -q '"ban_reason":"phishing"' "$TMP/body" || fail '封禁后详情里没有 ban_reason'
status=$(request "$CONSOLE_BASE" PATCH "/api/v1/links/$code" '{"url":"https://example.com/banned"}')
assert_eq "$status" 403 '被封禁链接的 PATCH'
grep -q '"link is banned"' "$TMP/body" || fail '被封禁链接 PATCH 的响应体不是 link is banned'
status=$(request "$CONSOLE_BASE" DELETE "/api/v1/links/$code")
assert_eq "$status" 403 '被封禁链接的 DELETE'
./bin/console link unban --code "$code" || fail 'console link unban 失败'
# HEAD 不记点击，后面按点击数的统计断言仍然成立。
status=$(curl -sS --max-time 10 -I -o /dev/null -w '%{http_code}' "$REDIRECTOR_BASE/r/$code")
assert_eq "$status" 302 '解封后跳转'

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

echo 'smoke: api key scopes'
smoke_tenant_id=$(psql_cmd "$TEST_DATABASE_URL" -v ON_ERROR_STOP=1 -Atc \
	"SELECT min(id) FROM tenants WHERE name = 'smoke'") || fail '查询冒烟租户 ID 失败'
readonly_out=$(./bin/console key issue --tenant "$smoke_tenant_id" --scope links:read) || fail '签发只读 API Key 失败'
readonly_key=$(printf '%s\n' "$readonly_out" | awk '/^api_key: /{print $2}')
[ -n "$readonly_key" ] || fail "无法解析只读 API Key: $readonly_out"
printf '%s\n' "$readonly_out" | grep -q '^scopes: links:read$' || fail "只读 API Key scopes 输出不对: $readonly_out"
status=$(request_as "$readonly_key" "$CONSOLE_BASE" GET "/api/v1/links/$code")
assert_eq "$status" 200 'links:read 密钥查询详情'
status=$(request_as "$readonly_key" "$CONSOLE_BASE" POST /api/v1/links '{"url":"https://example.com/scoped-deny"}')
assert_eq "$status" 403 'links:read 密钥创建链接应被拒绝'
grep -q '"forbidden"' "$TMP/body" || fail "无权限响应体不对: $(cat "$TMP/body")"
status=$(request_as "$readonly_key" "$CONSOLE_BASE" GET "/api/v1/links/$code/stats")
assert_eq "$status" 403 'links:read 密钥查询统计应被拒绝'
write_out=$(./bin/console key issue --tenant "$smoke_tenant_id" --scope links:write) || fail '签发只写 API Key 失败'
write_key=$(printf '%s\n' "$write_out" | awk '/^api_key: /{print $2}')
[ -n "$write_key" ] || fail "无法解析只写 API Key: $write_out"
status=$(request_as "$write_key" "$CONSOLE_BASE" PATCH "/api/v1/links/$code" '{"disabled":false}')
assert_eq "$status" 204 'links:write 密钥修改不返回链接'
if [ -s "$TMP/body" ]; then
	fail "links:write 的 PATCH 不应返回响应体: $(cat "$TMP/body")"
fi
status=$(request_as "$write_key" "$CONSOLE_BASE" GET "/api/v1/links/$code")
assert_eq "$status" 403 'links:write 密钥查询详情应被拒绝'

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
	"UPDATE tenants SET rate_limit_per_second = 100 WHERE id = (SELECT min(id) FROM tenants WHERE name = 'smoke')" \
	|| fail '恢复冒烟租户限流失败'
status=$(request "$CONSOLE_BASE" POST /api/v1/links '{"url":"https://example.com/rate-limit-restored"}')
assert_eq "$status" 201 '限流改回后立即生效'

echo 'smoke: oidc login and session'
./bin/console member invite --tenant "$smoke_tenant_id" --email "$SMOKE_OIDC_USER_EMAIL" --role owner >/dev/null \
	|| fail 'console member invite 失败'

oidc_cookies=$TMP/oidc_cookies.txt
login_meta=$(curl -sS --max-time 10 -L \
	-c "$oidc_cookies" -b "$oidc_cookies" \
	-o "$TMP/dex_login.html" -w '%{http_code} %{url_effective}' \
	"$CONSOLE_BASE/api/v1/auth/oidc/login") || fail '访问 /api/v1/auth/oidc/login 失败'
dex_status=${login_meta%% *}
dex_login_url=${login_meta#* }
assert_eq "$dex_status" 200 'GET /api/v1/auth/oidc/login 跳转 Dex 登录页状态码'
case "$dex_login_url" in
	"$SMOKE_OIDC_ISSUER"/*) ;;
	*) fail "未跳转到 Dex 登录页: $dex_login_url" ;;
esac

status=$(curl -sS --max-time 10 -L \
	-c "$oidc_cookies" -b "$oidc_cookies" \
	-o "$TMP/body" -D "$TMP/headers" -w '%{http_code}' \
	--data-urlencode "login=$SMOKE_OIDC_USER_EMAIL" \
	--data-urlencode "password=$SMOKE_OIDC_USER_PASSWORD" \
	"$dex_login_url") || fail '提交 Dex 登录表单失败'
assert_eq "$status" 200 'OIDC callback 登录状态码'
grep -Fq "\"email\":\"$SMOKE_OIDC_USER_EMAIL\"" "$TMP/body" || fail "OIDC callback 响应体缺少邮箱: $(cat "$TMP/body")"
grep -q 'sl_session' "$oidc_cookies" || fail 'OIDC callback 未写入 sl_session Cookie'

status=$(curl -sS --max-time 10 -o "$TMP/body" -D "$TMP/headers" -w '%{http_code}' \
	-b "$oidc_cookies" "$CONSOLE_BASE/api/v1/me")
assert_eq "$status" 200 'GET /api/v1/me 状态码'
assert_eq "$(header Cache-Control)" 'no-store' 'GET /api/v1/me Cache-Control'
grep -Fq "\"email\":\"$SMOKE_OIDC_USER_EMAIL\"" "$TMP/body" || fail "GET /api/v1/me 缺少用户邮箱: $(cat "$TMP/body")"
grep -Fq "\"id\":$smoke_tenant_id,\"name\":\"smoke\",\"role\":\"owner\"" "$TMP/body" \
	|| fail "GET /api/v1/me 缺少已兑现组织成员关系: $(cat "$TMP/body")"

status=$(curl -sS --max-time 10 -o "$TMP/body" -D "$TMP/headers" -w '%{http_code}' \
	-H "X-Org-ID: $smoke_tenant_id" \
	-b "$oidc_cookies" "$CONSOLE_BASE/api/v1/members")
assert_eq "$status" 200 'GET /api/v1/members 状态码'
grep -Fq "\"email\":\"$SMOKE_OIDC_USER_EMAIL\"" "$TMP/body" || fail "成员列表缺少已登录用户: $(cat "$TMP/body")"
grep -q '"role":"owner"' "$TMP/body" || fail "成员列表缺少 owner 角色: $(cat "$TMP/body")"

oidc_url="https://example.com/smoke-oidc-$$-$(date +%s)"
status=$(curl -sS --max-time 10 -o "$TMP/body" -D "$TMP/headers" -w '%{http_code}' \
	-X POST -H 'Content-Type: application/json' \
	-H "X-Org-ID: $smoke_tenant_id" \
	-b "$oidc_cookies" \
	-d "{\"url\":\"$oidc_url\"}" \
	"$CONSOLE_BASE/api/v1/links")
assert_eq "$status" 201 '会话 Cookie 创建短链状态码'
oidc_location=$(header Location)
case "$oidc_location" in
	/r/*) ;;
	*) fail "会话创建短链 Location 格式不对: $oidc_location" ;;
esac
oidc_code=${oidc_location#/r/}
grep -q "\"code\":\"$oidc_code\"" "$TMP/body" || fail '会话创建短链响应体里没有 code'
status=$(curl -sS --max-time 10 -I -o /dev/null -w '%{http_code}' "$REDIRECTOR_BASE/r/$oidc_code")
assert_eq "$status" 302 '会话创建短链后 HEAD 跳转状态码'

status=$(curl -sS --max-time 10 -o "$TMP/body" -D "$TMP/headers" -w '%{http_code}' \
	-X POST -H 'Content-Type: application/json' \
	-H "X-Org-ID: $smoke_tenant_id" \
	-H 'Sec-Fetch-Site: cross-site' \
	-b "$oidc_cookies" \
	-d '{"url":"https://example.com/smoke-csrf"}' \
	"$CONSOLE_BASE/api/v1/links")
assert_eq "$status" 403 '跨站携带会话 Cookie 创建短链应被拒绝'
grep -q '"cross-origin request rejected"' "$TMP/body" || fail "跨站拒绝响应体不对: $(cat "$TMP/body")"

echo 'smoke: tenant self-service'
status=$(curl -sS --max-time 10 -o "$TMP/body" -D "$TMP/headers" -w '%{http_code}' \
	"$CONSOLE_BASE/api/v1/capabilities")
assert_eq "$status" 200 'GET /api/v1/capabilities 状态码'
grep -Fq '"id":"oidc","kind":"redirect","login_url":"/api/v1/auth/oidc/login"' "$TMP/body" || fail "capabilities 缺少 oidc 登录方式: $(cat "$TMP/body")"
grep -Fq '"analytics":true' "$TMP/body" || fail "capabilities 缺少 analytics=true: $(cat "$TMP/body")"
grep -Fq '"webhooks":true' "$TMP/body" || fail "capabilities 缺少 webhooks=true: $(cat "$TMP/body")"
grep -Fq '"risk_scan":true' "$TMP/body" || fail "capabilities 缺少 risk_scan=true: $(cat "$TMP/body")"
grep -Fq '"email":true' "$TMP/body" || fail "capabilities 缺少 email=true: $(cat "$TMP/body")"

status=$(curl -sS --max-time 10 -o "$TMP/body" -D "$TMP/headers" -w '%{http_code}' \
	-X POST -H 'Content-Type: application/json' \
	-b "$oidc_cookies" \
	-d '{"name":"Self Service Org"}' \
	"$CONSOLE_BASE/api/v1/orgs")
assert_eq "$status" 201 'POST /api/v1/orgs 自助建组织状态码'
self_org_id=$(sed -n 's/.*"id":\([0-9][0-9]*\).*/\1/p' "$TMP/body")
[ -n "$self_org_id" ] || fail "无法解析新建组织 ID: $(cat "$TMP/body")"
grep -q '"role":"owner"' "$TMP/body" || fail "新建组织响应缺少 owner 角色: $(cat "$TMP/body")"

status=$(curl -sS --max-time 10 -o "$TMP/body" -D "$TMP/headers" -w '%{http_code}' \
	-b "$oidc_cookies" "$CONSOLE_BASE/api/v1/me")
assert_eq "$status" 200 '建组织后 GET /api/v1/me 状态码'
grep -Fq "\"id\":$self_org_id,\"name\":\"Self Service Org\",\"role\":\"owner\"" "$TMP/body" \
	|| fail "GET /api/v1/me 缺少新建组织: $(cat "$TMP/body")"

status=$(curl -sS --max-time 10 -o "$TMP/body" -D "$TMP/headers" -w '%{http_code}' \
	-X POST -H 'Content-Type: application/json' \
	-H "X-Org-ID: $self_org_id" \
	-b "$oidc_cookies" \
	-d '{"email":"bob@example.com","role":"member"}' \
	"$CONSOLE_BASE/api/v1/invitations")
assert_eq "$status" 201 'POST /api/v1/invitations 发起邀请状态码'
self_inv_id=$(sed -n 's/.*"id":\([0-9][0-9]*\).*/\1/p' "$TMP/body")
[ -n "$self_inv_id" ] || fail "无法解析邀请 ID: $(cat "$TMP/body")"
status=$(curl -sS --max-time 10 -o "$TMP/body" -D "$TMP/headers" -w '%{http_code}' \
	-H "X-Org-ID: $self_org_id" \
	-b "$oidc_cookies" "$CONSOLE_BASE/api/v1/invitations")
assert_eq "$status" 200 'GET /api/v1/invitations 状态码'
grep -Fq '"email":"bob@example.com"' "$TMP/body" || fail "邀请列表缺少 bob@example.com: $(cat "$TMP/body")"
status=$(curl -sS --max-time 10 -o "$TMP/body" -D "$TMP/headers" -w '%{http_code}' \
	-X DELETE -H "X-Org-ID: $self_org_id" \
	-b "$oidc_cookies" "$CONSOLE_BASE/api/v1/invitations/$self_inv_id")
assert_eq "$status" 204 'DELETE /api/v1/invitations/{id} 撤销邀请状态码'

status=$(curl -sS --max-time 10 -o "$TMP/body" -D "$TMP/headers" -w '%{http_code}' \
	-X POST -H 'Content-Type: application/json' \
	-H "X-Org-ID: $self_org_id" \
	-b "$oidc_cookies" \
	-d '{"name":"smoke-ci-key","scopes":["links:read","links:write"]}' \
	"$CONSOLE_BASE/api/v1/api-keys")
assert_eq "$status" 201 'POST /api/v1/api-keys 签发组织 API Key 状态码'
assert_eq "$(header Cache-Control)" 'no-store' 'POST /api/v1/api-keys Cache-Control'
grep -Fq '"name":"smoke-ci-key"' "$TMP/body" || fail "签发响应缺少 name=smoke-ci-key: $(cat "$TMP/body")"
self_key_id=$(sed -n 's/.*"id":\([0-9][0-9]*\).*/\1/p' "$TMP/body")
self_api_key=$(sed -n 's/.*"api_key":"\(slk_[0-9a-f]\{64\}\)".*/\1/p' "$TMP/body")
[ -n "$self_key_id" ] && [ -n "$self_api_key" ] || fail "无法解析自助 API Key: $(cat "$TMP/body")"

status=$(curl -sS --max-time 10 -o "$TMP/body" -D "$TMP/headers" -w '%{http_code}' \
	-H "X-Org-ID: $self_org_id" \
	-b "$oidc_cookies" "$CONSOLE_BASE/api/v1/api-keys")
assert_eq "$status" 200 'GET /api/v1/api-keys 状态码'
grep -Fq "\"id\":$self_key_id" "$TMP/body" || fail "API Key 列表缺少新签发的 key: $(cat "$TMP/body")"
grep -Fq '"name":"smoke-ci-key"' "$TMP/body" || fail "API Key 列表缺少 name=smoke-ci-key: $(cat "$TMP/body")"

status=$(request_as "$self_api_key" "$CONSOLE_BASE" POST /api/v1/links '{"url":"https://example.com/self-service-key"}')
assert_eq "$status" 201 '自助签发的 API Key 创建短链状态码'

status=$(curl -sS --max-time 10 -o "$TMP/body" -D "$TMP/headers" -w '%{http_code}' \
	-H "X-Org-ID: $self_org_id" \
	-b "$oidc_cookies" "$CONSOLE_BASE/api/v1/api-keys")
assert_eq "$status" 200 '使用后 GET /api/v1/api-keys 状态码'
grep -Fq '"last_used_at":"' "$TMP/body" || fail "使用后 API Key 列表缺少 last_used_at: $(cat "$TMP/body")"

status=$(curl -sS --max-time 10 -o "$TMP/body" -D "$TMP/headers" -w '%{http_code}' \
	-X DELETE -H "X-Org-ID: $self_org_id" \
	-b "$oidc_cookies" "$CONSOLE_BASE/api/v1/api-keys/$self_key_id")
assert_eq "$status" 204 'DELETE /api/v1/api-keys/{id} 吊销 API Key 状态码'

status=$(request_as "$self_api_key" "$CONSOLE_BASE" GET /api/v1/links)
assert_eq "$status" 401 '吊销后的自助 API Key 应返回 401'

echo 'smoke: audit logs'
status=$(request "$CONSOLE_BASE" GET /api/v1/audit-logs)
assert_eq "$status" 403 'API Key 访问 /api/v1/audit-logs 应返回 403'

status=$(curl -sS --max-time 10 -o "$TMP/body" -D "$TMP/headers" -w '%{http_code}' \
	-H "X-Org-ID: $self_org_id" \
	-b "$oidc_cookies" "$CONSOLE_BASE/api/v1/audit-logs")
assert_eq "$status" 200 'GET /api/v1/audit-logs 状态码'
assert_eq "$(header Cache-Control)" 'no-store' 'GET /api/v1/audit-logs Cache-Control'
for act in org.create invitation.create invitation.revoke apikey.issue link.create apikey.revoke; do
	grep -Fq "\"action\":\"$act\"" "$TMP/body" || fail "自助组织审计日志缺少动作 $act: $(cat "$TMP/body")"
done

status=$(curl -sS --max-time 10 -o "$TMP/body" -D "$TMP/headers" -w '%{http_code}' \
	-H "X-Org-ID: $self_org_id" \
	-b "$oidc_cookies" "$CONSOLE_BASE/api/v1/audit-logs?action=apikey.issue")
assert_eq "$status" 200 'GET /api/v1/audit-logs?action=apikey.issue 状态码'
grep -Fq '"action":"apikey.issue"' "$TMP/body" || fail "action 筛选未返回 apikey.issue: $(cat "$TMP/body")"
if grep -Fq '"action":"org.create"' "$TMP/body"; then
	fail "action 筛选返回了其他动作: $(cat "$TMP/body")"
fi

status=$(curl -sS --max-time 10 -o "$TMP/body" -D "$TMP/headers" -w '%{http_code}' \
	-H "X-Org-ID: $smoke_tenant_id" \
	-b "$oidc_cookies" "$CONSOLE_BASE/api/v1/audit-logs?limit=50")
assert_eq "$status" 200 'smoke 租户 GET /api/v1/audit-logs 状态码'
for act in invitation.redeem link.ban link.unban; do
	grep -Fq "\"action\":\"$act\"" "$TMP/body" || fail "smoke 租户审计日志缺少动作 $act: $(cat "$TMP/body")"
done

# 验证带 ?return_to=/ 的登录回调返回 302 并落到 SPA 首页且写入 sl_session。
return_cookies=$TMP/return_to_cookies.txt
return_meta=$(curl -sS --max-time 10 -L \
	-c "$return_cookies" -b "$return_cookies" \
	-o "$TMP/dex_return.html" -w '%{http_code} %{url_effective}' \
	"$CONSOLE_BASE/api/v1/auth/oidc/login?return_to=/") || fail '访问带 return_to 的 oidc login 失败'
return_dex_url=${return_meta#* }
return_end=$(curl -sS --max-time 10 -L \
	-c "$return_cookies" -b "$return_cookies" \
	-o "$TMP/body" -w '%{http_code} %{url_effective}' \
	--data-urlencode "login=$SMOKE_OIDC_USER_EMAIL" \
	--data-urlencode "password=$SMOKE_OIDC_USER_PASSWORD" \
	"$return_dex_url") || fail '提交 return_to Dex 登录失败'
assert_eq "${return_end%% *}" 200 'return_to=/ 登录后落地首页状态码'
assert_eq "${return_end#* }" "$CONSOLE_BASE/" 'return_to=/ 登录后落地 URL'
grep -q 'smoke-web-console' "$TMP/body" || fail 'return_to=/ 落地页不是 SPA 首页'
grep -q 'sl_session' "$return_cookies" || fail 'return_to=/ 登录后未写入 sl_session'

# 保存登出前的 Cookie 再调登出，用旧 Cookie 重放才能证明服务端已吊销会话。
cp "$oidc_cookies" "$TMP/oidc_cookies_before_logout.txt"
status=$(curl -sS --max-time 10 -o "$TMP/body" -D "$TMP/headers" -w '%{http_code}' \
	-X POST -b "$oidc_cookies" -c "$oidc_cookies" \
	"$CONSOLE_BASE/api/v1/auth/logout")
assert_eq "$status" 204 'POST /api/v1/auth/logout 状态码'

status=$(curl -sS --max-time 10 -o "$TMP/body" -D "$TMP/headers" -w '%{http_code}' \
	-b "$TMP/oidc_cookies_before_logout.txt" "$CONSOLE_BASE/api/v1/me")
assert_eq "$status" 401 '登出后重放会话访问 /api/v1/me'
grep -q '"unauthorized"' "$TMP/body" || fail "登出后 /me 响应体不是 unauthorized: $(cat "$TMP/body")"

status=$(curl -sS --max-time 10 -o "$TMP/body" -D "$TMP/headers" -w '%{http_code}' \
	-X POST -H 'Content-Type: application/json' \
	-H "X-Org-ID: $smoke_tenant_id" \
	-b "$TMP/oidc_cookies_before_logout.txt" \
	-d '{"url":"https://example.com/after-logout"}' \
	"$CONSOLE_BASE/api/v1/links")
assert_eq "$status" 401 '登出后重放会话创建短链'
grep -q '"unauthorized"' "$TMP/body" || fail "登出后创建短链响应体不是 unauthorized: $(cat "$TMP/body")"

echo 'smoke: phone otp login and session'
status=$(curl -sS --max-time 10 -o "$TMP/body" -w '%{http_code}' "$CONSOLE_BASE/api/v1/capabilities")
assert_eq "$status" 200 'GET /api/v1/capabilities 状态码'
grep -Fq '"kind":"phone_otp"' "$TMP/body" || fail "capabilities 缺少 phone_otp 登录方式: $(cat "$TMP/body")"
grep -Fq '"send_code_url":"/api/v1/auth/phone/send-code"' "$TMP/body" || fail "capabilities 缺少 send_code_url: $(cat "$TMP/body")"
grep -Fq '"verify_code_url":"/api/v1/auth/phone/verify"' "$TMP/body" || fail "capabilities 缺少 verify_code_url: $(cat "$TMP/body")"

smoke_phone='+8613800138099'
status=$(curl -sS --max-time 10 -o "$TMP/body" -D "$TMP/headers" -w '%{http_code}' \
	-X POST -H 'Content-Type: application/json' \
	-d "{\"phone\":\"$smoke_phone\"}" \
	"$CONSOLE_BASE/api/v1/auth/phone/send-code")
assert_eq "$status" 200 'POST /api/v1/auth/phone/send-code 状态码'
assert_eq "$(header Cache-Control)" 'no-store' 'send-code Cache-Control'
grep -Fq '"cooldown_seconds":60' "$TMP/body" || fail "send-code 缺少 cooldown_seconds: $(cat "$TMP/body")"

# 冷却期内立即重发必须返回 429 并带 Retry-After。
status=$(curl -sS --max-time 10 -o "$TMP/body" -D "$TMP/headers" -w '%{http_code}' \
	-X POST -H 'Content-Type: application/json' \
	-d "{\"phone\":\"$smoke_phone\"}" \
	"$CONSOLE_BASE/api/v1/auth/phone/send-code")
assert_eq "$status" 429 '冷却期内重复发码应返回 429'
otp_retry_after=$(header Retry-After)
case "$otp_retry_after" in
	''|*[!0-9]*) fail "OTP 冷却 Retry-After 不是整数秒: '$otp_retry_after'" ;;
esac
[ "$otp_retry_after" -ge 1 ] || fail "OTP 冷却 Retry-After 小于 1: $otp_retry_after"

smoke_otp_code=$(python3 -c "
import json, sys
with open('$TMP/sms_messages.jsonl', 'r', encoding='utf-8') as f:
    rows = [json.loads(line) for line in f if line.strip()]
for r in reversed(rows):
    if r.get('to') == '$smoke_phone':
        print(r.get('code', ''))
        sys.exit(0)
sys.exit(1)
") || fail "本地 SMS 桩未收到 $smoke_phone 的验证码"
[ "${#smoke_otp_code}" -eq 6 ] || fail "SMS 桩收到的验证码格式非法: $smoke_otp_code"

wrong_otp='000000'
if [ "$smoke_otp_code" = "$wrong_otp" ]; then
	wrong_otp='999999'
fi
status=$(curl -sS --max-time 10 -o "$TMP/body" -w '%{http_code}' \
	-X POST -H 'Content-Type: application/json' \
	-d "{\"phone\":\"$smoke_phone\",\"code\":\"$wrong_otp\"}" \
	"$CONSOLE_BASE/api/v1/auth/phone/verify")
assert_eq "$status" 401 '错误验证码核验应返回 401'

phone_cookies=$TMP/phone_cookies.txt
status=$(curl -sS --max-time 10 -o "$TMP/body" -D "$TMP/headers" -w '%{http_code}' \
	-c "$phone_cookies" \
	-X POST -H 'Content-Type: application/json' \
	-d "{\"phone\":\"$smoke_phone\",\"code\":\"$smoke_otp_code\"}" \
	"$CONSOLE_BASE/api/v1/auth/phone/verify")
assert_eq "$status" 200 '正确验证码核验应返回 200'
assert_eq "$(header Cache-Control)" 'no-store' 'verify Cache-Control'
grep -q 'sl_session' "$phone_cookies" || fail '手机号登录成功后未写入 sl_session Cookie'
grep -Fq '"name":"+86****8099"' "$TMP/body" || fail "手机号登录用户展示名未脱敏: $(cat "$TMP/body")"

# 同一验证码不可重放。
status=$(curl -sS --max-time 10 -o "$TMP/body" -w '%{http_code}' \
	-X POST -H 'Content-Type: application/json' \
	-d "{\"phone\":\"$smoke_phone\",\"code\":\"$smoke_otp_code\"}" \
	"$CONSOLE_BASE/api/v1/auth/phone/verify")
assert_eq "$status" 401 '已消费验证码重放应返回 401'

status=$(curl -sS --max-time 10 -o "$TMP/body" -w '%{http_code}' \
	-b "$phone_cookies" "$CONSOLE_BASE/api/v1/me")
assert_eq "$status" 200 '手机号会话 GET /api/v1/me 状态码'
grep -Fq '"organizations":[]' "$TMP/body" || fail "首次手机号登录用户不应自动加入任何组织: $(cat "$TMP/body")"

status=$(curl -sS --max-time 10 -o "$TMP/body" -w '%{http_code}' \
	-X POST -H 'Content-Type: application/json' \
	-b "$phone_cookies" \
	-d '{"name":"phone-smoke-org"}' \
	"$CONSOLE_BASE/api/v1/orgs")
assert_eq "$status" 201 '手机号登录用户自助创建组织状态码'

status=$(curl -sS --max-time 10 -o "$TMP/body" -w '%{http_code}' \
	-X POST -b "$phone_cookies" -c "$phone_cookies" \
	"$CONSOLE_BASE/api/v1/auth/logout")
assert_eq "$status" 204 '手机号会话 POST /api/v1/auth/logout 状态码'

echo 'smoke: async risk scan'
# 先清理前面冒烟段写入的 example.com 待审任务，确保本次风控冒烟完全离线、只访问本地假站点。
psql_cmd "$TEST_DATABASE_URL" -v ON_ERROR_STOP=1 -q -c "DELETE FROM jobs" \
	|| fail '清理冒烟前置 jobs 失败'

mkdir -p "$TMP/stub"
printf '%s\n' '<html>safe landing page</html>' >"$TMP/stub/safe.html"
printf '%s\n' '<html>urgent smoke-phish-marker page</html>' >"$TMP/stub/phish.html"
python3 -m http.server "$risk_stub_port" --bind 127.0.0.1 --directory "$TMP/stub" >"$TMP/stub.log" 2>&1 &
STUB_PID=$!

i=0
while ! curl -fsS --max-time 1 "http://$RISK_STUB_HOST/safe.html" >/dev/null 2>&1; do
	kill -0 "$STUB_PID" 2>/dev/null || fail '本地风控假站点启动失败'
	i=$((i + 1))
	[ "$i" -ge 50 ] && fail '本地风控假站点 5 秒内未就绪'
	sleep 0.1
done

cat <<'EOF' >"$TMP/webhook_stub.py"
import hashlib
import hmac
import json
import sys
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

port = int(sys.argv[1])
secret_path = sys.argv[2]
events_path = sys.argv[3]
state_lock = threading.Lock()
seen_created = False


def verify_sig(secret, header, body):
    parts = {}
    for item in header.split(","):
        if "=" in item:
            k, v = item.strip().split("=", 1)
            parts[k.strip()] = v.strip()
    ts = parts.get("t", "")
    v1 = parts.get("v1", "")
    if not ts or not v1 or not secret:
        return False
    expected = hmac.new(secret.encode("utf-8"), f"{ts}.".encode("utf-8") + body, hashlib.sha256).hexdigest()
    return hmac.compare_digest(v1, expected)


class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path == "/health":
            self.send_response(200)
            self.end_headers()
            self.wfile.write(b"ok")
            return
        self.send_response(404)
        self.end_headers()

    def do_POST(self):
        global seen_created
        length = int(self.headers.get("Content-Length", "0"))
        body = self.rfile.read(length)
        try:
            with open(secret_path, "r", encoding="utf-8") as f:
                secret = f.read().strip()
        except FileNotFoundError:
            secret = ""

        sig_hdr = self.headers.get("X-Shortlink-Signature", "")
        delivery_id = self.headers.get("X-Shortlink-Delivery", "")
        event_hdr = self.headers.get("X-Shortlink-Event", "")

        if not verify_sig(secret, sig_hdr, body):
            status = 401
            self.send_response(status)
            self.end_headers()
            self.wfile.write(b"invalid signature")
            return

        env = json.loads(body.decode("utf-8"))
        ev = env.get("event", event_hdr)
        with state_lock:
            if ev == "link.created" and not seen_created:
                seen_created = True
                status = 500
            else:
                status = 200
            with open(events_path, "a", encoding="utf-8") as out:
                out.write(json.dumps({
                    "event": ev,
                    "event_id": env.get("id"),
                    "delivery_id": delivery_id,
                    "status": status,
                    "data": env.get("data"),
                }) + "\n")
                out.flush()

        self.send_response(status)
        self.end_headers()
        self.wfile.write(b"simulated error" if status == 500 else b"accepted")

    def log_message(self, format, *args):
        pass


ThreadingHTTPServer(("127.0.0.1", port), Handler).serve_forever()
EOF

cat <<'EOF' >"$TMP/smtp_stub.py"
import json
import socketserver
import sys
import threading

port = int(sys.argv[1])
mails_path = sys.argv[2]
write_lock = threading.Lock()


class SMTPHandler(socketserver.StreamRequestHandler):
    def send_line(self, line):
        self.wfile.write((line + "\r\n").encode("utf-8"))
        self.wfile.flush()

    def handle(self):
        self.send_line("220 smoke-smtp ESMTP ready")
        mail_from = ""
        rcpt_to = []
        while True:
            raw = self.rfile.readline()
            if not raw:
                break
            line = raw.decode("utf-8", errors="replace").rstrip("\r\n")
            upper = line.upper()
            if upper.startswith("EHLO ") or upper.startswith("HELO ") or upper in ("EHLO", "HELO"):
                self.send_line("250 OK")
            elif upper.startswith("MAIL FROM:"):
                mail_from = line[10:].strip()
                rcpt_to = []
                self.send_line("250 OK")
            elif upper.startswith("RCPT TO:"):
                rcpt_to.append(line[8:].strip())
                self.send_line("250 OK")
            elif upper == "DATA":
                self.send_line("354 End data with <CR><LF>.<CR><LF>")
                lines = []
                while True:
                    dline = self.rfile.readline()
                    if not dline:
                        break
                    decoded = dline.decode("utf-8", errors="replace").rstrip("\r\n")
                    if decoded == ".":
                        break
                    if decoded.startswith(".."):
                        decoded = decoded[1:]
                    lines.append(decoded)
                with write_lock:
                    with open(mails_path, "a", encoding="utf-8") as f:
                        f.write(json.dumps({
                            "mail_from": mail_from,
                            "rcpt_to": rcpt_to,
                            "data": "\n".join(lines),
                        }) + "\n")
                        f.flush()
                self.send_line("250 2.0.0 Queued")
            elif upper == "RSET":
                mail_from = ""
                rcpt_to = []
                self.send_line("250 OK")
            elif upper == "NOOP":
                self.send_line("250 OK")
            elif upper == "QUIT":
                self.send_line("221 Bye")
                break
            else:
                self.send_line("500 unrecognized command")


class Server(socketserver.ThreadingMixIn, socketserver.TCPServer):
    allow_reuse_address = True


Server(("127.0.0.1", port), SMTPHandler).serve_forever()
EOF

: >"$TMP/webhook_events.jsonl"
: >"$TMP/smtp_mails.jsonl"
python3 "$TMP/webhook_stub.py" "$webhook_stub_port" "$TMP/webhook_secret.txt" "$TMP/webhook_events.jsonl" >"$TMP/webhook_stub.log" 2>&1 &
WEBHOOK_STUB_PID=$!
python3 "$TMP/smtp_stub.py" "$smtp_stub_port" "$TMP/smtp_mails.jsonl" >"$TMP/smtp_stub.log" 2>&1 &
SMTP_STUB_PID=$!

i=0
while ! curl -fsS --max-time 1 "http://$WEBHOOK_STUB_HOST/health" >/dev/null 2>&1; do
	kill -0 "$WEBHOOK_STUB_PID" 2>/dev/null || fail '本地 Webhook 接收桩启动失败'
	i=$((i + 1))
	[ "$i" -ge 50 ] && fail '本地 Webhook 接收桩 5 秒内未就绪'
	sleep 0.1
done

i=0
while ! python3 -c "import socket; socket.create_connection(('127.0.0.1', int('$smtp_stub_port')), timeout=1).close()" >/dev/null 2>&1; do
	kill -0 "$SMTP_STUB_PID" 2>/dev/null || fail '本地 SMTP 接收桩启动失败'
	i=$((i + 1))
	[ "$i" -ge 50 ] && fail '本地 SMTP 接收桩 5 秒内未就绪'
	sleep 0.1
done

WORKER_ADDR=$worker_addr \
WEBHOOK_SECRET_KEY=$SMOKE_WEBHOOK_KEY \
	"$CONSOLE_BIN" worker --config "$TMP/smoke.yaml" --allow-test-host "$RISK_STUB_HOST" --allow-test-host "$WEBHOOK_STUB_HOST" >"$WORKER_LOG" 2>&1 &
WORKER_PID=$!
wait_ready "$WORKER_BASE" "$WORKER_PID" worker

status=$(request "$WORKER_BASE" GET /readyz)
assert_eq "$status" 200 'worker GET /readyz 状态码'
assert_eq "$(cat "$TMP/body")" 'ok' 'worker GET /readyz 响应体'

phish_url="http://$RISK_STUB_HOST/phish.html"
status=$(curl -sS --max-time 10 -o "$TMP/body" -D "$TMP/headers" -w '%{http_code}' \
	-X POST -H 'Content-Type: application/json' \
	-H "Authorization: Bearer $SMOKE_API_KEY" \
	-H "traceparent: $SMOKE_TRACEPARENT" \
	-d "{\"url\":\"$phish_url\"}" "$CONSOLE_BASE/api/v1/links")
assert_eq "$status" 201 '创建待风控短链状态码'
risk_location=$(header Location)
risk_code=${risk_location#/r/}

i=0
while :; do
	status=$(request "$CONSOLE_BASE" GET "/api/v1/links/$risk_code")
	assert_eq "$status" 200 '轮询风控短链详情状态码'
	if grep -q '"status":"banned"' "$TMP/body"; then
		break
	fi
	i=$((i + 1))
	[ "$i" -ge 100 ] && fail "异步风控 10 秒内未自动封禁: $(cat "$TMP/body")"
	sleep 0.1
done
grep -q '"ban_reason":"phishing"' "$TMP/body" || fail "风控自动封禁缺少 ban_reason=phishing: $(cat "$TMP/body")"

status=$(curl -sS --max-time 10 -o "$TMP/body" -D "$TMP/headers" -w '%{http_code}' \
	-H "traceparent: $SMOKE_TRACEPARENT" "$REDIRECTOR_BASE/r/$risk_code")
assert_eq "$status" 404 '异步风控封禁后跳转应返回 404'

echo 'smoke: tracing correlation (logs + jobs.traceparent)'
job_tp=$(psql_cmd "$TEST_DATABASE_URL" -Atc "SELECT traceparent FROM jobs WHERE kind = 'link.risk_scan' AND payload->>'code' = '$risk_code' ORDER BY id DESC LIMIT 1")
case "$job_tp" in
	00-"$SMOKE_TRACE_ID"-*-01) ;;
	*) fail "jobs.traceparent 未持久化入队 trace_id=$SMOKE_TRACE_ID: got '$job_tp'" ;;
esac
python3 -c "
import json, sys

def read_entries(path):
    with open(path, 'r', encoding='utf-8') as f:
        return [json.loads(line) for line in f if line.strip()]

console_ok = any(e.get('trace_id') == '$SMOKE_TRACE_ID' and e.get('span_id') for e in read_entries('$CONSOLE_LOG'))
redir_ok = any(e.get('trace_id') == '$SMOKE_TRACE_ID' and e.get('span_id') for e in read_entries('$REDIRECTOR_LOG'))
worker_ok = any(
    e.get('msg') == 'risk scan banned link' and e.get('code') == '$risk_code'
    and len(e.get('trace_id', '')) == 32 and len(e.get('span_id', '')) == 16
    for e in read_entries('$WORKER_LOG')
)
if not (console_ok and redir_ok and worker_ok):
    print(f'console_ok={console_ok} redir_ok={redir_ok} worker_ok={worker_ok}', file=sys.stderr)
    sys.exit(1)
" || fail '日志 Phase C trace_id/span_id 跨进程关联校验失败'

status=$(request "$CONSOLE_BASE" GET '/api/v1/links?status=banned')
assert_eq "$status" 200 'GET /api/v1/links?status=banned 状态码'
grep -Fq "\"code\":\"$risk_code\"" "$TMP/body" || fail "status=banned 列表缺少风控封禁短码 $risk_code: $(cat "$TMP/body")"
if grep -Fq '"status":"active"' "$TMP/body"; then
	fail "status=banned 列表返回了 active 短链: $(cat "$TMP/body")"
fi
status=$(request "$CONSOLE_BASE" GET '/api/v1/links?q=phish.html')
assert_eq "$status" 200 'GET /api/v1/links?q=phish.html 状态码'
grep -Fq "\"code\":\"$risk_code\"" "$TMP/body" || fail "q=phish.html 列表缺少目标短码 $risk_code: $(cat "$TMP/body")"
status=$(request "$CONSOLE_BASE" GET '/api/v1/links?status=invalid')
assert_eq "$status" 400 '非法 status 筛选应返回 400'

status=$(curl -sS --max-time 10 -o "$TMP/body" -D "$TMP/headers" -w '%{http_code}' \
	-H "X-Org-ID: $smoke_tenant_id" \
	-b "$return_cookies" "$CONSOLE_BASE/api/v1/audit-logs?action=link.ban")
assert_eq "$status" 200 '异步风控封禁后 GET /api/v1/audit-logs 状态码'
grep -Fq '"actor_kind":"system"' "$TMP/body" || fail "异步风控审计日志缺少 actor_kind=system: $(cat "$TMP/body")"
grep -Fq "\"target_id\":\"$risk_code\"" "$TMP/body" || fail "异步风控审计日志缺少 target_id=$risk_code: $(cat "$TMP/body")"
grep -Fq '"request_id":"job:' "$TMP/body" || fail "异步风控审计日志缺少 job request_id: $(cat "$TMP/body")"

echo 'smoke: webhooks and async invitation email'
status=$(request "$CONSOLE_BASE" GET /api/v1/webhooks)
assert_eq "$status" 403 'API Key 访问 /api/v1/webhooks 应返回 403'

status=$(curl -sS --max-time 10 -o "$TMP/body" -D "$TMP/headers" -w '%{http_code}' \
	-X POST -H 'Content-Type: application/json' \
	-H "X-Org-ID: $smoke_tenant_id" \
	-b "$return_cookies" \
	-d "{\"url\":\"http://$WEBHOOK_STUB_HOST/hook\",\"description\":\"smoke hook\",\"events\":[\"link.created\",\"link.updated\",\"link.deleted\",\"link.banned\",\"link.unbanned\"]}" \
	"$CONSOLE_BASE/api/v1/webhooks")
assert_eq "$status" 201 'POST /api/v1/webhooks 状态码'
assert_eq "$(header Cache-Control)" 'no-store' 'POST /api/v1/webhooks Cache-Control'
wh_id=$(sed -n 's/.*"id":\([0-9][0-9]*\).*/\1/p' "$TMP/body")
wh_secret=$(sed -n 's/.*"secret":"\(whsec_[0-9a-f]\{64\}\)".*/\1/p' "$TMP/body")
[ -n "$wh_id" ] && [ -n "$wh_secret" ] || fail "无法解析 Webhook ID 或 secret: $(cat "$TMP/body")"
printf '%s' "$wh_secret" >"$TMP/webhook_secret.txt"

status=$(curl -sS --max-time 10 -o "$TMP/body" -D "$TMP/headers" -w '%{http_code}' \
	-X POST -H "X-Org-ID: $smoke_tenant_id" \
	-b "$return_cookies" \
	"$CONSOLE_BASE/api/v1/webhooks/$wh_id/test")
assert_eq "$status" 202 'POST /api/v1/webhooks/{id}/test 状态码'
ping_delivery_id=$(sed -n 's/.*"delivery_id":"\(dlv_[0-9][0-9]*\)".*/\1/p' "$TMP/body")
[ -n "$ping_delivery_id" ] || fail "无法解析 test ping delivery_id: $(cat "$TMP/body")"

i=0
while ! python3 -c "
import json, sys
with open('$TMP/webhook_events.jsonl', 'r', encoding='utf-8') as f:
    rows = [json.loads(line) for line in f if line.strip()]
sys.exit(0 if any(r['event'] == 'webhook.ping' and r['status'] == 200 and r['delivery_id'] == '$ping_delivery_id' for r in rows) else 1)
" >/dev/null 2>&1; do
	i=$((i + 1))
	[ "$i" -ge 100 ] && fail "webhook.ping 10 秒内未成功送达: $(cat "$TMP/webhook_events.jsonl")"
	sleep 0.1
done

wh_safe_url="http://$RISK_STUB_HOST/safe.html"
status=$(request "$CONSOLE_BASE" POST /api/v1/links "{\"url\":\"$wh_safe_url\"}")
assert_eq "$status" 201 '创建触发 Webhook 的短链状态码'
wh_location=$(header Location)
wh_code=${wh_location#/r/}

i=0
while ! python3 -c "
import json, sys
with open('$TMP/webhook_events.jsonl', 'r', encoding='utf-8') as f:
    rows = [json.loads(line) for line in f if line.strip()]
by_del = {}
for r in rows:
    if r['event'] == 'link.created' and isinstance(r.get('data'), dict) and r['data'].get('code') == '$wh_code':
        by_del.setdefault(r['delivery_id'], []).append(r['status'])
sys.exit(0 if any(500 in statuses and 200 in statuses for statuses in by_del.values()) else 1)
" >/dev/null 2>&1; do
	i=$((i + 1))
	[ "$i" -ge 100 ] && fail "link.created Webhook 500->200 同 DeliveryID 重试 10 秒内未完成: $(cat "$TMP/webhook_events.jsonl")"
	sleep 0.1
done

status=$(request "$CONSOLE_BASE" PATCH "/api/v1/links/$wh_code" "{\"url\":\"$wh_safe_url?v=2\"}")
assert_eq "$status" 200 'Webhook 生命周期 PATCH 短链状态码'
WEBHOOK_SECRET_KEY=$SMOKE_WEBHOOK_KEY ./bin/console link ban --code "$wh_code" --reason spam || fail 'Webhook 生命周期 link ban 失败'
WEBHOOK_SECRET_KEY=$SMOKE_WEBHOOK_KEY ./bin/console link unban --code "$wh_code" || fail 'Webhook 生命周期 link unban 失败'
status=$(request "$CONSOLE_BASE" DELETE "/api/v1/links/$wh_code")
assert_eq "$status" 204 'Webhook 生命周期 DELETE 短链状态码'

i=0
while ! python3 -c "
import json, sys
with open('$TMP/webhook_events.jsonl', 'r', encoding='utf-8') as f:
    rows = [json.loads(line) for line in f if line.strip()]
got = {r['event'] for r in rows if r['status'] == 200 and isinstance(r.get('data'), dict) and r['data'].get('code') == '$wh_code'}
want = {'link.created', 'link.updated', 'link.banned', 'link.unbanned', 'link.deleted'}
sys.exit(0 if want.issubset(got) else 1)
" >/dev/null 2>&1; do
	i=$((i + 1))
	[ "$i" -ge 100 ] && fail "link 完整生命周期 Webhook 事件 10 秒内未全部送达: $(cat "$TMP/webhook_events.jsonl")"
	sleep 0.1
done

status=$(curl -sS --max-time 10 -o "$TMP/body" -D "$TMP/headers" -w '%{http_code}' \
	-H "X-Org-ID: $smoke_tenant_id" \
	-b "$return_cookies" \
	"$CONSOLE_BASE/api/v1/webhooks/$wh_id/deliveries?limit=20")
assert_eq "$status" 200 'GET /api/v1/webhooks/{id}/deliveries 状态码'
assert_eq "$(header Cache-Control)" 'no-store' 'GET /api/v1/webhooks/{id}/deliveries Cache-Control'
for token in '"status":"retry"' '"status":"success"' '"event":"webhook.ping"' '"event":"link.created"' '"event":"link.deleted"'; do
	grep -Fq "$token" "$TMP/body" || fail "deliveries 缺少预期字段 $token: $(cat "$TMP/body")"
done

status=$(curl -sS --max-time 10 -o "$TMP/body" -D "$TMP/headers" -w '%{http_code}' \
	-X POST -H 'Content-Type: application/json' \
	-H "X-Org-ID: $smoke_tenant_id" \
	-b "$return_cookies" \
	-d '{"email":"invitee-smoke@example.com","role":"member"}' \
	"$CONSOLE_BASE/api/v1/invitations")
assert_eq "$status" 201 '创建触发异步邮件的邀请状态码'

i=0
while ! python3 -c "
import json, sys
with open('$TMP/smtp_mails.jsonl', 'r', encoding='utf-8') as f:
    rows = [json.loads(line) for line in f if line.strip()]
ok = any(
    any('invitee-smoke@example.com' in rcpt for rcpt in r.get('rcpt_to', []))
    and 'smoke' in r.get('data', '')
    and '$CONSOLE_BASE/login' in r.get('data', '')
    for r in rows
)
sys.exit(0 if ok else 1)
" >/dev/null 2>&1; do
	i=$((i + 1))
	[ "$i" -ge 100 ] && fail "异步邀请邮件 10 秒内未送达本地 SMTP 桩: $(cat "$TMP/smtp_mails.jsonl")"
	sleep 0.1
done

echo 'smoke: metrics'
status=$(request "$REDIRECTOR_OPS_BASE" GET /metrics)
assert_eq "$status" 200 'redirector GET /metrics 状态码'
grep -q 'shortlink_http_requests_total' "$TMP/body" || fail 'redirector 指标里没有 shortlink_http_requests_total'
grep -q 'shortlink_cache_gets_total{result="hit"}' "$TMP/body" || fail 'redirector 没有缓存命中，创建可能没有写穿'
# 正常短码的首次 GET 必须命中 console 写穿的正缓存，不能留下成功的数据库查询。
# 否则 shortlink_cache_gets_total{hit} 可能只是 redirector 自己回源后写缓存造成的。
if grep -q 'shortlink_store_operations_total{operation="get",result="success"}' "$TMP/body"; then
	fail 'redirector 首次跳转回源了数据库，console 可能没有写穿缓存'
fi
grep -q 'shortlink_clicks_' "$TMP/body" || fail 'redirector 指标里没有点击指标'

status=$(request "$CONSOLE_OPS_BASE" GET /metrics)
assert_eq "$status" 200 'console GET /metrics 状态码'
grep -q 'shortlink_http_requests_total' "$TMP/body" || fail 'console 指标里没有 shortlink_http_requests_total'
grep -q 'shortlink_cache_writes_total{result="success"}' "$TMP/body" || fail 'console 没有成功写缓存，创建可能没有写穿'
grep -q 'shortlink_store_operations_total{operation="create",result="success"}' "$TMP/body" || fail 'console 指标里没有创建存储操作'
if grep -q 'shortlink_clicks_' "$TMP/body"; then
	fail 'console 不应启动 ClickRecorder'
fi

status=$(request "$WORKER_BASE" GET /metrics)
assert_eq "$status" 200 'worker GET /metrics 状态码'
grep -q 'shortlink_jobs_processed_total{kind="link.risk_scan",result="done"}' "$TMP/body" || fail 'worker 指标缺少已完成风控任务计数'
grep -q 'shortlink_jobs_processed_total{kind="webhook.deliver",result="done"}' "$TMP/body" || fail 'worker 指标缺少已完成 Webhook 投递任务计数'
grep -q 'shortlink_jobs_processed_total{kind="invitation.email",result="done"}' "$TMP/body" || fail 'worker 指标缺少已完成邀请邮件任务计数'
grep -q 'shortlink_webhook_deliveries_total{event="link.created",result="retry"}' "$TMP/body" || fail 'worker 指标缺少 Webhook 重试计数'
grep -q 'shortlink_webhook_deliveries_total{event="link.created",result="success"}' "$TMP/body" || fail 'worker 指标缺少 Webhook 成功计数'
grep -q 'shortlink_jobs_oldest_pending_age_seconds{kind="link.risk_scan"}' "$TMP/body" || fail 'worker 指标缺少最老待执行任务时长'

echo 'smoke: shutdown'
# 看门狗：服务卡在关停流程里时最多等 20 秒，避免冒烟把 CI 拖到 job 超时。
( sleep 20; kill -KILL "$REDIRECTOR_PID" "$CONSOLE_PID" "$WORKER_PID" 2>/dev/null ) &
WATCHDOG_PID=$!
# 进程可能在上一次请求后已经退出；这里失败不能触发 set -e，交给后面的 wait 报状态。
kill -TERM "$REDIRECTOR_PID" "$CONSOLE_PID" "$WORKER_PID" 2>/dev/null || true
redirector_status=0
console_status=0
worker_status=0
wait "$REDIRECTOR_PID" || redirector_status=$?
wait "$CONSOLE_PID" || console_status=$?
wait "$WORKER_PID" || worker_status=$?
kill "$WATCHDOG_PID" 2>/dev/null || true
wait "$WATCHDOG_PID" 2>/dev/null || true
WATCHDOG_PID=
REDIRECTOR_PID=
CONSOLE_PID=
WORKER_PID=
assert_eq "$redirector_status" 0 'redirector SIGTERM 后的退出码'
assert_eq "$console_status" 0 'console SIGTERM 后的退出码'
assert_eq "$worker_status" 0 'worker SIGTERM 后的退出码'

python3 -c "
import json, sys
with open('$TMP/otlp_requests.jsonl', 'r', encoding='utf-8') as f:
    rows = [json.loads(line) for line in f if line.strip()]
seen = set()
for r in rows:
    if r.get('path') == '/v1/traces' and r.get('bytes', 0) > 0:
        seen.update(r.get('services', []))
want = {'redirector', 'console', 'console-worker'}
if not want.issubset(seen):
    print(f'seen={seen}, want={want}', file=sys.stderr)
    sys.exit(1)
" || fail "停机后 OTLP 接收桩未收到全部三个服务的 traces: $(cat "$TMP/otlp_requests.jsonl")"

echo 'smoke: minimal single-pg mode (no redis, no analytics, no risk scan)'
psql_cmd "$TEST_DATABASE_URL" -v ON_ERROR_STOP=1 -q -c "DELETE FROM jobs" \
	|| fail '清理极简模式前置 jobs 失败'

SHORTLINK_CONFIG= \
OTEL_EXPORTER_OTLP_ENDPOINT= \
REDIS_URL= \
VISITOR_SALT= \
FEATURE_ANALYTICS=false \
FEATURE_RISK_SCAN=false \
ADDR=$redirector_addr \
OPS_ADDR=$redirector_ops_addr \
	./bin/redirector >"$REDIRECTOR_LOG" 2>&1 &
REDIRECTOR_PID=$!

SHORTLINK_CONFIG= \
OTEL_EXPORTER_OTLP_ENDPOINT= \
REDIS_URL= \
FEATURE_ANALYTICS=false \
FEATURE_RISK_SCAN=false \
OIDC_CLIENT_SECRET= \
GITHUB_CLIENT_SECRET= \
SMS_WEBHOOK_TOKEN= \
WEBHOOK_SECRET_KEY= \
SMTP_PASSWORD= \
RISK_GSB_API_KEY= \
RISK_LLM_API_KEY= \
CONSOLE_ADDR=$console_addr \
CONSOLE_OPS_ADDR=$console_ops_addr \
	./bin/console serve >"$CONSOLE_LOG" 2>&1 &
CONSOLE_PID=$!

wait_ready "$REDIRECTOR_OPS_BASE" "$REDIRECTOR_PID" redirector-minimal
wait_ready "$CONSOLE_OPS_BASE" "$CONSOLE_PID" console-minimal

status=$(curl -sS --max-time 10 -o "$TMP/body" -w '%{http_code}' "$CONSOLE_BASE/api/v1/capabilities")
assert_eq "$status" 200 '极简模式 GET /api/v1/capabilities 状态码'
grep -Fq '"auth_methods":[]' "$TMP/body" || fail "极简模式 auth_methods 应为空: $(cat "$TMP/body")"
grep -Fq '"analytics":false' "$TMP/body" || fail "极简模式 analytics 应为 false: $(cat "$TMP/body")"
grep -Fq '"webhooks":false' "$TMP/body" || fail "极简模式 webhooks 应为 false: $(cat "$TMP/body")"
grep -Fq '"risk_scan":false' "$TMP/body" || fail "极简模式 risk_scan 应为 false: $(cat "$TMP/body")"
grep -Fq '"email":false' "$TMP/body" || fail "极简模式 email 应为 false: $(cat "$TMP/body")"

minimal_url="https://example.com/smoke-minimal-$$"
status=$(request "$CONSOLE_BASE" POST /api/v1/links "{\"url\":\"$minimal_url\"}")
assert_eq "$status" 201 '极简模式创建短链状态码'
if grep -q '"clicks"' "$TMP/body"; then
	fail "极简模式创建响应不应包含 clicks 字段: $(cat "$TMP/body")"
fi
minimal_loc=$(header Location)
minimal_code=${minimal_loc#/r/}

status=$(request "$REDIRECTOR_BASE" GET "/r/$minimal_code")
assert_eq "$status" 302 '极简模式直连 PG 跳转状态码'
assert_eq "$(header Location)" "$minimal_url" '极简模式跳转 Location'

status=$(request "$CONSOLE_BASE" GET "/api/v1/links/$minimal_code")
assert_eq "$status" 200 '极简模式查询详情状态码'
if grep -q '"clicks"' "$TMP/body"; then
	fail "极简模式详情响应不应包含 clicks 字段: $(cat "$TMP/body")"
fi

status=$(request "$CONSOLE_BASE" GET '/api/v1/links?sort=clicks')
assert_eq "$status" 400 '极简模式 sort=clicks 应返回 400'
status=$(request "$CONSOLE_BASE" GET '/api/v1/links?sort=clicks_today')
assert_eq "$status" 400 '极简模式 sort=clicks_today 应返回 400'
status=$(request "$CONSOLE_BASE" GET "/api/v1/links/$minimal_code/stats")
assert_eq "$status" 404 '极简模式 GET /stats 应返回 404'

status=$(request "$CONSOLE_BASE" DELETE "/api/v1/links/$minimal_code")
assert_eq "$status" 204 '极简模式删除短链状态码'
status=$(request "$REDIRECTOR_BASE" GET "/r/$minimal_code")
assert_eq "$status" 404 '极简模式删除后跳转应立即 404'

minimal_jobs=$(psql_cmd "$TEST_DATABASE_URL" -Atc "SELECT count(*) FROM jobs")
assert_eq "$minimal_jobs" 0 '极简模式写库不应产生任何外围异步任务'

( sleep 15; kill -KILL "$REDIRECTOR_PID" "$CONSOLE_PID" 2>/dev/null ) &
WATCHDOG_PID=$!
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
assert_eq "$redirector_status" 0 '极简模式 redirector SIGTERM 后的退出码'
assert_eq "$console_status" 0 '极简模式 console SIGTERM 后的退出码'

echo 'smoke: ok'
