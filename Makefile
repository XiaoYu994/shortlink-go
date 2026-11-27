# 有 .env 就加载，并导出给子进程。
-include .env
export

# 跳转进程读 ADDR（默认 :8080），管理进程读 CONSOLE_ADDR（默认 :8081），worker 读 WORKER_ADDR（默认关闭）。
# 不对 ADDR/CONSOLE_ADDR/WORKER_ADDR 做 ?= 赋值，避免 export 覆盖 config.yaml 中的 *.addr。
redirector_port := $(lastword $(subst :, ,$(or $(ADDR),:8080)))
console_port := $(lastword $(subst :, ,$(or $(CONSOLE_ADDR),:8081)))
worker_port := $(lastword $(subst :, ,$(or $(WORKER_ADDR),:8082)))
redirector_ops_port := $(if $(OPS_ADDR),$(lastword $(subst :, ,$(OPS_ADDR))),$(redirector_port))
console_ops_port := $(if $(CONSOLE_OPS_ADDR),$(lastword $(subst :, ,$(CONSOLE_OPS_ADDR))),$(console_port))

# 本地开发默认通过 air 热更新（改 .go 自动重编译重启）；传 HOT_RELOAD=0 可回退普通 go run。
HOT_RELOAD ?= 1
# 热更新模式默认跳过默认 5s 摘流等待，避免每次保存卡 5 秒或端口冲突；需演练摘流时传 DEV_DRAIN_DELAY=5s 或 HOT_RELOAD=0。
DEV_DRAIN_DELAY ?= 0s
AIR_VERSION ?= v1.67.4

# 让 make lint 能找到 go install 安装到 GOPATH/bin 的工具。
export PATH := $(shell go env GOPATH)/bin:$(PATH)

.PHONY: help install-air check-air run-redirector run-console run-worker build console migrate test test-unit cover vet lint fmt fmt-check check arch-check verify selfcheck \
	test-db test-integration smoke deps-up up stack tracing prometheus prometheus-config down psql redis-cli \
	web-install web-dev web-build web-fmt web-fmt-check web-lint web-test web-check

help: ## 列出所有命令
	@grep -hE '^[a-zA-Z_-]+:.*## ' $(MAKEFILE_LIST) | awk -F ':.*## ' '{printf "  %-18s %s\n", $$1, $$2}'

install-air: ## 安装本地 Go 热更新工具 air（安装到 GOPATH/bin，不污染 go.mod）
	go install github.com/air-verse/air@$(AIR_VERSION)

check-air:
	@if [ "$(HOT_RELOAD)" = "1" ] && ! command -v air >/dev/null 2>&1; then \
		echo "未找到 air，请先运行: make install-air（或传 HOT_RELOAD=0 使用普通 go run）" >&2; \
		exit 1; \
	fi

ifeq ($(HOT_RELOAD),1)
run-redirector: check-air ## 启动跳转进程（ADDR，默认 :8080；默认热更新，HOT_RELOAD=0 关闭）
	DRAIN_DELAY=$(DEV_DRAIN_DELAY) air \
		--tmp_dir tmp/air/redirector \
		--build.include_dir cmd/redirector --build.include_dir internal \
		--build.cmd "go build -o ./tmp/air/redirector/app ./cmd/redirector" \
		--build.entrypoint ./tmp/air/redirector/app \
		$(if $(strip $(ARGS)),-- $(strip $(ARGS)))

run-console: check-air ## 启动管理进程（CONSOLE_ADDR，默认 :8081；默认热更新，HOT_RELOAD=0 关闭）
	DRAIN_DELAY=$(DEV_DRAIN_DELAY) air \
		--tmp_dir tmp/air/console \
		--build.include_dir cmd/console --build.include_dir internal \
		--build.cmd "go build -o ./tmp/air/console/app ./cmd/console" \
		--build.entrypoint ./tmp/air/console/app \
		-- $(strip serve $(ARGS))

