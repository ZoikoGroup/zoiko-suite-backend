# servicectl

Runs the backend's 86 Go services as OS processes, on demand, with no Docker.

## Why this exists

A Go service is a static binary that reads its configuration from the
environment and listens on a port. Docker was supplying three things around
that:

| Docker provided | Replaced by |
| --- | --- |
| a build step | `go build`, cached per service |
| DNS names for service-to-service calls | the generated registry, rewritten to `127.0.0.1` |
| a per-service environment block | one global env file at the backend root |

What is left is process supervision, which is this program. It is a **server**
rather than a script because 86 services is far more than a laptop should run,
and the console knows which handful any given page actually reads. It asks for
those on navigation and they are stopped again once nothing has asked for a
while.

The compose files still work. Switching back is still
`docker compose --env-file .env -f deployments/docker-compose.yml up`.

## Quick start

```sh
# build the launcher itself
cd tools/servicectl && go build -o ../../.servicectl/servicectl.exe .

# one-time: compile all 86 services into the binary cache (a few minutes)
.servicectl/servicectl.exe build

# run the daemon the console talks to
.servicectl/servicectl.exe serve
```

Then in the **frontend's** `.env.local`, set `ZOIKO_SERVICECTL=true`. Visiting
`/admin/finance` now starts the six services that route reads, waits for them to
answer their health probes, and renders.

Skipping `build` is allowed — the first visit to a page pays for the Go compile
instead, which is why `ZOIKO_SERVICECTL_TIMEOUT_MS` defaults to 20s rather than
something tighter.

## Commands

```
serve  [--addr host:port] [--idle 20m] [--start-timeout 90s] [--no-reap]
status
start  <service|/admin/page>...      run in the foreground, Ctrl-C to stop
stop   <service|/admin/page>... | --all
build  [service]...                  no arguments builds everything
env    <service>                     the environment a service would receive
ports                                the port table
pages                                the route-to-services map
```

`start` and `stop` accept either a service name or a console route, so
`servicectl start /admin/tax` brings up the seven tax services.

The dashboard is the daemon's root path — `http://127.0.0.1:8079` — with a card
per console route and a row per service, each with start/stop/logs/env.

## HTTP API

| Method | Path | Purpose |
| --- | --- | --- |
| `POST` | `/v1/ensure` | start what a page needs; `{page, services, wait, timeoutMs}` |
| `POST` | `/v1/stop` | `{services}` or `{all:true}` |
| `GET` | `/v1/services` | registry plus live state |
| `GET` | `/v1/pages` | the route-to-services map |
| `GET` | `/v1/logs?service=` | retained output (last 300 lines) |
| `GET` | `/v1/env?service=` | composed environment, secrets masked |
| `GET` | `/healthz` | liveness, and what the launcher thinks it is configured with |

`/v1/ensure` answers **207** when some of a route's services came up and some did
not. That is not an error to retry: the body names which failed, and a blanket
500 would make the console treat one dead service as a dead launcher and stop
asking.

## Ports are allocated here

`registry_gen.go` is the authoritative port table, and the ports in it are an
allocation rather than a convention. The four compose files were never meant to
run together, so **sixteen ports were claimed twice** across them, and four were
claimed twice inside the console's own `lib/api/config.ts`. A launcher that can
start any service at any moment cannot rely on the files never overlapping.

Services the main compose file already ran keep the port they had. Seven that
lost a tie moved into the 8200+ band, and the frontend's `.env.local` carries a
`ZOIKO_*_URL` override for each. `servicectl ports` prints the whole table.

## Cross-service URLs are resolved by variable name

