# Release Certificate — secret-vault-integration-svc

**Date:** 2026-09-21
**Scope:** the service and its console surface (`/admin/secrets`), end to end.
**Result:** `scripts/audit.sh` — **83 checks, 0 failures**, against a running
stack.

This certifies what was verified, against what, and what was deliberately not
done. It is not a summary of the design — that is `context.md` — nor of the
build history, which is `progress.md`.

---

## What this service is held to

The bar is the two services in this repo that already carry a release
certificate, `identity-context-svc` (GOV-01) and `tenant-entity-registry-svc`
(ORG-02/03). Both ship an `openapi.yaml`, an `asyncapi.yaml`, a `RUNBOOK.md`
and a certificate, and both are re-provable by a script. This service had the
code and the audit script and none of the four documents; §"Gaps closed" below
is the difference.

---

## Verification performed

Every figure here came from a run on 2026-09-21, not from a prior claim.

### Static and unit

| Check | Result |
|---|---|
| `go build ./...` | clean |
| `go vet ./...` | clean |
| `go test ./...` | 4 packages ok, 0 failing |
| Console `tsc --noEmit` | clean |
| Console `eslint` on changed files | clean |

### Store, against real Postgres 16

18 integration tests against a scratch `postgres:16-alpine` database, 0 failed,
0 **skipped**. The skip count is asserted, not reported: the suite is
`TEST_DATABASE_URL`-gated and an unset variable would skip every test in it
while `go test ./...` still printed `ok`. `REQUIRE_DB_TESTS=1` makes that a
loud failure, and §4 of the audit checks the guard itself still works.

The scratch database is not the live one. The last test in the suite drops all
four tables on purpose to prove the error path and does not restore them;
pointing it at `secret_vault_integration` leaves the running service with no
schema. This has happened twice historically and is now structural.

### Live, against the running service

The whole of `scripts/audit.sh` §5–§15: health and the container's own
`HEALTHCHECK`, policy administration and its idempotency, the three broker
outcomes, tenant isolation, malformed path parameters, rotation, audit
evidence, the §4 input contract, the "no material in Postgres" doctrine, 30
metric series, and the 6 alert rules loaded and evaluating in Prometheus.

### Console, against a hermetic mock

9 Playwright specs (`e2e/secrets.spec.ts`), 0 failures, plus the 22
pre-existing specs still passing — 31 total. Run as §17 of the audit.

---

## Contract surface

12 v1 routes plus 3 probes, all documented in `openapi.yaml` and all compared
against the chi router **in both directions** by §16 of the audit — a route the
service serves but does not document, and a route documented but not served,
both fail the run.

| Route | Authorization |
|---|---|
| `POST /v1/secret-policies` | `SECRET_POLICY_CREATE` |
| `GET /v1/secret-policies` | tenant scope only |
| `POST /v1/secret-policies/{id}/versions` | `SECRET_POLICY_VERSION_CREATE` |
| `GET /v1/secret-policies/{id}/versions` | tenant scope only |
| `POST /v1/secret-policies/{id}/versions/{vid}/activate` | `SECRET_POLICY_VERSION_ACTIVATE` |
| `POST /v1/secret-policies/{id}/material` | `SECRET_MATERIAL_WRITE` |
| `POST /v1/secret-policies/{id}/rotate` | `SECRET_ROTATE` |
| `POST /v1/secrets/broker` | **none** — see below |
| `GET /v1/secrets/leases/{lease_id}` | tenant scope only |
| `GET /v1/secrets/leases` | tenant scope only |
| `POST /v1/secrets/leases/{lease_id}/revoke` | `SECRET_LEASE_REVOKE` |
| `GET /v1/secrets/audit` | tenant scope only |

The broker route carrying no `SECRET_*` requirement is deliberate and is the
one asymmetry worth stating twice: the policy version's `allowed_workload_ids`
IS its authorization, and requiring an RBAC grant as well would mean every
workload needed a role assignment to use the service designed to remove exactly
that coupling. `TestBroker_NotRBACGated` pins it.

### Events

Three, on `zoiko.secretvault.events`, documented in `asyncapi.yaml` and
compared against the publisher in both directions by §16:
`secret.access.requested`, `secret.access.granted`,
`secret.rotation.completed`. None carries material or a `lease_token`.

---

## Gaps closed in this pass

Five defects and four missing artifacts. Fuller write-ups are in `progress.md`;
what matters for certification is that each is now pinned by a check that fails
if it returns.

