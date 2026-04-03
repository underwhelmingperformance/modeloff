.PHONY: build test lint fmt vet check

build:
	go build ./...

test:
	go test -race ./...

lint:
	golangci-lint run ./...

fmt:
	golangci-lint fmt ./...

vet:
	go vet ./...

check: build vet lint test
