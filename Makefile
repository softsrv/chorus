APP_NAME         := chorus
BIN_DIR          := ./bin

# Load .env if present (for local dev convenience)
-include .env
export

.PHONY: dev run build test test-js test-integration test-system test-system-image test-cover boundaries fmt fmt-check vet lint vulncheck secret-scan mod-verify sqlc-check check \
        services-up services-down services-reset \
        daisyui-install tailwind tailwind-watch app \
        migrate-up migrate-down migrate-create migrate-status require-test-db migrate-test migrate-roundtrip-test \
        sqlc-generate docker-build prod clean

## ── Development ─────────────────────────────────────────────────────────────

# Full hot-reload: Postgres + smtp4dev, migrate, build the CSS once, then the
# app container (air watching the bind-mounted source) detached. Run
# `make tailwind-watch` in another terminal while editing web client styles.
dev: services-up migrate-up tailwind
	docker compose up -d --build app

# Postgres + smtp4dev only (the app container is started by dev/run).
services-up:
	docker compose up -d --wait postgres smtp4dev

services-down:
	docker compose down

# Destroys the local database volume.
services-reset:
	docker compose down -v

# App container in the foreground with hot reload; Ctrl-C stops it.
app:
	docker compose up --build app

# One-off app container without hot reload.
run:
	docker compose run --rm --build --service-ports app go run ./cmd/app

build:
	go build -ldflags="-s -w" -o $(BIN_DIR)/$(APP_NAME) ./cmd/app

## ── Quality ──────────────────────────────────────────────────────────────────

test:
	go test -race ./...

# Web client unit tests: the client JS modules against a minimal DOM and a stubbed fetch.
test-js:
	node --test web/jstest/*.test.mjs

# Integration tests run against TEST_DATABASE_URL (the chorus_test database).
test-integration: migrate-test migrate-roundtrip-test
	go test -race -tags integration -p 1 ./...

# System tests exec the built binary (or test the Docker image when SYSTEST_IMAGE is set).
# tailwind first: the binary embeds the generated stylesheet the tests assert.
test-system: migrate-test tailwind
	go test -race -tags systest -p 1 ./systest/...

# test-system-image builds the image and runs system tests against it (Linux-only: uses host networking).
test-system-image: migrate-test
	docker build -t chorus:systest .
	SYSTEST_IMAGE=chorus:systest go test -tags systest -p 1 ./systest/...

test-cover:
	go test -coverprofile=coverage.out ./...
	go tool cover -html=coverage.out

boundaries:
	go test -run 'TestImportBoundaries|TestWebClientImportsStayInWeb' ./internal/archtest/

fmt:
	gofmt -w .

fmt-check:
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "$$out" >&2; exit 1; fi

vet:
	go vet ./...
	go vet -tags integration ./...
	go vet -tags systest ./...

lint:
	golangci-lint run

vulncheck:
	go run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./...

GITLEAKS_VERSION ?= 8.24.0
secret-scan:
	go run github.com/zricethezav/gitleaks/v8@v$(GITLEAKS_VERSION) git --redact --config .gitleaks.toml

mod-verify:
	go mod download && go mod verify

# Everything CI runs.
check: boundaries vet test test-js test-integration test-system vulncheck secret-scan fmt-check sqlc-check mod-verify

## ── CSS ──────────────────────────────────────────────────────────────────────

# The DaisyUI .mjs bundles are committed next to app.css so the @plugin
# directive resolves in CI and Docker builds. To upgrade, bump
# DAISYUI_VERSION, run this, and commit the result.
DAISYUI_VERSION ?= 5.5.20
daisyui-install:
	curl -fsSLo web/static/css/daisyui.mjs \
	  https://github.com/saadeghi/daisyui/releases/download/v$(DAISYUI_VERSION)/daisyui.mjs
	curl -fsSLo web/static/css/daisyui-theme.mjs \
	  https://github.com/saadeghi/daisyui/releases/download/v$(DAISYUI_VERSION)/daisyui-theme.mjs
	@echo "DaisyUI $(DAISYUI_VERSION) downloaded to web/static/css/"

tailwind:
	tailwindcss -i ./web/static/css/app.css -o ./web/static/css/dist/app.css --minify

tailwind-watch:
	tailwindcss -i ./web/static/css/app.css -o ./web/static/css/dist/app.css --watch

## ── Database ─────────────────────────────────────────────────────────────────

migrate-up:
	migrate -path db/migrations -database "$(DATABASE_URL)" up

migrate-down:
	migrate -path db/migrations -database "$(DATABASE_URL)" down 1

# Every target that touches the test database needs TEST_DATABASE_URL, from
# .env (copy .env.example) or the environment. Fail fast with that, rather
# than running migrate against an empty URL.
require-test-db:
	@test -n "$(TEST_DATABASE_URL)" || { echo "TEST_DATABASE_URL is not set: set it in .env (see .env.example) or the environment" >&2; exit 1; }

# Migrates the way CI does (go run, pinned), so no migrate binary is needed.
migrate-test: require-test-db
	go run -tags postgres github.com/golang-migrate/migrate/v4/cmd/migrate@v4.20.1 -path db/migrations -database "$(TEST_DATABASE_URL)" up

migrate-roundtrip-test: require-test-db
	@url='$(TEST_DATABASE_URL)'; u="$${url%%\?*}"; db="$${u##*/}"; case "$$db" in *_test) ;; *) echo "migrate-roundtrip-test: refusing to run: TEST_DATABASE_URL database name must end in _test (got '$$db')" >&2; exit 1;; esac
	go run -tags postgres github.com/golang-migrate/migrate/v4/cmd/migrate@v4.20.1 -path db/migrations -database "$(TEST_DATABASE_URL)" down -all
	go run -tags postgres github.com/golang-migrate/migrate/v4/cmd/migrate@v4.20.1 -path db/migrations -database "$(TEST_DATABASE_URL)" up

migrate-create:
	@test -n "$(NAME)" || (echo "Usage: make migrate-create NAME=<name>" && exit 1)
	migrate create -ext sql -dir db/migrations -seq $(NAME)

migrate-status:
	migrate -path db/migrations -database "$(DATABASE_URL)" version

sqlc-generate:
	sqlc generate -f db/sqlc.yaml

sqlc-check:
	go run github.com/sqlc-dev/sqlc/cmd/sqlc@v1.31.1 generate -f db/sqlc.yaml
	git diff --exit-code internal/db

## ── Docker ───────────────────────────────────────────────────────────────────

docker-build:
	docker build -t $(APP_NAME):dev .

prod:
	docker build -t $(APP_NAME):prod .

## ── Clean ────────────────────────────────────────────────────────────────────

clean:
	rm -rf $(BIN_DIR) tmp/ coverage.out
