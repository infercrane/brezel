.PHONY: build check fmt-check shell-syntax verify-engine-patches test test-integrations test-race vet qualify-single-host qualify-rootdevice-reflink benchmark-single-host benchmark-dax-local

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || printf dev)
REVISION ?= $(shell git rev-parse HEAD 2>/dev/null || printf unknown)
BUILT_AT ?= $(shell date -u '+%Y-%m-%dT%H:%M:%SZ')
GO_BUILD_FLAGS ?= -trimpath -buildvcs=true -ldflags "-X github.com/infercrane/brezel/internal/buildinfo.Version=$(VERSION) -X github.com/infercrane/brezel/internal/buildinfo.Revision=$(REVISION) -X github.com/infercrane/brezel/internal/buildinfo.BuiltAt=$(BUILT_AT)"

build:
	mkdir -p bin
	go build $(GO_BUILD_FLAGS) -o bin/brezeld ./cmd/brezeld
	go build $(GO_BUILD_FLAGS) -o bin/brezel-node ./cmd/brezel-node
	go build $(GO_BUILD_FLAGS) -o bin/brezel-conformance ./cmd/brezel-conformance
	go build $(GO_BUILD_FLAGS) -o bin/brezel-bench ./cmd/brezel-bench
	go build $(GO_BUILD_FLAGS) -o bin/brezel-bench-compare ./cmd/brezel-bench-compare
	go build $(GO_BUILD_FLAGS) -o bin/brezel ./cmd/brezel

fmt-check:
	@files="$$(find cmd internal spec deploy -type f -name '*.go' -print)"; \
	unformatted="$$(gofmt -l $$files)"; \
	if [ -n "$$unformatted" ]; then printf 'gofmt required:\n%s\n' "$$unformatted" >&2; exit 1; fi

shell-syntax:
	@find deploy benchmarks examples -type f -name '*.sh' -exec sh -c 'for file do case "$$(head -n 1 "$$file")" in *bash*) bash -n "$$file" ;; *) sh -n "$$file" ;; esac || exit 1; done' sh {} +

verify-engine-patches:
	./deploy/single-host/verify-engine-patch-chain.sh

test:
	go test ./...

test-integrations:
	python3 scripts/check-sdk-versions.py
	PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover -s sdk/python/tests -v
	node --check sdk/typescript/src/index.js
	node --test sdk/typescript/test/*.test.mjs
	node --test benchmarks/computesdk/*.test.mjs
	node --check benchmarks/computesdk/dax-bottleneck-model.mjs
	node --check benchmarks/computesdk/adapter.mjs
	node --check benchmarks/computesdk/provider-entry.mjs
	node --check benchmarks/computesdk/qualified-smoke.mjs
	node --check benchmarks/computesdk/dax-rehearsal.mjs
	node --check benchmarks/computesdk/dax-paired-ab.mjs
	node --check benchmarks/computesdk/burst-rehearsal.mjs

test-race:
	go test -race ./...

vet:
	go vet ./...

check: fmt-check shell-syntax test test-integrations test-race vet build
	bin/brezel version --json
	git diff --check

# Destructive: installs the pinned engine and creates real microVM resources.
# Requires a dedicated Linux/amd64 host with KVM and /dev/net/tun.
qualify-single-host:
	./deploy/single-host/install.sh

# Destructive and Linux-only: creates, fills, and removes a disposable 1 GiB
# loopback XFS filesystem. It never changes the production root-device default.
qualify-rootdevice-reflink:
	./deploy/qualification/rootdevice-reflink.sh

# Destructive: runs 24 real-microVM benchmark cells and preserves raw evidence.
# See docs/BENCHMARKING.md for required identity and execution variables.
benchmark-single-host: build
	./deploy/single-host/benchmark.sh

# Non-publishable local A/B lab for image and writable-root experiments. It
# executes the exact pinned upstream workload but does not emulate KVM/NUMA.
benchmark-dax-local:
	node benchmarks/computesdk/local-dax-lab.mjs --probe prepare --image baseline --iterations 3 --output /tmp/brezel-dax-local-baseline.json
	node benchmarks/computesdk/local-dax-lab.mjs --probe prepare --build-candidate --image candidate --iterations 3 --output /tmp/brezel-dax-local-candidate.json
