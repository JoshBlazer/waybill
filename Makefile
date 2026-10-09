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
        lint-contracts lint-web check-gen e2e setup secrets budget

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

# A random demo-contractor API key per machine, never committed.
deploy/.env:
	@umask 077; printf 'WAYBILL_DEV_CONTRACTOR_KEY=wb_test_%s_%s\n' \
		"$$(LC_ALL=C tr -dc '0-9a-z' </dev/urandom | head -c 8)" \
		"$$(LC_ALL=C tr -dc 'a-z2-7' </dev/urandom | head -c 52)" > $@
	@echo "Generated $@ with a local demo API key."

up: deploy/.env contracts/lib/forge-std/src ## Build and start the local stack; waits until every service is healthy
	$(COMPOSE) up --build --detach --wait
	@echo "API: http://localhost:8080/v1/health   Web: http://localhost:3000"

down: deploy/.env ## Stop the local stack and delete its volumes
	$(COMPOSE) down --volumes --remove-orphans

logs: deploy/.env ## Follow logs from the local stack
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

PLAYWRIGHT_IMAGE := mcr.microsoft.com/playwright:v1.64.0-noble

e2e: web/node_modules ## End-to-end tests against the running stack (run make up first)
	docker run --rm --init --ipc=host --network waybill_default \
		--user "$$(id -u):$$(id -g)" -e HOME=/tmp \
		-v "$(CURDIR)/web:/web" -w /web \
		-e E2E_BASE_URL=http://web:3000 -e E2E_RPC_URL=http://anvil:8545 \
		$(PLAYWRIGHT_IMAGE) npx playwright test

# Initial JavaScript per page, gzipped (docs/DESIGN.md §6, ADR-033).
JS_BUDGET := 185000

budget: deploy/.env ## Check JS budgets on the running stack (needs an invoice: run make e2e first)
	@code=$$($(COMPOSE) exec -T postgres psql -h 127.0.0.1 -U waybill -qAt \
		-c "SELECT tracking_code FROM invoices ORDER BY created_at DESC LIMIT 1"); \
	test -n "$$code" || { echo "No invoice yet: run make e2e first." >&2; exit 1; }; \
	cd web && for page in /t/$$code /pay/$$code; do \
		node scripts/js-budget.mjs http://localhost:$${WAYBILL_WEB_PORT:-3000} $$page $(JS_BUDGET) || exit 1; \
	done

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
