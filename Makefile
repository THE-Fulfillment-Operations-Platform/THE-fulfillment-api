# THE Fulfillment API — dev tasks.
#
# The API port is read from .env (PORT=...), falling back to 8080 to match
# internal/config/config.go. `make run` frees a stale-held port first so you
# never hit "bind: address already in use" again.

PORT := $(shell grep -E '^PORT=' .env 2>/dev/null | tail -1 | cut -d= -f2)
PORT := $(if $(strip $(PORT)),$(strip $(PORT)),8080)

.PHONY: run kill-port build test tidy

## run: free the port if a stale server holds it, then start the API
run: kill-port
	go run ./cmd/server

## kill-port: kill any process still listening on $(PORT)
kill-port:
	@lsof -ti:$(PORT) | xargs kill -9 2>/dev/null || true
	@echo "port $(PORT) is free"

## build: compile the server binary to ./bin/server
build:
	go build -o bin/server ./cmd/server

## test: run the full test suite
test:
	go test ./...

## tidy: sync go.mod / go.sum
tidy:
	go mod tidy
