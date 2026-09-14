.PHONY: build check test test-race vet qualify-single-host benchmark-single-host

build:
	mkdir -p bin
	go build -trimpath -o bin/runtime-api ./cmd/runtime-api
	go build -trimpath -o bin/runtime-conformance ./cmd/runtime-conformance
	go build -trimpath -o bin/runtime-benchmark ./cmd/runtime-benchmark
	go build -trimpath -o bin/sandbox-bench ./cmd/sandbox-bench
	go build -trimpath -o bin/runtimectl ./cmd/runtimectl

test:
	go test ./...

test-race:
	go test -race ./...

vet:
	go vet ./...

check: test test-race vet build
	git diff --check

# Destructive: installs the pinned engine and creates real microVM resources.
# Requires a dedicated Linux/amd64 host with KVM and /dev/net/tun.
qualify-single-host:
	./deploy/single-host/install.sh

# Destructive: runs 24 real-microVM benchmark cells and preserves raw evidence.
# See docs/BENCHMARKING.md for required identity and execution variables.
benchmark-single-host: build
	./deploy/single-host/benchmark.sh
