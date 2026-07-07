# ─── Stage 1: builder ─────────────────────────────────────────────────────────
# Use the exact Go toolchain version from go.mod (go 1.22).
# The builder image produces a statically-linked binary so it runs on distroless.
FROM golang:1.22-alpine AS builder

# Install git for modules that fetch via VCS (e.g. some indirect deps).
# ca-certificates is needed for HTTPS module downloads.
RUN apk add --no-cache git ca-certificates

WORKDIR /build

# Copy dependency manifests first to leverage Docker layer caching:
# if go.mod / go.sum are unchanged the module download layer is reused.
COPY go.mod go.sum ./
RUN go mod download

# Copy the rest of the source.
COPY . .

# Build a statically-linked binary.
# CGO_ENABLED=0     — pure Go; no libc dependency, runs on distroless/static.
# -trimpath         — strips host filesystem paths from the binary.
# -ldflags "-s -w"  — strip debug symbols, reduces image size by ~30%.
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags="-s -w" \
    -o /identity-context-svc \
    ./cmd/server/

# ─── Stage 2: runtime ─────────────────────────────────────────────────────────
# gcr.io/distroless/static-debian12 has no shell, no package manager, and no
# libc — the smallest safe base for a statically-linked Go binary.
# It ships with ca-certificates so TLS to Postgres/Redis works out of the box.
FROM gcr.io/distroless/static-debian12:nonroot

# Copy the compiled binary from the builder stage.
COPY --from=builder /identity-context-svc /identity-context-svc

# distroless/static:nonroot already runs as uid 65532 (nonroot).
# Declaring USER is explicit documentation and enforces the intent.
USER nonroot:nonroot

# Health probe port (matches config.Load() PORT default = 8080).
# Override with -e PORT=<n> when running the container.
EXPOSE 8080

ENTRYPOINT ["/identity-context-svc"]
