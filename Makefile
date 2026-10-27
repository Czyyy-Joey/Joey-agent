.PHONY: all build test smoke e2e web web-test lint run demo clean

all: lint test web-test

build: web
	go build -o joey ./cmd/joey

# 离线单元 + 集成 + e2e 测试（无需网络，无需 API key）。
test:
	go test -race ./...

# 调用真实网关；需要 ONEAPI_API_KEY（可选 ONEAPI_BASE_URL、JOEY_SMOKE_MODEL）。
smoke:
	go test -race -tags=smoke -run Smoke -v ./internal/llm/openaicompat/

e2e:
	go test -race -v ./e2e/

web:
	cd web && npm install --no-audit --no-fund && npm run build

web-test:
	cd web && npm test

lint:
	gofmt -l . | (! grep .)
	go vet ./...
	@go list -deps ./internal/agent | grep -E 'internal/(session|server)$$' && { echo "agent must not depend on session/server"; exit 1; } || true

# 真实模型；需要 ONEAPI_API_KEY。
run: web
	go run ./cmd/joey serve

# 脚本化模型，无需网络。演示数据写入单独的数据库，不会混进真实会话。
demo: web
	go run ./cmd/joey serve -provider scripted -script testdata/scenarios/read_readme.json -db ~/.joey/demo.db

clean:
	rm -rf joey web/dist
