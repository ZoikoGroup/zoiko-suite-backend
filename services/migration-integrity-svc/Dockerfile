# BUILD CONTEXT IS services/, NOT THIS DIRECTORY:
#   docker build -f migration-integrity-svc/Dockerfile services/
# go.mod replaces zoiko.io/eventing with ../eventing, and Docker can only
# COPY from inside the build context -- matching general-ledger-svc's
# Dockerfile, which has the same replace directive and the same build-context
# requirement.
#
# Previously this Dockerfile only had a runtime stage that COPYed a
# pre-built ./server and ./healthcheck from the build context, with nothing
# in this repository building them -- the service could not actually be
# built into an image. Added for the migration-integrity-svc <->
# general-ledger-svc wiring task, which needs to run this service as a real
# container alongside general-ledger-svc and authorization-svc.

# ─── Stage 1: builder ─────────────────────────────────────────────────────────
FROM golang:1.25-alpine AS builder

RUN apk add --no-cache git ca-certificates

WORKDIR /build/migration-integrity-svc

COPY eventing/ /build/eventing/
COPY migration-integrity-svc/go.mod migration-integrity-svc/go.sum ./
RUN go mod download

COPY migration-integrity-svc/ ./

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags="-s -w" \
    -o /server \
    ./cmd/server/

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags="-s -w" \
    -o /healthcheck \
    ./cmd/healthcheck/

# ─── Stage 2: runtime ─────────────────────────────────────────────────────────
FROM alpine:3.19

RUN apk add --no-cache ca-certificates tzdata

WORKDIR /app

COPY --from=builder /server /app/server
COPY --from=builder /healthcheck /app/healthcheck

RUN chmod +x /app/server /app/healthcheck

EXPOSE 8139

HEALTHCHECK --interval=10s --timeout=5s --start-period=5s --retries=3 \
  CMD ["/app/healthcheck"]

CMD ["/app/server"]
