# BUILD CONTEXT IS services/, NOT THIS DIRECTORY:
#   docker build -f connectivity-api-bridge-svc/Dockerfile services/
# go.mod replaces zoiko.io/eventing with ../eventing (the shared event envelope
# and transactional outbox module), and Docker can only COPY from inside the
# build context. Dockerfile.dockerignore next to this file trims the context to
# the two directories the build reads.
FROM golang:1.25-alpine AS builder

# The service sits at /build/connectivity-api-bridge-svc so the go.mod replace
# directive's ../eventing resolves to /build/eventing.
WORKDIR /build/connectivity-api-bridge-svc

# The replaced module must be present before go mod download can resolve it.
COPY eventing/ /build/eventing/
COPY connectivity-api-bridge-svc/go.mod connectivity-api-bridge-svc/go.sum ./
RUN go mod download

COPY connectivity-api-bridge-svc/ ./
RUN CGO_ENABLED=0 GOOS=linux go build -o /server ./cmd/server
RUN CGO_ENABLED=0 GOOS=linux go build -o /healthcheck ./cmd/healthcheck

FROM alpine:3.19
RUN apk add --no-cache ca-certificates tzdata
WORKDIR /app
COPY --from=builder /server /app/server
COPY --from=builder /healthcheck /app/healthcheck
EXPOSE 8144
ENTRYPOINT ["/app/server"]
