.PHONY: build test lint

build:
	go build -o bin/malachi ./cmd/malachi

test:
	go test ./...

lint:
	go vet ./...
