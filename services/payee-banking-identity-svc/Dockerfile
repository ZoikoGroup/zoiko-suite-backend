FROM golang:1.25-alpine AS builder

WORKDIR /app/payee-banking-identity-svc

COPY eventing /app/eventing
COPY payee-banking-identity-svc/go.mod payee-banking-identity-svc/go.sum ./
RUN go mod download

COPY payee-banking-identity-svc/ ./

RUN CGO_ENABLED=0 GOOS=linux go build -o /server ./cmd/server
RUN CGO_ENABLED=0 GOOS=linux go build -o /healthcheck ./cmd/healthcheck

FROM alpine:3.19

RUN apk --no-cache add ca-certificates

WORKDIR /app

COPY --from=builder /server /app/server
COPY --from=builder /healthcheck /app/healthcheck

EXPOSE 8166

ENTRYPOINT ["/app/server"]
