# syntax=docker/dockerfile:1.7@sha256:a57df69d0ea827fb7266491f2813635de6f17269be881f696fbfdf2d83dda33e
FROM golang:1.23-alpine@sha256:383395b794dffa5b53012a212365d40c8e37109a626ca30d6151c8348d380b5f AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/runtime-api ./cmd/runtime-api \
    && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/runtime-conformance ./cmd/runtime-conformance \
    && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/runtime-benchmark ./cmd/runtime-benchmark \
    && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/runtimectl ./cmd/runtimectl

FROM alpine:3.22@sha256:14358309a308569c32bdc37e2e0e9694be33a9d99e68afb0f5ff33cc1f695dce
RUN addgroup -S runtime \
    && adduser -S -G runtime runtime
COPY --from=build /out/runtime-api /usr/local/bin/runtime-api
COPY --from=build /out/runtime-conformance /usr/local/bin/runtime-conformance
COPY --from=build /out/runtime-benchmark /usr/local/bin/runtime-benchmark
COPY --from=build /out/runtimectl /usr/local/bin/runtimectl
USER runtime:runtime
ENTRYPOINT ["/usr/local/bin/runtime-api"]
