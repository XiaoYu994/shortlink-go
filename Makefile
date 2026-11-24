# 有 .env 就加载，并导出给子进程。
-include .env
export

# 跳转进程读 ADDR，管理进程读 CONSOLE_ADDR。两边不能共用一个地址。
ADDR ?= :8080
CONSOLE_ADDR ?= :8081
WORKER_ADDR ?= :8082
redirector_port := $(lastword $(subst :, ,$(ADDR)))
console_port := $(lastword $(subst :, ,$(CONSOLE_ADDR)))
worker_port := $(lastword $(subst :, ,$(WORKER_ADDR)))

# 让 make lint 能找到 go install 安装到 GOPATH/bin 的工具。
export PATH := $(shell go env GOPATH)/bin:$(PATH)

.PHONY: help run-redirector run-console run-worker build console migrate test test-unit cover vet lint fmt fmt-check check verify selfcheck \
	test-db test-integration smoke deps-up up stack prometheus prometheus-config down psql redis-cli \
	web-install web-dev web-build web-fmt web-fmt-check web-lint web-test web-check

help: ## 列出所有命令
	@grep -hE '^[a-zA-Z_-]+:.*## ' $(MAKEFILE_LIST) | awk -F ':.*## ' '{printf "  %-18s %s\n", $$1, $$2}'

run-redirector: ## 启动跳转进程（ADDR，默认 :8080）
	ADDR=$(ADDR) go run ./cmd/redirector

run-console: ## 启动管理进程（CONSOLE_ADDR，默认 :8081）
	CONSOLE_ADDR=$(CONSOLE_ADDR) go run ./cmd/console serve

run-worker: ## 启动后台任务进程（异步 URL 风控与定期清理）
	go run ./cmd/console worker

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

verify: check lint build ## 完整代码检查：check + golangci-lint + build

deps-up: ## 只启动 compose 的 PostgreSQL、Redis 和 Dex（本机已有依赖时不要执行）
	docker compose up -d --wait postgres redis dex

test-db: ## 创建测试库（如不存在）、重建 schema 并执行迁移
	./scripts/prepare-test-db.sh

test-integration: test-db ## 跑全部测试，包含需要 PostgreSQL/Redis 的集成测试
	go test -race -count=1 ./...

smoke: test-db build ## 启动真实进程做冒烟测试（独立端口，使用测试库）
	go build -tags devtest -o bin/console-smoke ./cmd/console
	CONSOLE_BIN=./bin/console-smoke ./scripts/smoke.sh

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
		printf '%s\n' '          - host.docker.internal:$(redirector_port)'; \
		printf '%s\n' '  - job_name: console'; \
		printf '%s\n' '    static_configs:'; \
		printf '%s\n' '      - targets:'; \
		printf '%s\n' '          - host.docker.internal:$(console_port)'; \
		printf '%s\n' '  - job_name: worker'; \
		printf '%s\n' '    static_configs:'; \
		printf '%s\n' '      - targets:'; \
		printf '%s\n' '          - host.docker.internal:$(worker_port)'; \
	} > deploy/prometheus.local.yml

prometheus: prometheus-config ## 启动本地 Prometheus，抓取端口跟随 ADDR / CONSOLE_ADDR
	docker compose up -d --force-recreate --no-deps prometheus

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
