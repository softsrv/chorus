APP_NAME         := chorus
BIN_DIR          := ./bin

# Load .env if present (for local dev convenience)
-include .env
export

.PHONY: dev run build test test-integration test-cover fmt vet lint vulncheck check \
        services-up services-down services-reset \
        daisyui-install tailwind tailwind-watch air \
        migrate-up migrate-down migrate-create migrate-status migrate-test \
        sqlc-generate docker-build prod clean

## ── Development ─────────────────────────────────────────────────────────────

# Full hot-reload: Postgres + smtp4dev containers, then Go (air) + Tailwind watch.
dev: services-up migrate-up tailwind
	$(MAKE) -j2 air tailwind-watch

services-up:
	docker compose up -d --wait

services-down:
	docker compose down

# Destroys the local database volume.
services-reset:
	docker compose down -v

air:
	air

run:
	go run ./cmd/app

build:
	go build -ldflags="-s -w" -o $(BIN_DIR)/$(APP_NAME) ./cmd/app

## ── Quality ──────────────────────────────────────────────────────────────────

test:
	go test -race ./...

# Integration tests run against TEST_DATABASE_URL (the chorus_test database).
test-integration: migrate-test
	go test -race -tags integration -p 1 ./...

test-cover:
	go test -coverprofile=coverage.out ./...
	go tool cover -html=coverage.out

fmt:
	gofmt -w .

vet:
	go vet ./...
	go vet -tags integration ./...

lint:
	golangci-lint run

vulncheck:
	go run golang.org/x/vuln/cmd/govulncheck@latest ./...

# Everything CI runs.
check: vet test test-integration vulncheck

## ── CSS ──────────────────────────────────────────────────────────────────────

# Download DaisyUI .mjs bundles next to app.css so the @plugin directive
# can resolve them. Re-run this whenever you upgrade DaisyUI.
daisyui-install:
	curl -sLo web/static/css/daisyui.mjs \
	  https://github.com/saadeghi/daisyui/releases/latest/download/daisyui.mjs
	curl -sLo web/static/css/daisyui-theme.mjs \
	  https://github.com/saadeghi/daisyui/releases/latest/download/daisyui-theme.mjs
	@echo "DaisyUI bundles downloaded to web/static/css/"

tailwind:
	tailwindcss -i ./web/static/css/app.css -o ./web/static/css/dist/app.css --minify

tailwind-watch:
	tailwindcss -i ./web/static/css/app.css -o ./web/static/css/dist/app.css --watch

## ── Database ─────────────────────────────────────────────────────────────────

migrate-up:
	migrate -path db/migrations -database "$(DATABASE_URL)" up

migrate-down:
	migrate -path db/migrations -database "$(DATABASE_URL)" down 1

migrate-test:
	migrate -path db/migrations -database "$(TEST_DATABASE_URL)" up

migrate-create:
	@test -n "$(NAME)" || (echo "Usage: make migrate-create NAME=<name>" && exit 1)
	migrate create -ext sql -dir db/migrations -seq $(NAME)

migrate-status:
	migrate -path db/migrations -database "$(DATABASE_URL)" version

sqlc-generate:
	sqlc generate -f db/sqlc.yaml

## ── Docker ───────────────────────────────────────────────────────────────────

docker-build:
	docker build -t $(APP_NAME):dev .

prod:
	docker build -t $(APP_NAME):prod .

## ── Clean ────────────────────────────────────────────────────────────────────

clean:
	rm -rf $(BIN_DIR) tmp/ coverage.out
