# s3d — a lightweight single-node S3-compatible object server.
#
# The compatibility suite is the important target here. Unit tests prove the
# implementation against itself; `make test-compat` proves it against aws-cli
# and boto3, which is what actually decides whether real clients work.

SHELL := /bin/bash
BINARY := bin/s3d
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

# The database the tests run against. Each test package migrates into its own
# schema inside it, so they can run in parallel without colliding.
TEST_DATABASE_URL ?= postgres://s3d:test@localhost:55432/s3d?sslmode=disable
TEST_PG_CONTAINER := s3d-test-pg

.PHONY: help
help: ## Show this help
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}'

.PHONY: web
web: ## Build the console interface into the Go embed directory
	cd web && npm ci --silent && npm run build
	@$(MAKE) --no-print-directory web-keepfile

.PHONY: web-keepfile
web-keepfile: ## Restore the placeholder the Vite build removes
	@# Vite empties its output directory, which deletes the tracked .gitkeep
	@# that lets `go build` work before the frontend has ever been built.
	@if [ ! -f internal/web/dist/.gitkeep ]; then 		printf '%s\n' \
			'This directory holds the built console, which the Go binary embeds.' \
			'' \
			'It is kept in the repository (empty) because go:embed fails at compile time if' \
			'its pattern matches nothing — so without this file, `go build` would not work' \
			'until someone had run the frontend build first.' \
			'' \
			'The Vite build empties this directory, so the Makefile and Dockerfile restore' \
			'this file afterwards.' > internal/web/dist/.gitkeep; \
	fi

.PHONY: build
build: ## Build the binary (without rebuilding the console)
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(BINARY) ./cmd/s3d

.PHONY: all
all: web build ## Build the console and the binary

.PHONY: fmt
fmt: ## Format all Go source
	gofmt -w ./cmd ./internal

.PHONY: check
check: ## Vet, check formatting and build
	go vet ./...
	@unformatted=$$(gofmt -l ./cmd ./internal); \
		if [ -n "$$unformatted" ]; then echo "not gofmt'd:"; echo "$$unformatted"; exit 1; fi
	go build ./...

.PHONY: test
test: test-db-up ## Run the unit and integration tests
	TEST_DATABASE_URL="$(TEST_DATABASE_URL)" go test ./... -count=1 -timeout 15m

.PHONY: test-short
test-short: ## Run only the tests that need no database or large I/O
	go test ./... -short -count=1

.PHONY: test-compat
test-compat: test-db-up ## Run the aws-cli and boto3 compatibility suite (needs Docker)
	S3D_EXTERNAL_TESTS=1 TEST_DATABASE_URL="$(TEST_DATABASE_URL)" \
		go test ./internal/s3api/ -run 'Compatibility' -count=1 -timeout 30m -v

.PHONY: test-all
test-all: test test-compat ## Everything

.PHONY: test-db-up
test-db-up: ## Start the Postgres the tests use
	@if ! docker ps --format '{{.Names}}' | grep -q '^$(TEST_PG_CONTAINER)$$'; then \
		echo "starting $(TEST_PG_CONTAINER)"; \
		docker run -d --rm --name $(TEST_PG_CONTAINER) \
			-e POSTGRES_PASSWORD=test -e POSTGRES_USER=s3d -e POSTGRES_DB=s3d \
			-p 55432:5432 postgres:17-alpine > /dev/null; \
		until docker exec $(TEST_PG_CONTAINER) pg_isready -U s3d -q 2>/dev/null; do sleep 1; done; \
	fi

.PHONY: test-db-down
test-db-down: ## Stop the test Postgres
	-docker rm -f $(TEST_PG_CONTAINER) 2>/dev/null

.PHONY: clean
clean: ## Remove build output
	rm -rf bin dist web/dist
