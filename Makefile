.PHONY: build test lint check release clean help

build: ## Build litefind
	go build -o build/litefind .

test: ## Run tests
	go test ./...

lint: ## Run static analysis
	go vet ./...

check: test lint ## Run all checks
	go fix -diff ./...
	golangci-lint run ./...

release: ## Publish a GitHub release
	goreleaser release --clean

clean: ## Remove build artifacts
	-rm -rf ./build

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "  %-10s %s\n", $$1, $$2}'

.DEFAULT_GOAL := help
