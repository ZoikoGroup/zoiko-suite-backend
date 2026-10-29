# BUILT FROM THE REPOSITORY ROOT:
#
#   docker build -f services/key-management-svc/Dockerfile .
#
# go.mod replaces zoiko.io/identity-envelope with ../identity-envelope, so the
# build must see that sibling module; with the service folder as the context
# the path does not exist. Dockerfile.dockerignore keeps the root context to
# the modules listed here.
FROM golang:1.25-alpine AS builder
RUN apk add --no-cache git gcc musl-dev
# The same relative layout as the repository, so the replace path resolves.
WORKDIR /src/services/key-management-svc
COPY services/key-management-svc/go.mod services/key-management-svc/go.sum ./
COPY services/identity-envelope/ /src/services/identity-envelope/
RUN go mod download
COPY services/key-management-svc/ ./
RUN CGO_ENABLED=0 GOOS=linux go build -o /app/server ./cmd/server
RUN CGO_ENABLED=0 GOOS=linux go build -o /app/healthcheck ./cmd/healthcheck

FROM alpine:3.19
RUN apk add --no-cache ca-certificates tzdata
WORKDIR /app
COPY --from=builder /app/server /app/server
COPY --from=builder /app/healthcheck /app/healthcheck
EXPOSE 8143
HEALTHCHECK --interval=10s --timeout=5s --start-period=5s --retries=3 CMD ["/app/healthcheck"]
CMD ["/app/server"]
