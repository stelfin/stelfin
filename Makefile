# stelfin — a non-custodial DAO treasury bot on Stellar, run from chat.
#
# Tests bring up their own Postgres (see ledger/ledger_test.go), so `make test`
# needs no local database, no Docker and no setup. The first run downloads a
# Postgres binary and caches it under $HOME.

.DEFAULT_GOAL := check

GO       ?= go
NODE     ?= node
CARGO    ?= cargo
FUZZTIME ?= 30s

# Pinned, not @latest. Formatting is a gate, and a gate whose rules arrive from
# upstream mid-week fails builds for reasons no commit in this repo caused.
#
# v0.11.0 rather than the newest: v0.12.0 requires Go 1.26, which is ahead of
# this module, so on a toolchain pinned to 1.25 it cannot run at all.
GOFUMPT  ?= mvdan.cc/gofumpt@v0.11.0

# The Rust contracts are deliberately not in `check`. They need a toolchain the
# Go work does not, and a first build takes ten minutes — putting them here
# would make the ordinary loop unusable for anyone touching Go. Run
# `make test-contracts` when the contracts change; CI runs both.
.PHONY: check
check: fmt vet test test-js ## Format, vet and test everything

.PHONY: fmt
fmt: ## Report files that gofumpt would change
	@err=$$(mktemp); \
	if ! out="$$($(GO) run $(GOFUMPT) -l . 2>$$err)"; then \
		echo "gofumpt could not run, so nothing was checked:"; cat $$err; rm -f $$err; exit 1; \
	fi; \
	rm -f $$err; \
	if [ -n "$$out" ]; then echo "needs formatting:"; echo "$$out"; exit 1; fi

.PHONY: vet
vet: ## Static analysis
	$(GO) vet ./...

.PHONY: test
test: ## Unit and integration tests
	$(GO) test ./...

.PHONY: test-js
test-js: ## Check the browser's renderer against the Go corpus, and its shape rules
	@cd web/static && $(NODE) --test describe.test.js policy.test.js pages.test.js styles.test.js

.PHONY: check-web
check-web: ## Typecheck, lint and build the marketing site
	@cd marketing && npx tsc --noEmit && npm run lint && npm run build

.PHONY: test-contracts
test-contracts: ## The Rust contracts, including the negative-auth suite
	$(CARGO) test --manifest-path contracts/Cargo.toml

.PHONY: build-contracts
build-contracts: ## Build the release WASM the committed hashes are taken from
	@cd contracts && stellar contract build

.PHONY: test-deploy
test-deploy: ## Prove the committed WASM hashes are on testnet (needs network)
	$(GO) test -tags=integration ./contracts/ -run OnChain -v

.PHONY: test-race
test-race: ## Tests under the race detector
	$(GO) test -race ./...

.PHONY: cover
cover: ## Tests with a coverage report at coverage.html
	$(GO) test -coverprofile=coverage.out ./...
	$(GO) tool cover -html=coverage.out -o coverage.html
	@echo "wrote coverage.html"

.PHONY: fuzz-money
fuzz-money: ## Fuzz the fixed-point money parser
	$(GO) test ./internal/money/ -run=xxx -fuzz=FuzzStringRoundTrip -fuzztime=$(FUZZTIME)
	$(GO) test ./internal/money/ -run=xxx -fuzz=FuzzParseNeverPanics -fuzztime=$(FUZZTIME)

.PHONY: tidy
tidy: ## Sync go.mod and go.sum
	$(GO) mod tidy

.PHONY: help
help: ## List targets
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'
