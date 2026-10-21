# BUILD CONTEXT IS services/, NOT THIS DIRECTORY:
#   docker build -f reconciliation-intelligence-svc/Dockerfile services/
# go.mod replaces zoiko.io/eventing with ../eventing (the shared event
# envelope + outbox module), and Docker can only COPY from inside the build
# context. Dockerfile.dockerignore next to this file trims the context to the
# two directories the build reads.

FROM golang:1.25-alpine AS builder

RUN apk add --no-cache git gcc musl-dev

# The service sits at /app/reconciliation-intelligence-svc so the go.mod
# replace directive's ../eventing resolves to /app/eventing.
WORKDIR /app/reconciliation-intelligence-svc

COPY eventing/ /app/eventing/
COPY reconciliation-intelligence-svc/go.mod reconciliation-intelligence-svc/go.sum ./
RUN go mod download

COPY reconciliation-intelligence-svc/ ./

RUN CGO_ENABLED=0 GOOS=linux go build -o /app/server ./cmd/server
RUN CGO_ENABLED=0 GOOS=linux go build -o /app/healthcheck ./cmd/healthcheck

FROM alpine:3.19

RUN apk add --no-cache ca-certificates tzdata

WORKDIR /app

COPY --from=builder /app/server /app/server
COPY --from=builder /app/healthcheck /app/healthcheck

EXPOSE 8137

HEALTHCHECK --interval=10s --timeout=5s --start-period=5s --retries=3 \
  CMD ["/app/healthcheck"]

CMD ["/app/server"]
