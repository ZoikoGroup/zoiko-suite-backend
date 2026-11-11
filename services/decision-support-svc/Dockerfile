# BUILT FROM THE REPOSITORY ROOT:
#
#   docker build -f services/decision-support-svc/Dockerfile .
#
# go.mod replaces zoiko.io/identity-envelope with ../identity-envelope (the
# shared mTLS client the decision-log reads use, D2-2.F07), so the build must
# see both modules. Dockerfile.dockerignore keeps the root context to them.
FROM golang:1.25-alpine AS builder

RUN apk add --no-cache git ca-certificates

# The same relative layout as the repository, so the replace path resolves.
WORKDIR /src/services/decision-support-svc

COPY services/decision-support-svc/go.mod services/decision-support-svc/go.sum ./
COPY services/identity-envelope/ /src/services/identity-envelope/
RUN go mod download

COPY services/decision-support-svc/ ./

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags="-s -w" \
    -o /decision-support-svc \
    ./cmd/server/

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags="-s -w" \
    -o /healthcheck \
    ./cmd/healthcheck/

# Runtime
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=builder /decision-support-svc /decision-support-svc
COPY --from=builder /healthcheck /healthcheck

USER nonroot:nonroot

EXPOSE 8138

ENTRYPOINT ["/decision-support-svc"]
