# 有 .env 就加载，并导出给子进程。
-include .env
export

# 让 make lint 能找到 go install 安装到 GOPATH/bin 的工具。
export PATH := $(shell go env GOPATH)/bin:$(PATH)

.PHONY: help run build test cover vet lint fmt check up prometheus down psql redis-cli

help: ## 列出所有命令
	@grep -E '^[a-zA-Z_-]+:.*## ' $(MAKEFILE_LIST) | awk -F ':.*## ' '{printf "  %-10s %s\n", $$1, $$2}'

run: ## 启动服务
	go run ./cmd/server

build: ## 编译到 bin/server
	go build -o bin/server ./cmd/server

test: ## 跑全部测试（带竞争检测，不使用缓存）
	go test -race -count=1 ./...

cover: ## 测试覆盖率
	go test -cover ./...

vet: ## 静态检查
	go vet ./...

lint: ## 完整静态检查（需要先安装 golangci-lint）
	golangci-lint run ./...

fmt: ## 格式化代码
	gofmt -w .

check: fmt vet test ## 提交前跑一遍：格式化、静态检查、测试

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
