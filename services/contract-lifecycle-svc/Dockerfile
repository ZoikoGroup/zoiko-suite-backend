# BUILT FROM THE REPOSITORY ROOT:
#
#   docker build -f services/contract-lifecycle-svc/Dockerfile .
#
# go.mod replaces zoiko.io/identity-envelope with ../identity-envelope (the
# shared mTLS client the decision-log reads use, D2-2.F07), so the build must
# see both modules. Dockerfile.dockerignore keeps the root context to them.
FROM golang:1.25-alpine AS builder

# The same relative layout as the repository, so the replace path resolves.
WORKDIR /src/services/contract-lifecycle-svc

COPY services/contract-lifecycle-svc/go.mod services/contract-lifecycle-svc/go.sum ./
COPY services/identity-envelope/ /src/services/identity-envelope/
RUN go mod download

COPY services/contract-lifecycle-svc/ ./

RUN CGO_ENABLED=0 GOOS=linux go build -o /server ./cmd/server
RUN CGO_ENABLED=0 GOOS=linux go build -o /healthcheck ./cmd/healthcheck

FROM alpine:3.19

RUN apk --no-cache add ca-certificates

WORKDIR /app

COPY --from=builder /server /app/server
COPY --from=builder /healthcheck /app/healthcheck

EXPOSE 8119

ENTRYPOINT ["/app/server"]
