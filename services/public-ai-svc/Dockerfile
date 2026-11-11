# public-ai-svc (Suivo R0). Build context is this service directory only:
#   docker build -t public-ai-svc services/public-ai-svc
# The image contains the server, the healthcheck probe and the kbctl operator
# tool. It contains no fixtures, keys or secrets.
FROM golang:1.27-alpine@sha256:0a5f7fe796599ff487605987432e7619ae45beeb2fba4c2ce0b907879932aebf AS builder
# go.mod pins toolchain go1.27.4 (standard-library security fixes). The
# digest-pinned builder image ships exactly go1.27.4 (GOLANG_VERSION), so the
# build never downloads a toolchain: GOTOOLCHAIN=local fails instead of
# fetching if the two ever disagree. Bump both together.
ENV GOTOOLCHAIN=local

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG GIT_SHA=unknown
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags "-s -w -X main.gitSHA=${GIT_SHA}" -o /out/server ./cmd/server && \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags "-s -w" -o /out/healthcheck ./cmd/healthcheck && \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags "-s -w" -o /out/kbctl ./cmd/kbctl

FROM alpine:3.22@sha256:2e0b81d60597f449baa56dc25d355367869d2dca4cd57e866f1e2b52af3eb55f

RUN apk --no-cache add ca-certificates && adduser -D -H -u 10001 publicai

WORKDIR /app
COPY --from=builder /out/server /out/healthcheck /out/kbctl /app/

USER 10001
EXPOSE 8080
ENV PUBLIC_AI_HTTP_ADDR=:8080

HEALTHCHECK --interval=15s --timeout=3s --start-period=10s --retries=3 CMD ["/app/healthcheck"]

ENTRYPOINT ["/app/server"]