Fourteen compose values named a different service than their own key did: six
services asked `authorization-svc` for jurisdiction rules, six pointed
`AUTHZ_SERVICE_URL` at `host.docker.internal:8081` (the tenant registry's port),
`filing-preparation-svc` asked authorization for evidence manifests, and
`corporate-tax-svc` addressed `general-ledger-svc` on `:8091` instead of `:8098`.

Most never fired, because the services carrying them lived in compose files that
were never started alongside the main one. Under one launcher they would all
fire, so the generator resolves these from the variable name and
`registry_test.go` holds that property.

The two `identity-context-svc` self-references — `ACCESS_CONTROL_URL` and
`DELEGATED_AUTHORITY_URL` pointing at `identity-svc` — are deliberate: that
service serves those endpoints itself, and the by-name rule is explicitly
exempted for them.

## Regenerating the registry

`registry_gen.go` is generated from `deployments/docker-compose*.yml` (each
service's environment and database name) and the console's `lib/api/config.ts`
(the ports it expects). Regenerate after changing either, then run
`gofmt -w registry_gen.go` and `go test ./...` — the tests hold uniqueness, the
absence of Docker hostnames, and the by-name URL property.

## Docker volume paths

Four services were configured with absolute container paths backed by named
volumes, which do not exist without Docker — and on Windows resolve against the
current drive root, so the failure is either a refusal to start or a service
quietly writing to `C:\data`. The launcher owns a directory per service under
`.servicectl/data/` and rewrites the path:

| Service | Variable | Was | Now |
| --- | --- | --- | --- |
| identity-context-svc | `JWT_SIGNING_PRIVATE_KEY_PATH` | `/keys/envelope_signing_key.pem` | `.servicectl/data/<svc>/envelope_signing_key.pem` |
| document-vault-svc | `STORAGE_DIR` | `/data/documents` | `.servicectl/data/<svc>/documents` |
| mtls-management-svc | `CA_DATA_DIR` | `/data/ca` | `.servicectl/data/<svc>/ca` |
| secret-vault-integration-svc | `VAULT_LOCAL_STORE_PATH` | `/tmp/secret_store.local` | `.servicectl/data/<svc>/secret_store.local` |

An explicit value in the global env always wins, so a deployment points these at
real storage.

One of those volumes also had a **sidecar container populating it**:
`identity-svc-keygen` ran `openssl genrsa` into `/keys` and identity-svc depended
on it completing. The launcher generates that key itself with `crypto/rsa` — so
openssl need not be on PATH — as a throwaway development key, written via a
temp-file rename so an interrupted start cannot leave a truncated PEM. An
existing key is *parsed* rather than trusted, because a corrupt one fails at
`NewJWTSigner` with "parse private key", which reads like a code fault and no
amount of restarting fixes.

`JWT_SIGNING_SECRET` is **not** auto-generated, deliberately. Other services
verify envelopes with it, so a fresh random value per run would break every
cross-service verification — and it would fail as an authorization error, which
sends you looking at RBAC. It must be in the global env; `.env.local` carries the
same throwaway value compose pinned.

## Adopted listeners

If something is already serving a service's port and answers a health probe, the
launcher **adopts** it rather than restarting it — it could be that service run
from a terminal. An adopted listener has no child handle, so:

- its pid column reads `adopted`, not blank
- `stop` **refuses** it with a message naming the port, instead of reporting a
  stop that did not happen
- idle reaping skips it
- shutdown lists it under `STILL RUNNING (adopted, not ours to stop)`

The common way to create one is force-killing the daemon: `TerminateProcess`
(PowerShell's `Stop-Process -Force`, or a killed terminal) runs no cleanup path
and orphans the children. Ctrl-C or SIGTERM stops them properly.

## What this does NOT replace

The launcher runs the **services**. It does not run their infrastructure:

- **Postgres** is a hard dependency — every service with a database calls
  `pool.Ping` at startup and `log.Fatal`s if it fails. Point `ZOIKO_DB_PROVIDER`
  at `supabase`, or run a Postgres somewhere `DB_HOST` can reach.
- **Kafka** is needed by four services at startup
  (`configuration-feature-flag`, `governance-decision-log`,
  `jurisdiction-rules`, `secret-vault-integration`). The other 74 use kafka-go
  writers that dial on first publish, so an unreachable broker costs a log line.
- **Redis** is a hard dependency of `identity-context-svc` only.
- **MinIO / OpenSearch / OTel / Jaeger / Prometheus / Grafana** are not started.
  Tracing degrades cleanly: the OTLP exporter never dials at construction, so an
  unset endpoint is not a startup failure.

### The Supabase schema gap

`ZOIKO_DB_PROVIDER=supabase` works fully for **38 of the 80** services that have
a database:

- **4** read `DB_SCHEMA` directly — `identity-context-svc`,
  `tenant-entity-registry-svc`, `authorization-svc`,
  `governance-decision-log-svc`.
- **34** read a whole `DATABASE_URL`, which the launcher gives a `search_path`
  query parameter; pgx passes it to the server as a runtime parameter.
- **The remaining 42** build a DSN from the discrete `DB_*` variables with no
  `search_path`, so they would silently read the default schema. Adding
  `DB_SCHEMA` to their `config.go` and `DSN()` is what closes this.

Under `docker`/`local` all 80 work, because each gets its own database.

### The four entry services are provisioned and verified

`identity-context-svc`, `tenant-entity-registry-svc`, `authorization-svc` and
`governance-decision-log-svc` run against Supabase today — schemas, migrations
and per-service login roles all in place, started in 1.7–2.8s each.

Each connects as its **own** `app_<schema>` role (`authorization_svc` is the one
naming exception: its role is `app_authorization`). That is not cosmetic. On a
single-database host the only thing separating one service's data from another's
is that its role holds `USAGE` on its own schema and no other, so a single shared
login would leave the schemas as a naming convention rather than isolation.
Verified directly: all four roles are `NOSUPERUSER NOBYPASSRLS`, each reads its
own schema, and each is **refused** on a peer's.

`SUPABASE_DB_USER` overrides this for the whole estate. It is an escape hatch for
a host provisioned with one role, and using it gives up the separation above.

Provisioning is `deployments/supabase`. Its `-roles-only` flag exists because
that tool applied migrations and provisioned roles in one pass and returned early
on the first migration it could not apply — leaving **zero roles for four
services**, three of which had migrated perfectly, so none of them could start
for a reason that had nothing to do with them.
