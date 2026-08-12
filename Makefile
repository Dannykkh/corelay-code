.PHONY: build web deploy run test clean

# Build everything
build: web deploy go

# Build Go binary
go:
	go build -o corelaycode ./cmd/proxy
	go build -o corelaycode-acp ./cmd/corelaycode-acp
	go build -o corelaycode-profile ./cmd/corelaycode-profile

# Build frontend
web:
	cd web && npm run build

# Copy web assets to Go embed directory
deploy:
	rm -f internal/server/webdist/assets/index-*.js internal/server/webdist/assets/index-*.css
	cp -r web/dist/* internal/server/webdist/

# Build + deploy in one step
all: web deploy go
	@echo "Build complete: ./corelaycode"

# Run with default settings
run: all
	./corelaycode -provider ollama -model qwen3:14b

# Run tests
test:
	go test ./internal/... -count=1

# Run tests with verbose
test-v:
	go test ./internal/... -v -count=1

# Count tests
test-count:
	@go test ./internal/... -v -count=1 2>&1 | grep "PASS:" | wc -l

# Count lines
loc:
	@echo "Go backend:"
	@find internal -name "*.go" | xargs wc -l | tail -1
	@echo "Go tests:"
	@find internal -name "*_test.go" | xargs wc -l | tail -1
	@echo "Web frontend:"
	@find web/src -name "*.ts" -o -name "*.tsx" | xargs wc -l | tail -1

# Clean build artifacts
clean:
	rm -f corelaycode corelaycode.exe corelaycode-acp corelaycode-acp.exe corelaycode-profile corelaycode-profile.exe proxy-go.exe
	rm -rf web/dist
	rm -f internal/server/webdist/assets/index-*.js internal/server/webdist/assets/index-*.css
