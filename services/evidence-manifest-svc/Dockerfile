# BUILT FROM THE REPOSITORY ROOT:
#
#   docker build -f services/evidence-manifest-svc/Dockerfile .
#
# go.mod replaces zoiko.io/identity-envelope with ../identity-envelope (the
# shared mTLS client the decision-log reads use, D2-2.F07), so the build must
# see both modules. Dockerfile.dockerignore keeps the root context to them.
FROM golang:1.25-alpine AS builder

RUN apk add --no-cache git ca-certificates

# The same relative layout as the repository, so the replace path resolves.
WORKDIR /src/services/evidence-manifest-svc

COPY services/evidence-manifest-svc/go.mod services/evidence-manifest-svc/go.sum ./
COPY services/identity-envelope/ /src/services/identity-envelope/
RUN go mod download

COPY services/evidence-manifest-svc/ ./

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags="-s -w" -o /evidence-manifest-svc ./cmd/server/
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags="-s -w" -o /healthcheck ./cmd/healthcheck/

# Runtime
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=builder /evidence-manifest-svc /evidence-manifest-svc
COPY --from=builder /healthcheck /healthcheck

USER nonroot:nonroot

EXPOSE 8095

ENTRYPOINT ["/evidence-manifest-svc"]