run-worker: check-air ## 启动后台任务进程（异步风控/Webhook/清理；默认热更新，HOT_RELOAD=0 关闭）
	air \
		--tmp_dir tmp/air/worker \
		--build.include_dir cmd/console --build.include_dir internal \
		--build.cmd "go build -o ./tmp/air/worker/app ./cmd/console" \
		--build.entrypoint ./tmp/air/worker/app \
		-- $(strip worker $(ARGS))
else
run-redirector:
	go run ./cmd/redirector $(ARGS)

run-console:
	go run ./cmd/console $(strip serve $(ARGS))

run-worker:
	go run ./cmd/console $(strip worker $(ARGS))
endif

build: ## 编译到 bin/redirector 和 bin/console
	go build -o bin/redirector ./cmd/redirector
	go build -o bin/console ./cmd/console

console: ## 运维命令。例如 make console ARGS="key issue --name acme"
	go run ./cmd/console $(ARGS)

migrate: ## 把 DATABASE_URL 迁到最新。只允许 shortlink 库
	go run ./cmd/migrate up

test: ## 跑全部测试，使用当前 .env 里的 TEST_*（带竞争检测，不使用缓存）
	go test -race -count=1 ./...

test-unit: ## 只跑不依赖 PostgreSQL/Redis 的测试
	TEST_DATABASE_URL= TEST_REDIS_URL= go test -race -count=1 ./...

cover: ## 测试覆盖率
	go test -cover ./...

vet: ## 静态检查
	go vet ./...

lint: ## 完整静态检查（需要先安装 golangci-lint）
	golangci-lint run ./...

fmt: ## 格式化代码
	gofmt -w .

fmt-check: ## 检查格式，不修改文件（CI 和 agent 用）
	@files="$$(gofmt -l .)"; \
	if [ -n "$$files" ]; then \
		echo "gofmt 需要处理这些文件：" >&2; \
		echo "$$files" >&2; \
		echo "运行 make fmt 后重试" >&2; \
		exit 1; \
	fi

check: fmt-check vet test-unit ## 快速检查：格式、vet、单测（不启动依赖）

