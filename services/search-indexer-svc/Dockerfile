# syntax=docker/dockerfile:1

# ── Stage 1: build ───────────────────────────────────────────────────────────
FROM golang:1.22-alpine AS builder

WORKDIR /workspace

# Copy search-client and search-indexer-svc keeping directory structure
COPY services/search-client ./services/search-client
COPY services/search-indexer-svc ./services/search-indexer-svc

WORKDIR /workspace/services/search-indexer-svc

RUN go mod download

# Build the main server binary
RUN CGO_ENABLED=0 GOOS=linux go build \
    -ldflags="-s -w" \
    -o /out/server \
    ./cmd/server

# Build the healthcheck helper
RUN CGO_ENABLED=0 GOOS=linux go build \
    -ldflags="-s -w" \
    -o /out/healthcheck \
    ./cmd/healthcheck

# ── Stage 2: runtime (distroless, non-root) ──────────────────────────────────
FROM gcr.io/distroless/static:nonroot

COPY --from=builder /out/server /server
COPY --from=builder /out/healthcheck /healthcheck

USER nonroot:nonroot

EXPOSE 8094

ENTRYPOINT ["/server"]
