.PHONY: build check test test-race vet

build:
	mkdir -p bin
	go build -trimpath -o bin/runtime-api ./cmd/runtime-api
	go build -trimpath -o bin/runtime-conformance ./cmd/runtime-conformance

test:
	go test ./...

test-race:
	go test -race ./...

vet:
	go vet ./...

check: test test-race vet build
	git diff --check
