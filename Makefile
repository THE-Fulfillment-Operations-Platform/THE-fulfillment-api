# THE Fulfillment API — dev tasks.
#
# `make run` chạy trên POSTGRES LOCAL (.env.local). Mặc định phải an toàn: .env
# đang trỏ vào Supabase production của khách, nên một lần quên là ghi thẳng vào
# dữ liệu thật (AutoMigrate còn tự thêm cột). Muốn nối production thì phải gõ
# hẳn `make run-prod` và gõ tay chữ xác nhận — không có đường vào đó do lỡ tay.
#
# Port đọc từ file env tương ứng, mặc định 8080 khớp internal/config/config.go.

ENV_LOCAL := .env.local
PORT := $(shell grep -E '^PORT=' $(ENV_LOCAL) 2>/dev/null | tail -1 | cut -d= -f2)
PORT := $(if $(strip $(PORT)),$(strip $(PORT)),8080)
PROD_PORT := $(shell grep -E '^PORT=' .env 2>/dev/null | tail -1 | cut -d= -f2)
PROD_PORT := $(if $(strip $(PROD_PORT)),$(strip $(PROD_PORT)),8080)

.PHONY: run run-prod db-check kill-port build test tidy

## run: chạy API trên Postgres LOCAL (.env.local) — mặc định an toàn
run: kill-port
	@test -f $(ENV_LOCAL) || { \
		echo "Thiếu $(ENV_LOCAL). Copy .env.example rồi sửa DB_* trỏ Postgres trên máy."; exit 1; }
	@grep -qE '^DB_HOST=(localhost|127\.0\.0\.1)' $(ENV_LOCAL) || { \
		echo "CHẶN: $(ENV_LOCAL) có DB_HOST không phải localhost — 'make run' chỉ chạy DB local."; exit 1; }
	@echo "→ DB local: $$(grep -E '^DB_NAME=' $(ENV_LOCAL) | cut -d= -f2) @ localhost"
	@set -a; . ./$(ENV_LOCAL); set +a; go run ./cmd/server

## run-prod: chạy API nối DB THẬT (.env) — phải gõ tay xác nhận
run-prod:
	@echo "CẢNH BÁO: sắp nối vào DB PRODUCTION của khách ($$(grep -E '^DB_HOST=' .env | cut -d= -f2))."
	@echo "Server sẽ chạy AutoMigrate trên đó. Gõ đúng chữ  chay-that  rồi Enter để tiếp tục:"
	@read -r answer; [ "$$answer" = "chay-that" ] || { echo "Đã huỷ."; exit 1; }
	@lsof -ti:$(PROD_PORT) | xargs kill -9 2>/dev/null || true
	go run ./cmd/server

## db-check: server đang chạy nối vào đâu (local hay production)
db-check:
	@for p in $$(lsof -ti:$(PORT) -ti:$(PROD_PORT) 2>/dev/null | sort -u); do \
		conn=$$(lsof -nP -p $$p 2>/dev/null | grep -E 'TCP .*->.*:5432 \(ESTABLISHED\)' | awk '{print $$9}' | head -1); \
		[ -n "$$conn" ] && echo "pid $$p → $$conn"; \
	done; echo "(không thấy dòng nào = không có server nào đang nối Postgres)"

## kill-port: kill tiến trình còn giữ cổng local $(PORT)
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
