.PHONY: setup reset run build test test-all demo fmt vet sync-migrations

# 本地用户态基础设施（PG 5433 / Redis DB15 / Kafka KRaft 9092）
setup:
	bash scripts/setup-local.sh

reset:
	bash scripts/reset-local.sh

# internal/store 通过 go:embed 内嵌迁移；规范源在 db/migrations，构建前同步
sync-migrations:
	cp db/migrations/*.sql internal/store/migrations/

build: sync-migrations
	go build ./...

run: sync-migrations
	go run ./cmd/server

fmt:
	gofmt -w cmd internal

vet: sync-migrations
	go vet ./...

# 纯单元测试
test:
	go test ./internal/pricing/

# 单元 + 集成测试（依赖 setup 启动的 PG/Redis）
test-all: sync-migrations
	TEST_DATABASE_URL="host=$(PWD)/.local/run port=5433 user=recycle dbname=recycling_test sslmode=disable" \
		go test ./internal/...

# 端到端五场景（需服务已在 :8080 运行）
demo:
	bash scripts/demo.sh