| # | Defect | Pinned by |
|---|---|---|
| 1 | The audit log could not say **who revoked a lease** — the revoker was authorized, then discarded. Migration `000004` adds `acted_by_principal_id`. | audit §11, `TestRevokeLease_RecordsTheRevokerNotTheLeaseHolder`, `TestPgStore_ActorIsPersistedAndDistinctFromSubject`, console spec 8 |
| 2 | `ResultBanner` rendered an **empty banner** whenever a caller passed two conditional children (an array of falsy values is truthy). Shared component; not specific to this page. | console spec 3–5, which could not have waited on an outcome while the banner was always present |
| 3 | The console **told operators the admin routes were unauthorized**. All six are RBAC-gated; the guard was added and the copy was never updated. | corrected in `page.tsx` and `actions.ts`; `TestGatedRoutes_403_Denied` was already pinning the behaviour |
| 4 | The store suite's **migration list was hardcoded**, so a new migration would silently leave every test on the previous schema. | now reads the directory, and fails if it finds none |
| 5 | **Six live alert rules pointed at a `RUNBOOK.md` that did not exist.** | audit §16 resolves every `RUNBOOK section N.N` annotation to a real heading, both directions |

Artifacts added: `openapi.yaml`, `asyncapi.yaml`, `RUNBOOK.md`,
`deployments/migrations/000004_*`, plus `e2e/secrets.spec.ts` and
`e2e/mock/secret-vault-service.mjs` in the console repo.

### One thing the mock got wrong first, and why it is recorded

The first draft of the e2e mock demanded `X-Principal-Id` on reads. The service
does not — the envelope middleware applies to material writes, so a GET reaches
the handler and the handler's own `requireTenant` is the whole check. Every read
panel reported `401 missing_principal` and it read exactly like a console
defect. The contract the mock enforces now was read off the running service
rather than inferred from its policy declaration. An over-strict mock is worse
than no mock: it manufactures failures in correct code.

---

## The 404-vs-403 question

Open since design review, resolved here: **they stay distinct.** The full
reasoning is in `RUNBOOK.md` §3, where an on-call reader will actually need it.
Summarised: the two codes are the difference between a permissions fix and a
provisioning fix, and the provisioning case is this service's commonest failure
mode; the enumeration exposed is bounded to callers already inside the estate
and already carrying a verified tenant, every probe writes a `DENIED` row naming
the caller, and neither code brings anyone closer to the material. The opposite
answer is given on `/v1/secrets/leases/{id}`, where a foreign lease and a
nonexistent one are indistinguishable — pinned by audit §8.

---

## Not done, and deliberately

These are unchanged from `context.md` §7.10 and are **not** defects. Each is
listed so that a reader does not mistake absence for oversight.

- **No real Vault or KMS backend.** The `LocalFileVaultBackend` is real
  AES-256-GCM, not a stub, and the plaintext is not recoverable from the store
  file — but it is a local file. A production deployment needs a real backend
  behind the same `VaultBackend` interface.
- **No `identity-context-svc` wiring.** Documented as the concrete future
  consumer; not connected.
- **No caching of authorization or policy decisions on the broker path.** A
  permanent stance, not a v1 shortcut: a cached grant outlives the rotation that
  should have killed it.
- **No §9.6 sensitive-key separation** (tenant/document/evidence/payment key
  scopes). v2+.
- **`acted_by_principal_id` is not backfilled.** Rows written before migration
  `000004` read `NULL`, which is true. Copying the subject across would have
  been right for three event types and wrong for `REVOKED`, the one the column
  exists for — that is manufacturing evidence, not filling a gap.
- **The rotate/revoke step and the `ROTATED` audit write are not one
  transaction.** Known, documented at the call site. A crash between them leaves
  leases correctly revoked and the rotation unrecorded — the safe direction, but
  not atomic.

---

## Re-proving this

```bash
cd services/secret-vault-integration-svc && bash scripts/audit.sh
```

Needs: the service on `:8087`, `zoiko-postgres`, a Go toolchain, Prometheus on
`:9090` for §15, the console's `node_modules` for §17 (`SKIP_FE=1` skips it),
and the `CONSOLE_DEMO_OPERATOR` grants from
`deployments/scripts/seed-demo-rbac.ps1` — this service's six `SECRET_*` actions
are in its `VAULT_FULL` bundle. Without those grants every mutation is a correct
`403` and the script reports a working service as broken.

Exits non-zero on any failure, so CI can gate on it.

---

## Certification

As of 2026-09-21, secret-vault-integration-svc and its console surface pass
**83 of 83** audited checks against a live stack: build, vet, unit tests, 18
store tests against real Postgres 16, the full live request surface, tenant
isolation, the §4 input contract, telemetry, the alert rules, contract-document
parity with the code in both directions, and 9 console end-to-end specs.

The five defects this pass found are fixed and each is pinned by a check that
fails if it returns. The one design question left open since review is decided
and written down where it will be read. What remains undone is listed above and
is deliberate.
