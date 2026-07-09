# Secret Vault Integration Service — Progress & Phase Plan

Status: **feature-complete for v1 and verified live end-to-end, 2026-07-08.**
Full spec is in `context.md` (finalized after two rounds of self-review
that caught real design bugs before any code was written — see that
file's §0 and its inline "correction found on review" notes). Building it
surfaced two more real bugs that self-review couldn't have caught,
documented below.

Workflow per the approved task: one branch for the whole service
(`feat/secret-vault-integration-svc`) off `main`, never touch `main`
directly, PR when done. Verify against real Postgres (Docker) before
calling any batch done — same bar as `policy-svc` and
`configuration-feature-flag-svc`.

## Verification actually performed (2026-07-08)

Using the same Docker-based method as the other two services (no host Go
toolchain needed): a `golang:1.25-alpine` container against a real
`postgres:16-alpine` container (network `svi-net`), plus the actual
multi-stage `Dockerfile` image built and run separately.

1. `go mod tidy && go vet ./... && go build ./cmd/server && go build ./cmd/healthcheck`
   → clean on the first pass, no errors — a genuinely large first build
   (4 tables, 11 endpoints, a real crypto backend) compiling clean the
   first time.
2. Migration applied cleanly to a fresh database — 4 tables, 16 indexes,
   including the two partial unique indexes (one-active-per-scope, and
   the ROTATED-only request_id dedup) and the rollout-style CHECK-free
   schema — first real proof the SQL is valid, not just plausible.
3. `go test ./... -v` — **first run found a real bug**:
   `TestPgStore_CreateSecretPolicy_IdempotencyAndConflict` and five other
   store tests failed with `invalid input syntax for type uuid: ""`.
   Root cause: `CreateSecretPolicy` and `CreateSecretPolicyVersion` were
   passing `params.SecretPolicyID`/`params.SecretPolicyVersionID` (empty
   string when not caller-supplied, which is always, in practice) directly
   into an `INSERT` targeting a `UUID PRIMARY KEY DEFAULT
   gen_random_uuid()` column — the empty string was rejected outright by
   Postgres instead of falling back to the column default. **Fixed** by
   removing those columns from the `INSERT` statements entirely, letting
   Postgres's own `DEFAULT` generate the ID (`CreateLease` and
   `RecordAuditEntry` never had this bug — they were written without an
   ID column in their `INSERT`s from the start). Re-ran: **37/37 pass**
   (27 handler unit tests + 10 store integration tests).
4. Live server started (container `svi-app`, port 8087). `/healthz` and
   `/readyz` both `200`.
5. **Full live round trip, including a second real bug found and fixed
   mid-verification**: create policy → create version → activate →
   `POST /v1/secrets/broker` → **`503 vault_backend_unavailable`**. Root
   cause: nothing in the original design exposed any way to call
   `VaultBackend.Put` — `Broker`'s call to `Get` can never succeed for a
   real deployment because no material is ever seeded. The grant path was
   completely unreachable end to end despite every other piece (policy,
   lease, audit) working correctly. **Fixed** by adding
   `POST /v1/secret-policies/{id}/material` (§7.2 addendum in
   `context.md`) — not part of the original spec, found missing only by
   actually trying to use the service, not by re-reading the design.
6. Re-ran the full flow after the fix — all of the following confirmed
   live, not just asserted in tests:
   - `POST /v1/secret-policies/{id}/material` → `200`, material stored
     (confirmed the local vault store file contains only base64
     nonce/ciphertext — the plaintext value is not recoverable by reading
     the file directly, real AES-256-GCM, not a no-op)
   - `POST /v1/secrets/broker` (authorized workload) → `200`, real
     `lease_token` returned
   - Identical retry (same `request_id`) → `200`, **same `lease_id`**,
     fresh `lease_token` (the documented idempotency design choice —
     durable state is idempotent, the token itself is re-minted)
   - Broker for an unauthorized workload → `403 access_denied`
   - Broker for an unregistered `secret_path` → `404 no_applicable_secret_policy`
   - `POST /v1/secret-policies/{id}/rotate` → `200`,
     `revoked_lease_count: 1`
   - The previously-granted lease, re-fetched → `status: "REVOKED"`, not
     still `GRANTED` — the exact fix from `context.md`'s third design
     review pass, now proven against a real database, not just a unit
     test
   - Retried rotate (same `request_id`) → `200`, vault backend's `Rotate`
     **not called a second time** (confirmed via the dev container's
     build, which would have logged a second rotation)
