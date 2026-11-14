# 有 .env 就加载，并导出给子进程。
-include .env
export

# 让 make lint 能找到 go install 安装到 GOPATH/bin 的工具。
export PATH := $(shell go env GOPATH)/bin:$(PATH)

.PHONY: help run build console migrate test test-unit cover vet lint fmt fmt-check check verify selfcheck \
	test-db test-integration smoke deps-up up prometheus down psql redis-cli

help: ## 列出所有命令
	@grep -hE '^[a-zA-Z_-]+:.*## ' $(MAKEFILE_LIST) | awk -F ':.*## ' '{printf "  %-18s %s\n", $$1, $$2}'

run: ## 启动服务
	go run ./cmd/server

build: ## 编译到 bin/server 和 bin/console
	go build -o bin/server ./cmd/server
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

deps-up: ## 只启动 compose 的 PostgreSQL 和 Redis（本机已有依赖时不要执行）
	docker compose up -d --wait postgres redis

test-db: ## 创建测试库（如不存在）、重建 schema 并执行迁移
	./scripts/prepare-test-db.sh

test-integration: test-db ## 跑全部测试，包含需要 PostgreSQL/Redis 的集成测试
	go test -race -count=1 ./...

smoke: test-db build ## 启动真实进程做冒烟测试（独立端口，使用测试库）
	./scripts/smoke.sh

selfcheck: test-db verify test-integration smoke ## agent 自测：完整检查、集成测试和冒烟

up: ## 启动 PostgreSQL、Redis 和 Prometheus
	docker compose up -d --wait

prometheus: ## 启动本地 Prometheus
	docker compose up -d prometheus

down: ## 停止依赖（数据保留；加 -v 才会清空）
	docker compose down

psql: ## 进入 PostgreSQL 命令行
	docker compose exec postgres psql -U shortlink -d shortlink

redis-cli: ## 进入 Redis 命令行
	docker compose exec redis redis-cli
