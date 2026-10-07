# Running `ncd_live_check.py`

The live check drives a running notification-svc over HTTP and reads the
`notification` database through `docker exec zoiko-postgres psql`. It needs the
service, Postgres, Mailpit and two stand-ins. It does **not** need the rest of
the estate.

## 1. Containers

Start only these two. Never start the whole stack for this.

```sh
docker start zoiko-postgres mailpit
```

The `notification` database must be at the latest migration
(`deployments/migrations/`, currently `000024`). The service does not migrate
on start; apply pending migrations with `python deployments/migrate.py up --db notification`. Check for the newest
migration's objects, for example the `trg_reject_evidence_delete` trigger.

## 2. Stand-ins for authorization-svc and identity-context-svc

```sh
python scripts/live_check_stubs.py      # authz on :18089, identity on :18080
```

Authorization grants every principal except `mallory`. Identity resolves every
principal to `<id>@example.test` except `ghost`. The service's own authz
client, identity client, envelope, idempotency, RLS and SMTP path are the real
ones.

## 3. The service

```sh
go build -o notification-svc.exe ./cmd/server
env PORT=8133 DB_HOST=localhost DB_PORT=5432 DB_NAME=notification \
    DB_USER=app_notification DB_PASSWORD=postgres DB_SSLMODE=disable \
    KAFKA_BROKERS= OTEL_EXPORTER_OTLP_ENDPOINT= \
    AUTHZ_SERVICE_URL=http://localhost:18089 IDENTITY_SERVICE_URL=http://localhost:18080 \
    NOTIFICATION_EMAIL_PROVIDER=smtp NOTIFICATION_EMAIL_FROM='Zoiko Suite <no-reply@zoiko.local>' \
    SMTP_HOST=localhost SMTP_PORT=1025 SMTP_TLS_MODE=none SMTP_ALLOW_CLEARTEXT=true \
    NCD_CALLBACK_SECRET_SMTP_PRIMARY=live-callback-secret \
    NOTIFICATION_WEBHOOK_SECRETS='{"smtp":["live-webhook-secret-0123456789"]}' \
    NOTIFICATION_UNSUBSCRIBE_SECRET=live-unsubscribe-secret-0123456789abcdef \
    NOTIFICATION_PUBLIC_BASE_URL=http://localhost:8133 \
    ./notification-svc.exe
```

`KAFKA_BROKERS=` (empty) runs the outbox relay dry: events are committed and
marked published, but nothing is sent to a broker. Leave the DKIM variables
unset. A monitor pointed at a domain with no published records would correctly
hold all mail.

## 4. Run

```sh
python scripts/ncd_live_check.py        # exit code = number of failures
```

The marketing round-trip prints `SKIP` instead of failing when the recipient
is inside the platform's marketing quiet window (21:00 to 08:00
Europe/London). The skip is correct behaviour, not a defect.

Each run uses a fresh tenant (`live-<hex>`) and leaves its rows in place. They
are evidence, and the database refuses to delete them.
