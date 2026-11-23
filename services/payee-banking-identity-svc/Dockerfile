# BUILD CONTEXT IS services/, NOT THIS DIRECTORY:
#   docker build -f payee-banking-identity-svc/Dockerfile services/
# go.mod replaces zoiko.io/eventing with ../eventing (the shared event
# envelope and transactional outbox module), and Docker can only COPY from
# inside the build context. Dockerfile.dockerignore next to this file trims
# the context to the two directories the build reads.
#
# Found running this service for real under docker-compose.yml (tokenization
# enforcement live test): the previous version of this file assumed a context
# of just this directory (COPY go.mod go.sum ./ then COPY . .), which cannot
# resolve the eventing replace directive at all since Docker never sees
# outside its build context -- "go mod download" failed with "reading
# /eventing/go.mod: open /eventing/go.mod: no such file or directory". The
# same bug, and the same fix, as banking-connector-svc/Dockerfile.
FROM golang:1.25-alpine AS builder

# The service sits at /build/payee-banking-identity-svc so the go.mod replace
# directive's ../eventing resolves to /build/eventing.
WORKDIR /build/payee-banking-identity-svc

# The replaced module must be present before go mod download can resolve it.
COPY eventing/ /build/eventing/
COPY payee-banking-identity-svc/go.mod payee-banking-identity-svc/go.sum ./
RUN go mod download

COPY payee-banking-identity-svc/ ./

RUN CGO_ENABLED=0 GOOS=linux go build -o /server ./cmd/server
RUN CGO_ENABLED=0 GOOS=linux go build -o /healthcheck ./cmd/healthcheck

FROM alpine:3.19

RUN apk --no-cache add ca-certificates

WORKDIR /app

COPY --from=builder /server /app/server
COPY --from=builder /healthcheck /app/healthcheck

EXPOSE 8166

ENTRYPOINT ["/app/server"]
