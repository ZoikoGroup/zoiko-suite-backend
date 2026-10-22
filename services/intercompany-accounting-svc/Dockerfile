# BUILD CONTEXT IS services/, NOT THIS DIRECTORY:
#   docker build -f intercompany-accounting-svc/Dockerfile services/
# go.mod replaces zoiko.io/eventing with ../eventing (the shared event
# envelope + outbox module), and Docker can only COPY from inside the build
# context. Dockerfile.dockerignore next to this file trims the context to the
# two directories the build reads.

# ─── Stage 1: builder ─────────────────────────────────────────────────────────
FROM golang:1.25-alpine AS builder

RUN apk add --no-cache git ca-certificates

# The service sits at /build/intercompany-accounting-svc so the go.mod
# replace directive's ../eventing resolves to /build/eventing.
WORKDIR /build/intercompany-accounting-svc

COPY eventing/ /build/eventing/
COPY intercompany-accounting-svc/go.mod intercompany-accounting-svc/go.sum ./
RUN go mod download

COPY intercompany-accounting-svc/ ./

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags="-s -w" \
    -o /intercompany-accounting-svc \
    ./cmd/server/

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags="-s -w" \
    -o /healthcheck \
    ./cmd/healthcheck/

# ─── Stage 2: runtime ─────────────────────────────────────────────────────────
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=builder /intercompany-accounting-svc /intercompany-accounting-svc
COPY --from=builder /healthcheck /healthcheck

USER nonroot:nonroot

EXPOSE 8105

ENTRYPOINT ["/intercompany-accounting-svc"]