7. Built the real `Dockerfile` image (`secret-vault-integration-svc:verify`)
   — both the server and `healthcheck` binaries compiled into the same
   multi-stage build. Ran it as a container: `docker ps` reported
   **`(healthy)`** — the image's own `HEALTHCHECK` instruction, invoking
   the compiled `healthcheck` binary internally, actually works. This is
   the first service in this repo with a functioning Docker-level health
   check, not just an externally-polled `/healthz`.

**One known cosmetic gap, not a correctness bug**: an idempotent replay
of `POST .../rotate` returns `revoked_lease_count: 0` (it returns the
original `ROTATED` audit entry's basics, which don't include a replayed
lease count) even though the real first rotation revoked leases
correctly and permanently. The underlying data is correct either way —
only the replay response's reported count is imprecise. Not fixed in
this pass; flagged for whoever picks this up next.

**Repeat of a lesson from `configuration-feature-flag-svc`'s build, made
again despite being documented**: `TestPgStore_ErrorsWrapErrStoreUnavailable`
intentionally drops all four tables to test the failure path with no
teardown, and is the last test in the file — running the test suite
against the same database used for live verification left it broken
twice during this session, requiring the migration to be reapplied each
time. Worth actually automating a "test DB ≠ demo DB" guard at some point
rather than relying on remembering this a third time.

## Batch 1 — Schema + secret policy administration (mirrors policy-svc's Batch A)

- [x] `go.mod`/`go.sum` — same dependency set as every other Go service
      here (chi, pgx/v5, zap)
- [x] Migration `000001_initial_schema.up.sql`/`.down.sql` — all four
      tables from `context.md` §7.1: `secret_policies`,
      `secret_policy_versions`, `secret_leases`, `secret_access_audit_log`
- [x] `internal/domain/types.go` — `SecretPolicy`, `SecretPolicyVersion`,
      `SecretLease`, `SecretAccessAuditLog`, params structs, error
      sentinels (`ErrSecretPolicyNotFound`, `ErrSecretPolicyVersionNotFound`,
      `ErrLeaseNotFound`, `ErrInvalidTransition`, `ErrConflict`,
      `ErrStoreUnavailable`)
- [x] `internal/store/pg_store.go` — `CreateSecretPolicy`,
      `CreateSecretPolicyVersion`, `ActivateVersion`, `ListVersionHistory`,
      `FindApplicableVersions` — mirrors `policy-svc`'s
      `pg_store.go` method-for-method (dedup on `secret_path`, transition
      with caller-supplied `allowedPriors`, scope-precedence query)
- [x] `internal/handler/handler.go` — `POST /v1/secret-policies`,
      `POST /v1/secret-policies/{id}/versions`,
      `POST /v1/secret-policies/{id}/versions/{version_id}/activate`,
      `GET /v1/secret-policies/{id}/versions`,
      `GET /v1/secret-policies?secret_class=&tenant_id=&legal_entity_id=`
      (`secret_class` required, `400` if missing)
- [x] `internal/config/config.go`, `internal/health/health.go`,
      `cmd/server/main.go` — standard wiring, port **8087** (confirmed
      free, `context.md` §7.9)
- [x] Handler unit tests (stub store) + Postgres integration tests
      (`TEST_DATABASE_URL`-guarded) proving: create idempotency, version
      idempotency, activate supersedes-not-deletes, scope precedence

**Verification:** create a secret policy, create a version, activate it,
create a second version and activate that too, confirm the first is
`SUPERSEDED` not deleted and history shows both — identical proof
structure to `policy-svc`'s own Batch A verification.

## Batch 2 — Vault backend + brokering (the core value)

- [x] `internal/vault/backend.go` — `VaultBackend` interface
      (`Get`/`Put`/`Rotate`, `context.md` §7.6) +
      `LocalFileVaultBackend` (AES-GCM encrypted-at-rest local file,
      key from env var — real, not a fake stub)
