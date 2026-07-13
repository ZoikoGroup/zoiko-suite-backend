# ─── Stage 1: builder ─────────────────────────────────────────────────────────
FROM golang:1.25-alpine AS builder

RUN apk add --no-cache git ca-certificates
WORKDIR /build

COPY go.mod ./
# No go.sum: gtrm-pool has no external dependencies (stdlib only).
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags="-s -w" -o /gtrm-pool ./cmd/server/
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags="-s -w" -o /healthcheck ./cmd/healthcheck/

# ─── Stage 2: runtime ─────────────────────────────────────────────────────────
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=builder /gtrm-pool /gtrm-pool
COPY --from=builder /healthcheck /healthcheck

USER nonroot:nonroot
EXPOSE 8080

ENTRYPOINT ["/gtrm-pool"]
