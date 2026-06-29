PROJECT   := kutu
MAIN_FILE := cmd/$(PROJECT)/main.go

BUILD_DATE   := $(shell date -u '+%Y-%m-%d_%H:%M:%S')
BUILD_COMMIT := $(shell git rev-parse --short HEAD 2>/dev/null || echo "-")
VERSION      := $(or $(IMAGE_TAG),$(shell git describe --tags --first-parent --match "v*" 2>/dev/null || echo v0.0.0))

LDFLAGS := -X main.date=$(BUILD_DATE) -X main.commit=$(BUILD_COMMIT) -X main.version=$(VERSION)

.DEFAULT_GOAL := help

.PHONY: help
help: ## Show this help
	@grep -hE '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS=":.*?## "}; {printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'

.PHONY: install-ui
install-ui: ## Install UI dependencies (pnpm)
	@cd _ui && pnpm install

.PHONY: build-ui
build-ui: install-ui ## Build the SPA and embed it into internal/server/dist
	@cd _ui && pnpm build
	@rm -rf internal/server/dist && mv _ui/dist internal/server/dist
	@touch internal/server/dist/.gitkeep
	@echo "> UI generated into internal/server/dist"

.PHONY: build
build: build-ui build-go ## Generate the UI then build the Go binary

.PHONY: build-go
build-go: ## Build the Go binary into ./bin (uses the already-embedded UI)
	@mkdir -p bin
	go build -ldflags="$(LDFLAGS)" -o bin/$(PROJECT) $(MAIN_FILE)

.PHONY: run-ui
run-ui: ## Run the Vite dev server (proxies API to :8080)
	@cd _ui && pnpm dev

.PHONY: run
run: export CONFIG_FILE := env/config/kutu.yaml
run: export KUTU_LOG_LEVEL := debug
run: ## Run the server (needs KUTU_STORAGE_DSN; see `make env`)
	go run -ldflags="$(LDFLAGS)" $(MAIN_FILE)

.PHONY: test
test: ## Run unit tests
	go test ./...

.PHONY: tidy
tidy: ## Tidy go.mod / go.sum
	go mod tidy

.PHONY: env
env: ## Start the development environment
	docker compose -f env/compose.yaml up -d

.PHONY: env-down
env-down: ## Stop the development environment
	docker compose -f env/compose.yaml down
