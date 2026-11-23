# BUILD CONTEXT IS services/, NOT THIS DIRECTORY:
#   docker build -f inventory-management-svc/Dockerfile services/
# go.mod replaces zoiko.io/eventing with ../eventing (the shared event envelope and
# transactional outbox), and Docker can only COPY from inside the build context.
# Dockerfile.dockerignore next to this file trims the context to the directories the build reads.

# ─── Stage 1: builder ─────────────────────────────────────────────────────────
FROM golang:1.25-alpine AS builder

RUN apk add --no-cache git ca-certificates

# The service sits at /build/inventory-management-svc so the go.mod replace
# directive's ../eventing resolves to /build/eventing.
WORKDIR /build/inventory-management-svc

COPY eventing/ /build/eventing/
COPY inventory-management-svc/go.mod inventory-management-svc/go.sum ./
RUN go mod download

COPY inventory-management-svc/ ./

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags="-s -w" \
    -o /inventory-management-svc \
    ./cmd/server/

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags="-s -w" \
    -o /healthcheck \
    ./cmd/healthcheck/

# ─── Stage 2: runtime ─────────────────────────────────────────────────────────
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=builder /inventory-management-svc /inventory-management-svc
COPY --from=builder /healthcheck /healthcheck

USER nonroot:nonroot

EXPOSE 8168

ENTRYPOINT ["/inventory-management-svc"]
