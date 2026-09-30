FROM alpine:3.19

RUN apk add --no-cache ca-certificates tzdata

WORKDIR /app

COPY server /app/server
COPY healthcheck /app/healthcheck

RUN chmod +x /app/server /app/healthcheck

EXPOSE 8139

HEALTHCHECK --interval=10s --timeout=5s --start-period=5s --retries=3 \
  CMD ["/app/healthcheck"]

CMD ["/app/server"]
