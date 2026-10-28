# public-ai-svc (Suivo R0). Build context is this service directory only:
#   docker build -t public-ai-svc services/public-ai-svc
# The image contains the server, the healthcheck probe and the kbctl operator
# tool. It contains no fixtures, keys or secrets.
FROM golang:1.25-alpine AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG GIT_SHA=unknown
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags "-s -w -X main.gitSHA=${GIT_SHA}" -o /out/server ./cmd/server && \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags "-s -w" -o /out/healthcheck ./cmd/healthcheck && \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags "-s -w" -o /out/kbctl ./cmd/kbctl

FROM alpine:3.19

RUN apk --no-cache add ca-certificates && adduser -D -H -u 10001 publicai

WORKDIR /app
COPY --from=builder /out/server /out/healthcheck /out/kbctl /app/

USER 10001
EXPOSE 8080
ENV PUBLIC_AI_HTTP_ADDR=:8080

HEALTHCHECK --interval=15s --timeout=3s --start-period=10s --retries=3 CMD ["/app/healthcheck"]

ENTRYPOINT ["/app/server"]
