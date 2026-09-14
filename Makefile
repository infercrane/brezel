.PHONY: build check test test-race vet qualify-single-host benchmark-single-host

build:
	mkdir -p bin
	go build -trimpath -o bin/brezeld ./cmd/brezeld
	go build -trimpath -o bin/brezel-conformance ./cmd/brezel-conformance
	go build -trimpath -o bin/brezel-bench ./cmd/brezel-bench
	go build -trimpath -o bin/brezel ./cmd/brezel

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
