# ─── Stage 1: builder ─────────────────────────────────────────────────────────
# BUILT FROM THE REPOSITORY ROOT:
#
#   docker build -f services/workflow-svc/Dockerfile .
#
# go.mod replaces zoiko.io/identity-envelope with the sibling module
# ../identity-envelope, so the build must see both. With the service folder as
# the context (as before 21 Oct 2026) that path does not exist and the build
# fails "replacement directory ../identity-envelope does not exist".
# Dockerfile.dockerignore keeps the root context to the two modules this image
# is built from.
#
# Use the exact Go toolchain version from go.mod (go 1.25).
# The builder image produces a statically-linked binary so it runs on distroless.
FROM golang:1.25-alpine AS builder

# Install git for modules that fetch via VCS (e.g. some indirect deps).
# ca-certificates is needed for HTTPS module downloads.
RUN apk add --no-cache git ca-certificates

# The same relative layout as the repository, so the replace path resolves.
WORKDIR /src/services/workflow-svc

# Copy dependency manifests first to leverage Docker layer caching:
# if go.mod / go.sum are unchanged the module download layer is reused.
COPY services/workflow-svc/go.mod services/workflow-svc/go.sum ./
COPY services/identity-envelope/ /src/services/identity-envelope/
RUN go mod download

# Copy the rest of the source.
COPY services/workflow-svc/ ./

# Build a statically-linked binary.
# CGO_ENABLED=0     — pure Go; no libc dependency, runs on distroless/static.
# -trimpath         — strips host filesystem paths from the binary.
# -ldflags "-s -w"  — strip debug symbols, reduces image size by ~30%.
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags="-s -w" \
    -o /workflow-svc \
    ./cmd/server/

# Also build a tiny static healthcheck binary — distroless has no shell/wget,
# and a dynamically-linked binary (e.g. busybox) can't exec in this image at
# all since there's no libc to satisfy it. A CGO_ENABLED=0 Go binary works
# here because it's fully static, same as the service binary above.
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags="-s -w" \
    -o /healthcheck \
    ./cmd/healthcheck/

# ─── Stage 2: runtime ─────────────────────────────────────────────────────────
# gcr.io/distroless/static-debian12 has no shell, no package manager, and no
# libc — the smallest safe base for a statically-linked Go binary.
# It ships with ca-certificates so TLS to Postgres/Kafka works out of the box.
FROM gcr.io/distroless/static-debian12:nonroot

# Copy the compiled binaries from the builder stage.
COPY --from=builder /workflow-svc /workflow-svc
COPY --from=builder /healthcheck /healthcheck

# distroless/static:nonroot already runs as uid 65532 (nonroot).
# Declaring USER is explicit documentation and enforces the intent.
USER nonroot:nonroot

# Health probe port (matches config.Load() PORT default = 8090).
# Override with -e PORT=<n> when running the container.
EXPOSE 8090

ENTRYPOINT ["/workflow-svc"]
