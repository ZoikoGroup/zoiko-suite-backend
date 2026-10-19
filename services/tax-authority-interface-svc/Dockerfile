# BUILD CONTEXT IS services/, NOT THIS DIRECTORY:
#   docker build -f tax-authority-interface-svc/Dockerfile services/
# go.mod replaces zoiko.io/eventing with ../eventing (outbox, provider-adapter framework, signature),
# and Docker can only COPY from inside the build context. Dockerfile.dockerignore next to this file
# trims the context to the two directories the build reads.
FROM golang:1.25-alpine AS builder
WORKDIR /build/tax-authority-interface-svc
COPY eventing/ /build/eventing/
COPY tax-authority-interface-svc/go.mod tax-authority-interface-svc/go.sum ./
RUN go mod download
COPY tax-authority-interface-svc/ ./
RUN CGO_ENABLED=0 GOOS=linux go build -o /server ./cmd/server
RUN CGO_ENABLED=0 GOOS=linux go build -o /healthcheck ./cmd/healthcheck

FROM alpine:3.19
RUN apk add --no-cache ca-certificates tzdata
WORKDIR /app
COPY --from=builder /server /app/server
COPY --from=builder /healthcheck /app/healthcheck
EXPOSE 8147
ENTRYPOINT ["/app/server"]
