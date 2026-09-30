# syntax=docker/dockerfile:1
FROM gcr.io/distroless/static-debian12:nonroot

COPY server /obligations-svc
COPY healthcheck /healthcheck

USER nonroot:nonroot

EXPOSE 8088

ENTRYPOINT ["/obligations-svc"]
