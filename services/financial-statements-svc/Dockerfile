# BUILD CONTEXT IS services/, NOT THIS DIRECTORY:
#   docker build -f financial-statements-svc/Dockerfile services/
# go.mod replaces zoiko.io/eventing with ../eventing (the shared event
# envelope + outbox module), and Docker can only COPY from inside the build
# context. Dockerfile.dockerignore next to this file trims the context to
# the two directories the build reads.

FROM golang:1.26-alpine AS builder

RUN apk add --no-cache git ca-certificates

# The service sits at /build/financial-statements-svc so the go.mod replace
# directive's ../eventing resolves to /build/eventing.
WORKDIR /build/financial-statements-svc

COPY eventing/ /build/eventing/
COPY financial-statements-svc/go.mod financial-statements-svc/go.sum ./
RUN go mod download

COPY financial-statements-svc/ ./

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags="-s -w" \
    -o /financial-statements-svc \
    ./cmd/server/

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags="-s -w" \
    -o /healthcheck \
    ./cmd/healthcheck/

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=builder /financial-statements-svc /financial-statements-svc
COPY --from=builder /healthcheck /healthcheck

USER nonroot:nonroot

EXPOSE 8175

ENTRYPOINT ["/financial-statements-svc"]
