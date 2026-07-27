FROM golang:1.22-alpine AS builder
WORKDIR /app
COPY go.mod ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /server ./cmd/server
RUN CGO_ENABLED=0 GOOS=linux go build -o /healthcheck ./cmd/healthcheck

FROM alpine:3.19
RUN apk add --no-cache ca-certificates tzdata
WORKDIR /app
COPY --from=builder /server /app/server
COPY --from=builder /healthcheck /app/healthcheck
EXPOSE 8144
ENTRYPOINT ["/app/server"]
