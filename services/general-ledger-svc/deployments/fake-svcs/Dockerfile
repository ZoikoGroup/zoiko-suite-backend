# Tiny static two-in-one fake (authz GRANTED + close OPEN) for the limited local compose.
# Pure-Go, no dependencies → distroless/static, same pattern as the services.
FROM golang:1.25-alpine AS builder

WORKDIR /build

COPY go.mod go.sum* ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags="-s -w" \
    -o /fake-svcs \
    .

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=builder /fake-svcs /fake-svcs

USER nonroot:nonroot

EXPOSE 8089 8104

ENTRYPOINT ["/fake-svcs"]