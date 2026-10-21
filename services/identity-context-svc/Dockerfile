# BUILD CONTEXT IS services/, NOT THIS DIRECTORY:
#   docker build -f identity-context-svc/Dockerfile services/
# go.mod replaces zoiko.io/eventing with ../eventing (the shared egress guard INT-10 fetches tenant IdP keys
# through), and Docker can only COPY from inside the build context. Dockerfile.dockerignore next to this
# file trims the context to the two directories the build reads.
# ─── Stage 1: builder ─────────────────────────────────────────────────────────
# BUILT FROM THE REPOSITORY ROOT:
#
#   docker build -f services/identity-context-svc/Dockerfile .
#
# go.mod replaces zoiko.io/identity-envelope with the sibling module
# ../identity-envelope, so the build must see both. With the service folder as
# the context (as before 19 Oct 2026) that path does not exist and the build
# fails "replacement directory ../identity-envelope does not exist" (Group 1
# re-audit F-4). Dockerfile.dockerignore keeps the root context to the two
# modules this image is built from.
#
# The builder produces a statically-linked binary so it runs on distroless.
FROM golang:1.25-alpine AS builder

# Install git for modules that fetch via VCS (e.g. some indirect deps).
# ca-certificates is needed for HTTPS module downloads.
RUN apk add --no-cache git ca-certificates

# The same relative layout as the repository, so the replace path resolves.
WORKDIR /src/services/identity-context-svc

# Copy dependency manifests first to leverage Docker layer caching:
# if go.mod / go.sum are unchanged the module download layer is reused.
COPY services/identity-context-svc/go.mod services/identity-context-svc/go.sum ./
COPY services/identity-envelope/ /src/services/identity-envelope/
COPY services/eventing/ /src/services/eventing/
RUN go mod download

# Copy the rest of the source.
COPY services/identity-context-svc/ ./

# Build a statically-linked binary.
# CGO_ENABLED=0     — pure Go; no libc dependency, runs on distroless/static.
# -trimpath         — strips host filesystem paths from the binary.
# -ldflags "-s -w"  — strip debug symbols, reduces image size by ~30%.
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags="-s -w" \
    -o /identity-context-svc \
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
# It ships with ca-certificates so TLS to Postgres/Redis works out of the box.
FROM gcr.io/distroless/static-debian12:nonroot

# Copy the compiled binary from the builder stage.
COPY --from=builder /identity-context-svc /identity-context-svc
COPY --from=builder /healthcheck /healthcheck

# distroless/static:nonroot already runs as uid 65532 (nonroot).
# Declaring USER is explicit documentation and enforces the intent.
USER nonroot:nonroot

# Health probe port (matches config.Load() PORT default = 8080).
# Override with -e PORT=<n> when running the container.
EXPOSE 8080

ENTRYPOINT ["/identity-context-svc"]
