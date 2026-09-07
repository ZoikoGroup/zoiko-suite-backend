# Connecting the login path to Supabase

Points four services at a hosted Supabase database while the rest of the stack
stays on the local `postgres` container. Everything else — Kafka, Redis,
Traefik, the other 59 services — keeps running in Docker exactly as before.

| Service | Schema | Role | Why the login path needs it |
|---|---|---|---|
| `identity-svc` | `identity_context` | `app_identity_context` | principals and their password credentials; mints the identity envelope |
| `tenant-svc` | `tenant_entity_registry` | `app_tenant_entity_registry` | the root of all scope; resolve fails closed without it |
| `authorization-svc` | `authorization_svc` | `app_authorization` | consulted by every write on the platform |
| `governance-svc` | `governance_decision_log` | `app_governance_decision_log` | read by the console's landing page |

`gateway-auth-svc` is on the login path too but has no database, so there is
nothing here for it.

---

## The one thing to understand first

`deployments/init-db.sh` issues `CREATE DATABASE` **63 times**, one per service.
A Supabase project has exactly **one** database. That script cannot run there,
and there is no setting that changes it.

So each service gets a **schema** instead, named after the database it used to
have. The migration SQL is untouched and unaware: it says `CREATE TABLE
principals`, which lands wherever `search_path` points.

What that costs you is one guarantee. Isolation used to come from 63 separate
databases; now it comes from each role holding `USAGE` on its own schema and no
other. `deployments/supabase` provisions both halves — skip it and every service
would connect as the owner, which is the exact problem
`deployments/scripts/create-app-roles.sh` was written to fix.

### Do not put these tables in `public`

Supabase's Data API (PostgREST) serves the `public` schema over HTTPS to anyone
holding the anon key. `principal_credentials` holds argon2id password digests.
Named schemas are not exposed unless you add them under **Settings → API →
Exposed schemas** — so leave that setting alone, and never move these tables to
`public`.

---

## Step 1 — Collect four values

Supabase dashboard → **Project Settings → Database → Connection string**.

You need **two different connection modes**, which trips people up:

| | Used by | Tab to read | Port |
|---|---|---|---|
| **Session pooler** | the bootstrap tool, once | Session pooler | 5432 |
| **Transaction pooler** | the four services, always | Transaction pooler | 6543 |

The bootstrap tool needs the session pooler because it issues DDL across
statements that must stay on one connection. The services need the transaction
pooler because their pools are 20 connections each and the direct connection
caps out near 60.

Ignore the `db.<ref>.supabase.co` direct host entirely — it is IPv6-only on
newer projects, which fails as an opaque timeout on most home and office
networks.

Everything goes in **`.env.local` at the backend root** — already created, already
git-ignored. Four values:

| Variable | Where it comes from |
|---|---|
| `SUPABASE_PROJECT_REF` | the subdomain of your project URL |
| `SUPABASE_DB_HOST` | the pooler host, `aws-N-<region>.pooler.supabase.com` |
| `APP_DB_PASSWORD` | **invent one** — not your project password |
| `SUPABASE_DB_URL` | the full **session pooler** URI, for the bootstrap tool only |

`APP_DB_PASSWORD` is the password given to the four per-service login roles.
Keeping it distinct from the project password is the point: the services connect
as ordinary `NOBYPASSRLS` roles precisely so they are not using the owner's
credentials.

`.env` at the same level is the production counterpart — same variables, nothing
pre-filled. Both are git-ignored; `.env.example` is the committed template.

---

## Step 2 — Check what will be applied, offline

```bash
cd deployments/supabase
go run . -list
```

Needs no database and no credentials. It prints the 20 migrations in the order
they will run, with a checksum for each. If a service is missing or a directory
is misnamed, you find out here rather than half-way through a run against a live
database.

---

## Step 3 — Create the schemas and roles

The tool reads `SUPABASE_DB_URL` and `APP_DB_PASSWORD` from the environment, and
`.env.local` is not loaded automatically — export them for the run:

```powershell
# PowerShell, from the backend root
Get-Content .env.local | Where-Object { $_ -match '^\s*[A-Z]' } | ForEach-Object {
    $k, $v = $_ -split '=', 2
    [Environment]::SetEnvironmentVariable($k.Trim(), $v.Trim())
}
cd deployments/supabase
go run . -dry-run     # reports what it would do, writes nothing
go run .              # does it
```

```bash
# bash equivalent
set -a && . ./.env.local && set +a
cd deployments/supabase && go run . -dry-run && go run .
```

Or skip the export and pass them directly:
`go run . -url "<session pooler URI>" -app-password "<APP_DB_PASSWORD>"`.

This will:

1. create the four schemas, plus `zoiko_platform` for the migration ledger;
2. apply each migration inside its own transaction, with `search_path` set to
   that service's schema;
3. record every migration with a checksum, so a re-run is a no-op and an edited
   migration is reported rather than silently skipped;
4. create the four login roles `NOSUPERUSER NOBYPASSRLS`, each granted `USAGE`
   on its own schema and DML on its tables — no DDL, no `TRUNCATE`, nothing in
   any other schema.

It is safe to run again at any time.

---

## Step 4 — Start the stack

```bash
docker compose --env-file .env.local \
               -f deployments/docker-compose.yml \
               -f deployments/docker-compose.supabase.yml up -d --build
```

`--env-file` is required. With `-f`, Compose v2 takes its project directory from
the compose file's folder, so it would otherwise look for `deployments/.env` and
find nothing.

The second `-f` is the whole switch. Drop it and all four services are back on
the local `postgres` container with nothing else changed.

`--build` matters: `docker compose restart` reuses the existing image, so
without a rebuild you are testing the old binary against the new database.

Verify:

```bash
curl http://localhost:8000/identity-context-svc/healthz
curl http://localhost:8000/tenant-entity-registry-svc/healthz
```

---

## What broke, and what it means

**`password authentication failed for user "app_identity_context"`**
Through a Supabase pooler the username is `role.project_ref`, not the bare role.
Check `SUPABASE_PROJECT_REF` is set in `deployments/.env`.

**`relation "principals" does not exist`**
`search_path` names a schema that does not exist, or the role has no `USAGE` on
it. Postgres drops unresolvable `search_path` entries silently, so this is the
first place the mistake surfaces. Re-run `go run .` in `deployments/supabase`.

**`prepared statement "lrupsc_1_0" already exists`, intermittently**
`DB_OPTIONS` did not reach the service. pgx caches *named* prepared statements
by default and PgBouncer in transaction mode prepares them on one server
connection then executes on another. It passes every smoke test and fails under
concurrency. Confirm with
`docker compose exec identity-svc env | grep DB_OPTIONS`.

**A read returns nothing, and it used to return rows**
Expected, and the point of the exercise. Under a `NOBYPASSRLS` role, a query
that never sets `app.tenant_id` matches no rows — as superuser the same query
returned everything. If a panel goes empty after the cutover, that query was
never tenant-scoped and the isolation was previously inert.

**Connection timeout with no error detail**
Almost always IPv6. Use the `pooler.supabase.com` host, not
`db.<ref>.supabase.co`.

---

## What this does not cover

- **The other 59 services** stay on the local container. Extending this means
  adding entries to `services` in `main.go` and a block to the override file —
  the mechanism is unchanged, only the list grows.
- **Nothing is seeded.** The schemas come up empty, so there is still no tenant,
  no legal entity, no principal, and nothing to log in as.
  `seed-demo-registry.ps1` and `seed-demo-rbac.ps1` both assume the
  database-per-service layout and will need their connection details changed.
- **`init-db.sh` is untouched** and still correct for the all-local stack.
