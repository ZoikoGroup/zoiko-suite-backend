# syntax=docker/dockerfile:1

# ── Stage 1: build ───────────────────────────────────────────────────────────
FROM golang:1.22-alpine AS builder

WORKDIR /build

# Copy search-client (local replace directive in go.mod)
COPY ../search-client /search-client

# Copy this service's source
COPY . .

RUN go mod download

# Build the main server binary
RUN CGO_ENABLED=0 GOOS=linux go build \
    -ldflags="-s -w" \
    -o /out/server \
    ./cmd/server

# Build the healthcheck helper (same pattern as obligations-svc)
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