arch-check: ## 校验公开面与底座包的静态依赖边界（ADR 0024）
	@set -eu; \
	check_absent() { \
		pkg=$$1; shift; \
		self=$$(go list "$$pkg"); \
		deps=$$(go list -deps "$$pkg"); \
		deps=$$(printf '%s\n' "$$deps" | grep -Fxv "$$self" || true); \
		for forbid in "$$@"; do \
			if printf '%s\n' "$$deps" | grep -Eq "$$forbid"; then \
				echo "arch-check 失败: $$pkg 不应依赖 $$forbid" >&2; \
				exit 1; \
			fi; \
		done; \
	}; \
	check_direct_absent() { \
		pkg=$$1; shift; \
		imports=$$(go list -f '{{join .Imports "\n"}}' "$$pkg"); \
		for forbid in "$$@"; do \
			if printf '%s\n' "$$imports" | grep -Eq "$$forbid"; then \
				echo "arch-check 失败: $$pkg 不应直接引入 $$forbid" >&2; \
				exit 1; \
			fi; \
		done; \
	}; \
	check_internal_allow() { \
		pkg=$$1; shift; \
		self=$$(go list "$$pkg"); \
		deps=$$(go list -deps "$$pkg"); \
		bad=$$(printf '%s\n' "$$deps" | grep -Fxv "$$self" | grep -E '^shortlink/internal/' || true); \
		if [ "$$#" -gt 0 ]; then \
			for allow in "$$@"; do \
				if [ -z "$$bad" ]; then break; fi; \
				bad=$$(printf '%s\n' "$$bad" | grep -Fxv "$$allow" || true); \
			done; \
		fi; \
		if [ -n "$$bad" ]; then \
			echo "arch-check 失败: $$pkg 出现未允许的 internal 依赖:" >&2; \
			printf '%s\n' "$$bad" >&2; \
			exit 1; \
		fi; \
	}; \
	check_absent ./cmd/redirector \
		'^shortlink/internal/(console|identity|risk|job|audit|postgres|safehttp|webhook|mailer|sms)(/|$$)' \
		'^github\.com/coreos/go-oidc' \
		'^golang\.org/x/oauth2'; \
	check_internal_allow ./internal/logx; \
	check_absent ./internal/logx \
		'^github\.com/jackc/pgx' \
		'^github\.com/redis/go-redis' \
		'^github\.com/prometheus/' \
		'^github\.com/coreos/go-oidc' \
		'^golang\.org/x/oauth2'; \
	check_internal_allow ./internal/httpx \
		shortlink/internal/logx; \
	check_internal_allow ./internal/config \
		shortlink/internal/logx \
		shortlink/internal/pgdb; \
	check_internal_allow ./internal/safehttp \
		shortlink/internal/ratelimit; \
	check_internal_allow ./internal/pgdb; \
	check_absent ./internal/pgdb \
		'^github\.com/redis/go-redis' \
		'^github\.com/prometheus/' \
		'^github\.com/coreos/go-oidc' \
		'^golang\.org/x/oauth2'; \
	check_internal_allow ./internal/pgredirect \
		shortlink/internal/pgdb \
		shortlink/internal/link \
		shortlink/internal/click; \
	check_absent ./internal/pgredirect \
		'^github\.com/redis/go-redis' \
		'^github\.com/coreos/go-oidc' \
		'^golang\.org/x/oauth2'; \
	check_internal_allow ./internal/identity \
		shortlink/internal/tenant \
		shortlink/internal/logx; \
	check_absent ./internal/identity \
		'^github\.com/coreos/go-oidc' \
		'^golang\.org/x/oauth2' \
		'^github\.com/jackc/pgx' \
		'^github\.com/redis/go-redis'; \
	check_internal_allow ./internal/ratelimit; \
	check_absent ./internal/ratelimit \
		'^github\.com/jackc/pgx' \
		'^github\.com/redis/go-redis' \
		'^github\.com/prometheus/' \
		'^github\.com/coreos/go-oidc' \
		'^golang\.org/x/oauth2'; \
	check_internal_allow ./internal/mailer; \
	check_absent ./internal/mailer \
		'^github\.com/jackc/pgx' \
		'^github\.com/redis/go-redis' \
		'^github\.com/coreos/go-oidc' \
		'^golang\.org/x/oauth2'; \
	check_internal_allow ./internal/tracex; \
	for p in $$(go list ./internal/... | grep -Fxv 'shortlink/internal/tracex'); do \
		check_absent "$$p" '^go\.opentelemetry\.io/otel/(sdk|exporters)(/|$$)'; \
	done; \
	for p in ./internal/ratelimit ./internal/safehttp ./internal/mailer ./internal/link ./internal/tenant ./internal/idgen ./internal/audit; do \
		check_absent "$$p" '^go\.opentelemetry\.io/'; \
	done; \
	for p in ./internal/config ./internal/identity ./internal/sms; do \
		check_direct_absent "$$p" '^go\.opentelemetry\.io/'; \
	done; \
	check_internal_allow ./internal/sms \
		shortlink/internal/safehttp \
		shortlink/internal/ratelimit \
		shortlink/internal/logx; \
	check_absent ./internal/sms \
		'^github\.com/jackc/pgx' \
		'^github\.com/redis/go-redis' \
		'^github\.com/coreos/go-oidc' \
		'^golang\.org/x/oauth2'; \
	echo "arch-check: ok"

verify: check arch-check lint build ## 完整代码检查：check + arch-check + golangci-lint + build

deps-up: ## 只启动 compose 的 PostgreSQL、Redis 和 Dex（本机已有依赖时不要执行）
	docker compose up -d --wait postgres redis dex

test-db: ## 创建测试库（如不存在）、重建 schema 并执行迁移
	./scripts/prepare-test-db.sh

