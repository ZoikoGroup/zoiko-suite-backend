# ─── Stage 1: builder ─────────────────────────────────────────────────────────
# BUILT FROM THE REPOSITORY ROOT:
#
#   docker build -f services/gateway-auth-svc/Dockerfile .
#
# go.mod replaces zoiko.io/identity-envelope (and any other sibling module
# copied below) with ../<module>, so the build must see them; with the service
# folder as the context those paths do not exist (Group 1 re-audit F-4).
# Dockerfile.dockerignore keeps the root context to the modules listed here.
FROM golang:1.25-alpine AS builder

RUN apk add --no-cache git ca-certificates

# The same relative layout as the repository, so the replace paths resolve.
WORKDIR /src/services/gateway-auth-svc

COPY services/gateway-auth-svc/go.mod services/gateway-auth-svc/go.sum ./
COPY services/identity-envelope/ /src/services/identity-envelope/
RUN go mod download

COPY services/gateway-auth-svc/ ./

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o /gateway-auth-svc ./cmd/server/

# distroless has no shell or wget; a static Go probe binary works there.
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o /healthcheck ./cmd/healthcheck/

# ─── Stage 2: runtime ─────────────────────────────────────────────────────────
FROM alpine:3.20

RUN addgroup -g 65532 nonroot && adduser -u 65532 -G nonroot -D nonroot

COPY --from=builder /gateway-auth-svc /gateway-auth-svc
COPY --from=builder /healthcheck /healthcheck
COPY services/gateway-auth-svc/entrypoint.sh /entrypoint.sh

RUN chmod +x /entrypoint.sh && apk add --no-cache openssl su-exec

# Run as root to provision the token file, then drop to nonroot
EXPOSE 8092

ENTRYPOINT ["/entrypoint.sh"]
