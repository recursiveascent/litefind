.PHONY: build test lint check clean

build:
	go build -o build/litefind .

test:
	go test ./...

lint:
	go vet ./...

check: test lint
	go fix -d ./...
	golangci-lint run ./...

clean:
	-rm -rf ./build
