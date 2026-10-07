# syntax=docker/dockerfile:1
FROM gcr.io/distroless/static-debian12:nonroot

COPY services/search-indexer-svc/server /server
COPY services/search-indexer-svc/healthcheck /healthcheck

USER nonroot:nonroot

EXPOSE 8096

ENTRYPOINT ["/server"]
