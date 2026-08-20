.PHONY: build test lint check release install-test clean help

build: ## Build litefind
	go build -o build/litefind .

test: ## Run tests
	go test ./...

lint: ## Run static analysis
	go vet ./...
	shellcheck install.sh

check: test lint ## Run all checks
	go fix -diff ./...
	golangci-lint run ./...

release: ## Publish a GitHub release
	goreleaser release --clean

install-test: ## Smoke-test install.sh against a local snapshot release
	@./scripts/install-test.sh

clean: ## Remove build artifacts
	-rm -rf ./build

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "  %-10s %s\n", $$1, $$2}'

.DEFAULT_GOAL := help
