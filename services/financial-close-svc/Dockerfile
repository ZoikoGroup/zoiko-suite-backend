# ─── Stage 1: builder ─────────────────────────────────────────────────────────
# BUILD CONTEXT IS services/, NOT THIS DIRECTORY:
#   docker build -f financial-close-svc/Dockerfile services/
# go.mod replaces zoiko.io/eventing with ../eventing (the shared event
# envelope + outbox module), and Docker can only COPY from inside the build
# context. Dockerfile.dockerignore next to this file trims the context to the
# two directories the build reads.

FROM golang:1.25-alpine AS builder

RUN apk add --no-cache git ca-certificates

WORKDIR /build/financial-close-svc

# Copy dependency manifests first to leverage Docker layer caching:
# if go.mod / go.sum are unchanged the module download layer is reused.
# The replaced module must be present before go mod download can resolve it.
COPY eventing/ /build/eventing/
COPY financial-close-svc/go.mod financial-close-svc/go.sum ./
RUN go mod download

# Copy the rest of the source.
COPY financial-close-svc/ ./

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags="-s -w" \
    -o /financial-close-svc \
    ./cmd/server/

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags="-s -w" \
    -o /healthcheck \
    ./cmd/healthcheck/

# ─── Stage 2: runtime ─────────────────────────────────────────────────────────
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=builder /financial-close-svc /financial-close-svc
COPY --from=builder /healthcheck /healthcheck

USER nonroot:nonroot

EXPOSE 8104

ENTRYPOINT ["/financial-close-svc"]
