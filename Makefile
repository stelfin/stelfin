# stelfin — a non-custodial, WhatsApp-native stablecoin wallet on Stellar.
#
# Tests bring up their own Postgres (see ledger/ledger_test.go), so `make test`
# needs no local database, no Docker and no setup. The first run downloads a
# Postgres binary and caches it under $HOME.

.DEFAULT_GOAL := check

GO      ?= go
FUZZTIME ?= 30s

.PHONY: check
check: fmt vet test ## Format, vet and test everything

.PHONY: fmt
fmt: ## Report files that gofmt would change
	@out="$$($(GO) run mvdan.cc/gofumpt@latest -l . 2>/dev/null || gofmt -l .)"; \
	if [ -n "$$out" ]; then echo "needs formatting:"; echo "$$out"; exit 1; fi

.PHONY: vet
vet: ## Static analysis
	$(GO) vet ./...

.PHONY: test
test: ## Unit and integration tests
	$(GO) test ./...

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
