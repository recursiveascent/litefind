.PHONY: build test lint fmt

build:
	go build -o litefind .

test:
	go test ./...

lint:
	go vet ./...

fmt:
	gofmt -w .
