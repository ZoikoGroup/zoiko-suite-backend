# BUILD CONTEXT IS services/, NOT THIS DIRECTORY:
#   docker build -f banking-connector-svc/Dockerfile services/
# go.mod replaces zoiko.io/eventing with ../eventing (the shared event envelope
# and transactional outbox module), and Docker can only COPY from inside the
# build context. Dockerfile.dockerignore next to this file trims the context to
# the two directories the build reads.
#
# Found running this service for real under docker-compose.phase7.yml
# (tokenization enforcement live test): the previous version of this file
# assumed a context of just this directory (COPY go.mod go.sum ./ then
# COPY . .), which cannot resolve the eventing replace directive at all since
# Docker never sees outside its build context -- "go mod download" failed with
# "reading /eventing/go.mod: open /eventing/go.mod: no such file or
# directory". The sibling connectivity-api-bridge-svc/Dockerfile already had
# the right shape; this copies it.
FROM golang:1.25-alpine AS builder

# The service sits at /build/banking-connector-svc so the go.mod replace
# directive's ../eventing resolves to /build/eventing.
WORKDIR /build/banking-connector-svc

# The replaced module must be present before go mod download can resolve it.
COPY eventing/ /build/eventing/
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