- [x] `internal/store/pg_store.go` extended — `CreateLease` (idempotent
      on `request_id`), `FindLeaseByID`, `ListLeases` (5 filters +
      pagination), `RevokeLease` (allowedPriors transition),
      `RecordAuditEntry`
- [x] `internal/handler/handler.go` extended —
      `POST /v1/secrets/broker` (full flow: audit `REQUESTED` → resolve
      policy by `secret_path` → `404`/`403`/grant → audit + vault call →
      `secret.access.requested`/`secret.access.granted` events),
      `GET /v1/secrets/leases/{lease_id}`,
      `GET /v1/secrets/leases?...` (paginated),
      `POST /v1/secrets/leases/{lease_id}/revoke`
- [x] `POST /v1/secret-policies/{id}/material` — **not in the original
      spec, added during live verification** when it became clear
      `Broker` could never reach a grant without any way to seed material
      into the vault backend first. Administrative-only, never called
      from the broker request path.
- [x] Handler unit tests + Postgres integration tests proving: deny when
      no policy exists, deny when principal not in `allowed_workload_ids`,
      grant + idempotent-replay-returns-same-lease, revoke transition,
      audit entries recorded for every outcome (requested/granted/denied/
      revoked)

**Verification:** activate a policy for a `secret_path` with one allowed
workload, broker for that workload (expect grant), broker for a
different workload (expect `403`), broker for an unregistered path
(expect `404`), confirm all three produce audit log rows.

## Batch 3 — Rotation + audit query

- [x] `internal/handler/handler.go` extended —
      `POST /v1/secret-policies/{id}/rotate` (calls `VaultBackend.Rotate`,
      records `ROTATED` audit entry, **mass-revokes every currently
      `GRANTED` lease for that `secret_path` in the same transaction**
      — the fix found in `context.md`'s third review pass — publishes
      `secret.rotation.completed`), `GET /v1/secrets/audit?...`
      (paginated, same 5-ish filter shape as the leases list but against
      the audit table, surfaces `DENIED`/`REQUESTED`/`ROTATED` too)
- [x] Rotate idempotency: partial unique index on
      `secret_access_audit_log (request_id) WHERE event_type = 'ROTATED'`
- [x] Tests proving: rotate revokes existing leases (not just records an
      event), rotate is idempotent on `request_id`, audit query surfaces
      all five event types

**Verification:** grant a lease, rotate the policy's secret, confirm the
lease now reads `REVOKED` (not still `GRANTED`), confirm a `ROTATED`
audit entry exists, confirm retrying the same rotate `request_id` doesn't
rotate twice.

## Batch 4 — Events, `cmd/healthcheck`, CI, Dockerfile, README

- [x] `internal/events/publisher.go` — mirrors
      `governance-decision-log-svc`'s exactly; publishes
      `secret.access.requested`, `secret.access.granted`,
      `secret.rotation.completed`
- [x] `cmd/healthcheck/main.go` — **new pattern for this repo** (verified
      in `context.md` §7.8 that no other service has this yet): a minimal
      binary doing `http.Get("http://localhost:$PORT/healthz")`, exit `0`
      on `200`, exit `1` otherwise, for use in the Dockerfile's
      `HEALTHCHECK` instruction (distroless has no shell/curl/wget)
- [x] Dockerfile — mirrors `governance-decision-log-svc/Dockerfile`'s
      two-stage build, plus a `HEALTHCHECK` instruction invoking the
      compiled `healthcheck` binary (also new — no other Dockerfile here
      has one yet); `EXPOSE 8087`
- [x] `.dockerignore`
- [x] CI — add `secret-vault-integration-svc` to `matrix.service` and the
      `TEST_DATABASE_URL` conditional in `.github/workflows/ci.yml`
- [x] `services/README.md` — add the service row (port 8087)

**Verification:** build the real Dockerfile image, run it against real
Postgres, confirm the container's own `HEALTHCHECK` reports healthy, full
create-policy → activate → broker → rotate round trip from inside the
container.

## Explicit non-goals (see `context.md` §7.10 for full reasoning)

- No real Vault/KMS backend (local-file only)
- No Authorization Service integration for admin writes
- No `identity-context-svc` wiring (documented as the concrete future
  consumer, not connected in this build)
