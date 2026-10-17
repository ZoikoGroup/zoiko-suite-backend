# BUILD CONTEXT IS services/, NOT THIS DIRECTORY:
#   docker build -f payment-status-svc/Dockerfile services/
# go.mod replaces zoiko.io/eventing with ../eventing (outbox, provider-adapter framework, signature),
# and Docker can only COPY from inside the build context. Dockerfile.dockerignore next to this file
# trims the context to the two directories the build reads.
FROM golang:1.25-alpine AS builder

# The service sits at /build/payment-status-svc so the go.mod replace directive's ../eventing resolves to /build/eventing.
WORKDIR /build/payment-status-svc

COPY eventing/ /build/eventing/
COPY payment-status-svc/go.mod payment-status-svc/go.sum ./
RUN go mod download

COPY payment-status-svc/ ./
RUN CGO_ENABLED=0 GOOS=linux go build -o /server ./cmd/server
RUN CGO_ENABLED=0 GOOS=linux go build -o /healthcheck ./cmd/healthcheck

FROM alpine:3.19

RUN apk --no-cache add ca-certificates

WORKDIR /app

COPY --from=builder /server /app/server
COPY --from=builder /healthcheck /app/healthcheck

EXPOSE 8163

ENTRYPOINT ["/app/server"]
