ARG GOLANG_VERSION=1.26.8
ARG DEBIAN_VERSION=bookworm

FROM golang:${GOLANG_VERSION}-${DEBIAN_VERSION} AS builder
ARG TARGETARCH
ARG COMMIT_SHA
ARG VERSION

WORKDIR /build/shared
COPY ./shared/go.mod ./shared/go.sum ./
RUN go mod download

WORKDIR /build/envd
COPY ./envd/go.mod ./envd/go.sum ./
RUN go mod download

WORKDIR /build
COPY ./shared/pkg ./shared/pkg
COPY ./envd ./envd

WORKDIR /build/envd
RUN --mount=type=cache,target=/root/.cache/go-build \
    make build BUILD_ARCH=${TARGETARCH} BUILD=${COMMIT_SHA} LINK_VERSION=${VERSION}

FROM scratch
COPY --from=builder /build/envd/bin/envd /envd
ENTRYPOINT ["/envd"]