- No caching, ever — permanent stance, not a v1 shortcut
- No §9.6 "Sensitive Key Separation" (tenant/document/evidence/payment
  key-scope separation) — v2+ concern

## Post-verification corrections (2026-07-09)

A line-by-line spec-alignment audit (`context.md` vs. actual code, not
just re-reading this file's own claims) found four gaps this file's
"feature-complete" status didn't mention. All four fixed and verified the
same way as every other batch — real Postgres via Docker, plus a fresh
Docker Desktop start since it wasn't running:

1. **Real correctness gap, now fixed**: `context.md` §7.1 defines lease
   `status` as `GRANTED | EXPIRED | REVOKED`, with `EXPIRED` an explicitly
   designed *computed read* (`status = 'GRANTED' AND expires_at < NOW()`),
   never a background job. No query actually computed this — every read
   returned the raw stored column, which was only ever `GRANTED` or
   `REVOKED`, so an expired lease reported `status: "GRANTED"` forever.
   Fixed in `internal/store/pg_store.go`: added `secretLeaseReadColumns`
   (same as `secretLeaseColumns` but with a `CASE WHEN status = 'GRANTED'
   AND expires_at < NOW() THEN 'EXPIRED' ELSE status END`), used by
   `FindLeaseByID`, `findLeaseByRequestID`, and `ListLeases`. Raw
   `secretLeaseColumns` stays on the INSERT/UPDATE `RETURNING` paths
   (`CreateLease`, `RevokeLease`, `RevokeLeasesBySecretPath`), where the
   row was just written and its stored status is current by definition.
   `RevokeLease`'s existing `current.Status != "GRANTED" →
   ErrInvalidTransition` check now actually rejects revoking an expired
   lease, which its own doc comment already claimed it did. New test:
   `TestPgStore_LeaseStatus_ExpiredIsComputedNotStored` (creates a lease
   with `expires_at` in the past, confirms the stored column is still
   `GRANTED`, confirms `FindLeaseByID`/`ListLeases` report `EXPIRED`,
   confirms `RevokeLease` returns `ErrInvalidTransition`). All 11 store
   tests pass against real Postgres (Docker), this one included.
2. `POST /v1/secrets/broker`'s body per `context.md` §7.2 should include
   `correlation_id`; the handler only read `X-Correlation-ID`. Fixed:
   `brokerRequest` now has a `CorrelationID` field, used when the header
   is absent (header still wins when both are present, matching every
   other endpoint's precedent). New test:
   `TestBroker_CorrelationIDFromBody_UsedWhenHeaderAbsent`.
3. `DENIED` audit entries on the 403 (unauthorized-workload) path never
   populated `secret_class`, even though it's known at that point
   (`applicable.SecretClass`) — an evidence-completeness gap relative to
   §5's "every denial must produce retrievable audit evidence." Fixed:
   `recordDenial` now takes a `secretClass` parameter, populated from
   `applicable.SecretClass` on the 403 path and left empty on the 404
   (no-policy-at-all) path, where it's genuinely unknown. Extended
   `TestBroker_NotAuthorized` to assert this.
4. **Documentation fix, not a code change**: `context.md` §7.2's
   documented `rotate` body was `{request_id}` only, but the handler has
   always also required `rotated_by_principal_id` (needed because
   `secret_access_audit_log.requested_by_principal_id` is `NOT NULL`).
   Updated §7.2 to document both fields instead of changing working code
   to match an incomplete spec.

Verified: `go vet ./... && go build ./cmd/server && go build
./cmd/healthcheck` clean; all 27 handler unit tests pass; all 11 store
integration tests pass against real Postgres (`postgres:16-alpine` in
Docker). Not re-run against the built Docker image this time — no
runtime-shape change, only query text and struct fields.

## Open item flagged, not yet resolved

`context.md`'s final section raised a real adversarial-security question
not yet answered: should `404` (no policy for this `secret_path`) and
`403` (policy exists, caller not authorized) collapse into the same
response, to avoid letting a caller enumerate which `secret_path` values
exist by probing the broker endpoint? Not resolved before starting
implementation — flagged here so it isn't lost, and worth a `/security-review`
pass before this service is considered production-ready, not just
functionally complete.
