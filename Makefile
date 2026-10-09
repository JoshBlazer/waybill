# Waybill. Run `make help` for the list of targets.
SHELL := /usr/bin/env bash
.SHELLFLAGS := -euo pipefail -c
.DEFAULT_GOAL := help

COMPOSE := docker compose -f deploy/compose.yaml
GOLANGCI_LINT_VERSION := v2.14.0
GOLANGCI_LINT := bin/golangci-lint-$(GOLANGCI_LINT_VERSION)
GITLEAKS_IMAGE := ghcr.io/gitleaks/gitleaks:v8.30.1

# Files produced by `make gen`. CI fails if they differ from what is committed.
GENERATED := api/internal/httpapi/api.gen.go api/internal/store web/src/lib/api/schema.d.ts

.PHONY: help up down logs gen test test-api test-contracts test-web lint lint-api \
        lint-contracts lint-web check-gen e2e setup secrets

help: ## Show this help
	@grep -hE '^[a-zA-Z0-9_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[1m%-16s\033[0m %s\n", $$1, $$2}'

# --- Setup -------------------------------------------------------------------

setup: contracts/lib/forge-std/src web/node_modules ## Fetch submodules and web dependencies

contracts/lib/forge-std/src:
	git submodule update --init --recursive

web/node_modules: web/package-lock.json
	cd web && npm ci
	@touch web/node_modules

$(GOLANGCI_LINT):
	@mkdir -p bin
	curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/$(GOLANGCI_LINT_VERSION)/install.sh | sh -s -- -b bin $(GOLANGCI_LINT_VERSION)
	mv bin/golangci-lint $@

# --- Local stack ---------------------------------------------------------------

up: ## Build and start the local stack; waits until every service is healthy
	$(COMPOSE) up --build --detach --wait
	@echo "API: http://localhost:8080/v1/health   Web: http://localhost:3000"

down: ## Stop the local stack and delete its volumes
	$(COMPOSE) down --volumes --remove-orphans

logs: ## Follow logs from the local stack
	$(COMPOSE) logs --follow

# --- Code generation -----------------------------------------------------------

gen: web/node_modules ## Regenerate Go server types, sqlc queries and TS types
	cd api && go generate ./internal/httpapi/
	cd api && go tool sqlc generate
	cd web && npm run --silent gen:api

check-gen: gen ## Fail if generated code is out of date
	@if ! git diff --quiet -- $(GENERATED) || [ -n "$$(git ls-files --others --exclude-standard -- $(GENERATED))" ]; then \
		echo "Generated code is out of date. Run 'make gen' and commit the result:"; \
		git status --short -- $(GENERATED); exit 1; \
	fi
	@echo "Generated code is up to date."

# --- Tests ---------------------------------------------------------------------

test: test-api test-contracts test-web ## Run every test suite

test-api: ## Go unit and integration tests (integration tests need Docker)
	cd api && go test ./...

test-contracts: contracts/lib/forge-std/src ## Foundry tests
	cd contracts && forge test

test-web: web/node_modules ## Vitest unit tests
	cd web && npm test

e2e: ## End-to-end tests (stage 1)
	@echo "make e2e: not implemented yet; it arrives with the stage 1 thin slice (docs/ROADMAP.md)." >&2
	@exit 2

# --- Lint ----------------------------------------------------------------------

lint: lint-api lint-contracts lint-web check-gen ## Run every linter and the codegen drift check

secrets: ## Scan every commit for secrets (gitleaks, needs Docker)
	docker run --rm -v "$(CURDIR):/repo" $(GITLEAKS_IMAGE) git /repo --no-banner --redact --config /repo/.gitleaks.toml

lint-api: $(GOLANGCI_LINT)
	cd api && go vet ./... && ../$(GOLANGCI_LINT) run ./...

lint-contracts: contracts/lib/forge-std/src
	cd contracts && forge fmt --check && forge build --sizes >/dev/null

lint-web: web/node_modules
	cd web && npm run lint && npm run typecheck && npm run format:check
