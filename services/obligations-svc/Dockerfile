# ─── Stage 1: builder ─────────────────────────────────────────────────────────
# BUILT FROM THE REPOSITORY ROOT:
#
#   docker build -f services/obligations-svc/Dockerfile .
#
# go.mod replaces zoiko.io/identity-envelope with the sibling module
# ../identity-envelope, so the build must see both. Until 21 Oct 2026 this
# Dockerfile copied prebuilt, gitignored `server` and `healthcheck` binaries
# from the service folder, so the image could not be built from a clean
# checkout (the R-7 shape). Dockerfile.dockerignore keeps the root context to
# the two modules this image is built from.
FROM golang:1.25-alpine AS builder

RUN apk add --no-cache git ca-certificates

# The same relative layout as the repository, so the replace path resolves.
WORKDIR /src/services/obligations-svc

COPY services/obligations-svc/go.mod services/obligations-svc/go.sum ./
COPY services/identity-envelope/ /src/services/identity-envelope/
RUN go mod download

COPY services/obligations-svc/ ./

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags="-s -w" -o /obligations-svc ./cmd/server/

# distroless has no shell or wget; a static Go probe binary works there.
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags="-s -w" -o /healthcheck ./cmd/healthcheck/

# ─── Stage 2: runtime ─────────────────────────────────────────────────────────
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=builder /obligations-svc /obligations-svc
COPY --from=builder /healthcheck /healthcheck

USER nonroot:nonroot

EXPOSE 8088

ENTRYPOINT ["/obligations-svc"]
