# BUILD CONTEXT IS services/, NOT THIS DIRECTORY:
#   docker build -f commercial-account-svc/Dockerfile services/
# go.mod replaces zoiko.io/eventing with ../eventing (the shared event
# envelope + outbox module), and Docker can only COPY from inside the build
# context. Dockerfile.dockerignore next to this file trims the context to the
# two directories the build reads. Same layout as capability-registry-svc.
FROM golang:1.25-alpine AS builder

# The service sits at /build/commercial-account-svc so the go.mod replace
# directive's ../eventing resolves to /build/eventing.
WORKDIR /build/commercial-account-svc

COPY eventing/ /build/eventing/
COPY commercial-account-svc/go.mod commercial-account-svc/go.sum ./
RUN go mod download

COPY commercial-account-svc/ ./

RUN CGO_ENABLED=0 GOOS=linux go build -o /server ./cmd/server
RUN CGO_ENABLED=0 GOOS=linux go build -o /healthcheck ./cmd/healthcheck

FROM alpine:3.19

RUN apk --no-cache add ca-certificates

WORKDIR /app

COPY --from=builder /server /app/server
COPY --from=builder /healthcheck /app/healthcheck

EXPOSE 8144

ENTRYPOINT ["/app/server"]
