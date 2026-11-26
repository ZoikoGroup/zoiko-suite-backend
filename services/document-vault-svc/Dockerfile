# BUILD CONTEXT IS services/, NOT THIS DIRECTORY:
#   docker build -f document-vault-svc/Dockerfile services/
# go.mod replaces zoiko.io/eventing with ../eventing (the shared event
# envelope + outbox module), and Docker can only COPY from inside the build
# context. Dockerfile.dockerignore next to this file trims the context to
# the two directories the build reads.

# ─── Stage 1: builder ─────────────────────────────────────────────────────────
FROM golang:1.25-alpine AS builder

RUN apk add --no-cache git ca-certificates

# The service sits at /build/document-vault-svc so the go.mod replace
# directive's ../eventing resolves to /build/eventing.
WORKDIR /build/document-vault-svc

COPY eventing/ /build/eventing/
COPY document-vault-svc/go.mod document-vault-svc/go.sum ./
RUN go mod download

COPY document-vault-svc/ ./

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags="-s -w" -o /document-vault-svc ./cmd/server/
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags="-s -w" -o /healthcheck ./cmd/healthcheck/

# ─── Stage 2: runtime ─────────────────────────────────────────────────────────
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=builder /document-vault-svc /document-vault-svc
COPY --from=builder /healthcheck /healthcheck

USER nonroot:nonroot

# Health probe port (matches config.Load() PORT default = 8094).
EXPOSE 8094

ENTRYPOINT ["/document-vault-svc"]
