# ─── Stage 1: builder ─────────────────────────────────────────────────────────
# Use the exact Go toolchain version from go.mod (go 1.25).
# The builder image produces statically-linked binaries so they run on distroless.
FROM golang:1.25-alpine AS builder

RUN apk add --no-cache git ca-certificates

WORKDIR /build

COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Build both binaries: the server and the healthcheck probe used by this
# image's HEALTHCHECK instruction below (context.md §7.8 — distroless has
# no shell/curl/wget for a HEALTHCHECK to invoke, so the probe itself has
# to be a compiled Go binary).
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags="-s -w" \
    -o /secret-vault-integration-svc \
    ./cmd/server/
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags="-s -w" \
    -o /healthcheck \
    ./cmd/healthcheck/

# ─── Stage 2: runtime ─────────────────────────────────────────────────────────
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=builder /secret-vault-integration-svc /secret-vault-integration-svc
COPY --from=builder /healthcheck /healthcheck

USER nonroot:nonroot

# Health probe port (matches config.Load() PORT default = 8087).
EXPOSE 8087

# First HEALTHCHECK instruction in this repo (context.md §7.8) — every
# other service here relies on an external prober hitting /healthz
# directly; this one also declares it at the image level since
# distroless has nothing else capable of running one internally.
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
    CMD ["/healthcheck"]

ENTRYPOINT ["/secret-vault-integration-svc"]
