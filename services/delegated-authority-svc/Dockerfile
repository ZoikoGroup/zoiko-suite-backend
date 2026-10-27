# ─── Stage 1: builder ─────────────────────────────────────────────────────────
# BUILT FROM THE REPOSITORY ROOT:
#
#   docker build -f services/delegated-authority-svc/Dockerfile .
#
# go.mod replaces zoiko.io/identity-envelope (and any other sibling module
# copied below) with ../<module>, so the build must see them; with the service
# folder as the context those paths do not exist (Group 1 re-audit F-4).
# Dockerfile.dockerignore keeps the root context to the modules listed here.
FROM golang:1.25-alpine AS builder

RUN apk add --no-cache git ca-certificates

# The same relative layout as the repository, so the replace paths resolve.
WORKDIR /src/services/delegated-authority-svc

COPY services/delegated-authority-svc/go.mod services/delegated-authority-svc/go.sum ./
COPY services/identity-envelope/ /src/services/identity-envelope/
RUN go mod download

COPY services/delegated-authority-svc/ ./

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o /delegated-authority-svc ./cmd/server/

# distroless has no shell or wget; a static Go probe binary works there.
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o /healthcheck ./cmd/healthcheck/

# ─── Stage 2: runtime ─────────────────────────────────────────────────────────
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=builder /delegated-authority-svc /delegated-authority-svc
COPY --from=builder /healthcheck /healthcheck

USER nonroot:nonroot

EXPOSE 8136

ENTRYPOINT ["/delegated-authority-svc"]