test-integration: test-db ## 跑全部测试，包含需要 PostgreSQL/Redis 的集成测试
	go test -race -count=1 ./...

smoke: test-db build ## 启动真实进程做冒烟测试（独立端口，使用测试库）
	./scripts/smoke.sh

selfcheck: test-db verify test-integration smoke ## agent 自测：完整检查、集成测试和冒烟

stack: ## 构建并启动可选应用栈（profile app，不随 up 启动）
	docker compose up -d --wait postgres redis
	docker compose --profile app up -d --build --wait redirector console worker gateway

up: prometheus-config ## 启动 PostgreSQL、Redis、Dex 和 Prometheus
	docker compose up -d --wait postgres redis dex
	docker compose up -d --force-recreate --no-deps prometheus

prometheus-config: ## 按两个监听地址生成 Prometheus 抓取配置
	@mkdir -p deploy
	@{ \
		printf '%s\n' '# 由 make 生成。端口来自 ADDR 和 CONSOLE_ADDR。'; \
		printf '%s\n' 'global:'; \
		printf '%s\n' '  scrape_interval: 5s'; \
		printf '%s\n' '  scrape_timeout: 4s'; \
		printf '%s\n' '  evaluation_interval: 15s'; \
		printf '%s\n' ''; \
		printf '%s\n' 'rule_files:'; \
		printf '%s\n' '  - alerts.yml'; \
		printf '%s\n' ''; \
		printf '%s\n' 'scrape_configs:'; \
		printf '%s\n' '  - job_name: redirector'; \
		printf '%s\n' '    static_configs:'; \
		printf '%s\n' '      - targets:'; \
		printf '%s\n' '          - host.docker.internal:$(redirector_ops_port)'; \
		printf '%s\n' '  - job_name: console'; \
		printf '%s\n' '    static_configs:'; \
		printf '%s\n' '      - targets:'; \
		printf '%s\n' '          - host.docker.internal:$(console_ops_port)'; \
		printf '%s\n' '  - job_name: worker'; \
		printf '%s\n' '    static_configs:'; \
		printf '%s\n' '      - targets:'; \
		printf '%s\n' '          - host.docker.internal:$(worker_port)'; \
	} > deploy/prometheus.local.yml

prometheus: prometheus-config ## 启动本地 Prometheus，抓取端口跟随 ADDR / CONSOLE_ADDR
	docker compose up -d --force-recreate --no-deps prometheus

tracing: ## 启动可选本地 Jaeger（UI :16686，OTLP HTTP :4318）
	docker compose --profile tracing up -d jaeger

down: ## 停止依赖（数据保留；加 -v 才会清空）
	docker compose down

psql: ## 进入 PostgreSQL 命令行
	docker compose exec postgres psql -U shortlink -d shortlink

redis-cli: ## 进入 Redis 命令行
	docker compose exec redis redis-cli

web-install: ## 安装最小前端控制台依赖（需要 Node.js 与 pnpm）
	pnpm --dir web install

web-dev: ## 启动前端开发服务器（同源代理到 console 与 redirector）
	VITE_CONSOLE_ORIGIN=http://127.0.0.1:$(console_port) VITE_REDIRECTOR_ORIGIN=http://127.0.0.1:$(redirector_port) pnpm --dir web dev

web-build: ## 类型检查并构建前端静态产物到 web/dist
	pnpm --dir web build

web-fmt: ## 使用 Prettier 格式化前端代码
	pnpm --dir web fmt

web-fmt-check: ## 检查前端代码格式（不修改文件）
	pnpm --dir web fmt:check

web-lint: ## 前端 ESLint 静态检查（含 react-hooks 与 TypeScript 规则）
	pnpm --dir web lint

web-test: ## 运行前端 Vitest + Testing Library 组件测试
	pnpm --dir web test

web-check: ## 前端完整检查：prettier + tsc + eslint + vitest + build
	pnpm --dir web check
