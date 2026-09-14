# syntax=docker/dockerfile:1.7@sha256:a57df69d0ea827fb7266491f2813635de6f17269be881f696fbfdf2d83dda33e
FROM golang:1.23-alpine@sha256:383395b794dffa5b53012a212365d40c8e37109a626ca30d6151c8348d380b5f AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/brezeld ./cmd/brezeld \
    && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/brezel-conformance ./cmd/brezel-conformance \
    && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/brezel-bench ./cmd/brezel-bench \
    && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/brezel ./cmd/brezel

FROM alpine:3.22@sha256:14358309a308569c32bdc37e2e0e9694be33a9d99e68afb0f5ff33cc1f695dce
RUN addgroup -S brezel \
    && adduser -S -G brezel brezel
COPY --from=build /out/brezeld /usr/local/bin/brezeld
COPY --from=build /out/brezel-conformance /usr/local/bin/brezel-conformance
COPY --from=build /out/brezel-bench /usr/local/bin/brezel-bench
COPY --from=build /out/brezel /usr/local/bin/brezel
USER brezel:brezel
ENTRYPOINT ["/usr/local/bin/brezeld"]
