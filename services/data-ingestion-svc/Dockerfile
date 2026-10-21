# BUILD CONTEXT IS services/, NOT THIS DIRECTORY:
#   docker build -f data-ingestion-svc/Dockerfile services/
# go.mod replaces zoiko.io/eventing with ../eventing (the shared scanner, error taxonomy and
# signature framework), and Docker can only COPY from inside the build context.
# Dockerfile.dockerignore next to this file trims the context to the two directories the build reads.
FROM golang:1.25-alpine AS builder

# The service sits at /build/data-ingestion-svc so the go.mod replace directive's ../eventing
# resolves to /build/eventing.
WORKDIR /build/data-ingestion-svc

# The replaced module must be present before go mod download can resolve it.
COPY eventing/ /build/eventing/
COPY data-ingestion-svc/go.mod data-ingestion-svc/go.sum ./
RUN go mod download

COPY data-ingestion-svc/ ./
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /server ./cmd/server
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /healthcheck ./cmd/healthcheck

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=builder /server /server
COPY --from=builder /healthcheck /healthcheck
USER nonroot:nonroot
EXPOSE 8145
ENTRYPOINT ["/server"]
