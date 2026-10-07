FROM alpine:3.19

RUN apk --no-cache add ca-certificates

WORKDIR /app

COPY server /app/server
COPY healthcheck /app/healthcheck

RUN chmod +x /app/server /app/healthcheck

EXPOSE 8155

HEALTHCHECK --interval=10s --timeout=5s --start-period=5s --retries=3 \
  CMD ["/app/healthcheck"]

ENTRYPOINT ["/app/server"]
