# BUILD CONTEXT IS services/, NOT THIS DIRECTORY:
#   docker build -f comments-collaboration-svc/Dockerfile services/
# go.mod replaces zoiko.io/eventing with ../eventing (the shared event envelope and
# transactional outbox), and Docker can only COPY from inside the build context.
# Dockerfile.dockerignore next to this file trims the context to the directories the build reads.
FROM golang:1.25-alpine AS builder

# The service sits at /build/comments-collaboration-svc so the go.mod replace
# directive's ../eventing resolves to /build/eventing.
WORKDIR /build/comments-collaboration-svc

COPY eventing/ /build/eventing/
COPY comments-collaboration-svc/go.mod comments-collaboration-svc/go.sum ./
RUN go mod download

COPY comments-collaboration-svc/ ./

RUN CGO_ENABLED=0 GOOS=linux go build -o /server ./cmd/server
RUN CGO_ENABLED=0 GOOS=linux go build -o /healthcheck ./cmd/healthcheck

FROM alpine:3.19

RUN apk --no-cache add ca-certificates

WORKDIR /app

COPY --from=builder /server /app/server
COPY --from=builder /healthcheck /app/healthcheck

EXPOSE 8148

ENTRYPOINT ["/app/server"]
