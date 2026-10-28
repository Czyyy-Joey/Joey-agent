.PHONY: all build test smoke e2e web web-test lint run demo demo-coding demo-long clean

all: lint test web-test

build: web
	go build -o joey ./cmd/joey

# 离线单元 + 集成 + e2e 测试（无需网络，无需 API key）。沙箱测试需要 macOS。
test:
	go test -race ./...

# 调用真实网关；需要 ONEAPI_API_KEY 或 joey.toml 里的 api_key（可选 ONEAPI_BASE_URL、JOEY_SMOKE_MODEL）。
# JOEY_SMOKE_OVERFLOW=1 时额外发一个约 110 万 token 的请求，确认上下文超限错误能被识别。
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

# 脚本化的改代码任务：在 testdata/demo/goapp 的临时副本上搜索、打补丁、go build，每次修改都要审批。
demo-coding: web
	rm -rf /tmp/joey-goapp && cp -r testdata/demo/goapp /tmp/joey-goapp
	JOEY_APPROVAL=untrusted go run ./cmd/joey serve -provider scripted -script testdata/scenarios/coding_task.json -workspace /tmp/joey-goapp -db :memory:

# 脚本化的长任务：8000 token 的小窗口里连续读四个大文件，第三个读完后自动压缩上下文。
demo-long: web
	JOEY_CONTEXT_WINDOW=8000 go run ./cmd/joey serve -provider scripted -script testdata/scenarios/long_task.json -workspace testdata/demo/longtask -db :memory:

clean:
	rm -rf joey web/dist
