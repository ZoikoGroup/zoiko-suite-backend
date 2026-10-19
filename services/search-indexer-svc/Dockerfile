# syntax=docker/dockerfile:1
# ── Stage 1: builder ────────────────────────────────────────────────────────
FROM golang:1.25-alpine AS builder

RUN apk add --no-cache git ca-certificates

WORKDIR /src

# The service's go.mod replaces zoiko.io/search-client with the sibling
# module, so this Dockerfile is built from the repository root.
COPY services/search-indexer-svc/go.mod services/search-indexer-svc/go.sum ./
COPY services/search-client/ /search-client/
RUN go mod download

COPY services/search-indexer-svc/ ./

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags="-s -w" \
    -o /search-indexer-svc \
    ./cmd/server/

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags="-s -w" \
    -o /healthcheck \
    ./cmd/healthcheck/

# ── Stage 2: runtime ────────────────────────────────────────────────────────
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=builder /search-indexer-svc /search-indexer-svc
COPY --from=builder /healthcheck /healthcheck

USER nonroot:nonroot

EXPOSE 8096

ENTRYPOINT ["/search-indexer-svc"]
