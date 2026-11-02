# BUILD CONTEXT IS services/, NOT THIS DIRECTORY:
#   docker build -f banking-connector-svc/Dockerfile services/
# go.mod replaces zoiko.io/eventing with ../eventing, and Docker can only COPY
# from inside the build context. Dockerfile.dockerignore next to this file
# trims the context to the two directories the build reads.

FROM golang:1.25-alpine AS builder

RUN apk add --no-cache git

# The service sits at /app/banking-connector-svc so the replace directive's ../eventing
# resolves to /app/eventing.
WORKDIR /app/banking-connector-svc

COPY eventing/ /app/eventing/
COPY banking-connector-svc/go.mod banking-connector-svc/go.sum ./
RUN go mod download

COPY banking-connector-svc/ ./

RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /app/server ./cmd/server
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /app/healthcheck ./cmd/healthcheck

FROM alpine:3.19

RUN apk add --no-cache ca-certificates tzdata

WORKDIR /app

COPY --from=builder /app/server /app/server
COPY --from=builder /app/healthcheck /app/healthcheck

EXPOSE 8145

# cmd/healthcheck probes the public /healthz endpoint.
HEALTHCHECK --interval=10s --timeout=5s --start-period=10s --retries=3   CMD ["/app/healthcheck"]

ENTRYPOINT ["/app/server"]
