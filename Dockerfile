# syntax=docker/dockerfile:1.7@sha256:a57df69d0ea827fb7266491f2813635de6f17269be881f696fbfdf2d83dda33e
FROM golang:1.26.6-alpine@sha256:3889b425f035be855a72fb4755265311293b6d414521f0a519d819df32222d83 AS build
ARG BREZEL_VERSION=dev
ARG BREZEL_REVISION=unknown
ARG BREZEL_BUILT_AT=unknown
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN build_flags="-s -w -X github.com/infercrane/brezel/internal/buildinfo.Version=${BREZEL_VERSION} -X github.com/infercrane/brezel/internal/buildinfo.Revision=${BREZEL_REVISION} -X github.com/infercrane/brezel/internal/buildinfo.BuiltAt=${BREZEL_BUILT_AT}" \
    && CGO_ENABLED=0 go build -trimpath -buildvcs=false -ldflags="$build_flags" -o /out/brezeld ./cmd/brezeld \
    && CGO_ENABLED=0 go build -trimpath -buildvcs=false -ldflags="$build_flags" -o /out/brezel-node ./cmd/brezel-node \
    && CGO_ENABLED=0 go build -trimpath -buildvcs=false -ldflags="$build_flags" -o /out/brezel-conformance ./cmd/brezel-conformance \
    && CGO_ENABLED=0 go build -trimpath -buildvcs=false -ldflags="$build_flags" -o /out/brezel-bench ./cmd/brezel-bench \
    && CGO_ENABLED=0 go build -trimpath -buildvcs=false -ldflags="$build_flags" -o /out/brezel-bench-compare ./cmd/brezel-bench-compare \
    && CGO_ENABLED=0 go build -trimpath -buildvcs=false -ldflags="$build_flags" -o /out/brezel ./cmd/brezel

FROM alpine:3.24@sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b
ARG BREZEL_VERSION=dev
ARG BREZEL_REVISION=unknown
ARG BREZEL_BUILT_AT=unknown
LABEL org.opencontainers.image.title="Brezel" \
      org.opencontainers.image.description="Self-hosted Firecracker sandboxes for agents" \
      org.opencontainers.image.source="https://github.com/infercrane/brezel" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.version="$BREZEL_VERSION" \
      org.opencontainers.image.revision="$BREZEL_REVISION" \
      org.opencontainers.image.created="$BREZEL_BUILT_AT"
RUN addgroup -S brezel \
    && adduser -S -G brezel brezel
COPY --from=build /out/brezeld /usr/local/bin/brezeld
COPY --from=build /out/brezel-node /usr/local/bin/brezel-node
COPY --from=build /out/brezel-conformance /usr/local/bin/brezel-conformance
COPY --from=build /out/brezel-bench /usr/local/bin/brezel-bench
COPY --from=build /out/brezel-bench-compare /usr/local/bin/brezel-bench-compare
COPY --from=build /out/brezel /usr/local/bin/brezel
USER brezel:brezel
ENTRYPOINT ["/usr/local/bin/brezeld"]
