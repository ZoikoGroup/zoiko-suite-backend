# delegated-authority-svc — implementation progress

Tracked against `docs/architecture/03-microservices.md` §9.3 and the data-model
rules in `docs/architecture/04-data-model.md` §6.3. There is no dedicated
`ZS-SVC-*` specification document for this service — §9.3 is four lines, and
most of what this register has to get right follows from those four lines plus
the estate-wide invariants rather than from a detailed spec.

Last updated: 2026-09-22.

---

## §9.3, clause by clause

| Clause | Status | Evidence |
|---|---|---|
| "Time-bound" | **Done** | `effective_from`/`effective_to` required; a window with no positive duration is refused `invalid_time_window`. Expiry is enforced by a background sweeper, not by trusting the reader — see below. |
| "Scope-bound" | **Done** | Every grant is bound to one `legal_entity_id` and one `action_type`. There is no "all actions" or "all entities" form. |
| "Approval-bound" | **Partial** | Creation is gated on a live authorization decision, and administered creation needs a second grant (`DELEGATION_ADMINISTER`). There is no *workflow* approval step — no route submits a delegation to workflow-svc for a second human. See "Not done". |
| "Delegated authority must never exceed the delegator's own authority" | **Done** | `CheckAllowed` on the **delegator**, for the exact `action_type`, against authorization-svc at creation time. Never trusted from the request body. `TestCreateDelegation_DelegatorLacksAuthority`; audit §7. |
| Publishes `authority.delegated` | **Done** | Through the transactional outbox. audit §6, §11. |
| Publishes `authority.revoked` | **Done** | Through the outbox, in the same transaction as the status change. |
| Publishes `authority.expired` | **Done** | Background sweeper across all tenants; enqueued in the transaction that flips the row. audit §9, §10. |

## Doc 04 §6.3 modelling rules

| Rule | Status | Note |
|---|---|---|
| "Delegated authority must be effective-dated" | **Done** | Both ends required and validated. |
| "Access decisions are evidence and must not be ephemeral" | **Done** | Nothing is hard-deleted; `ACTIVE` → `REVOKED`/`EXPIRED` are both terminal and both retain the row. This is also why `expired_at` was corrected — see below. |
| "Denials are as important as grants from an evidential standpoint" | **Partial** | Refusals are counted by decision in Prometheus and logged, but there is no durable per-refusal evidence row in this service's own tables. authorization-svc records its own decisions; a refusal this service makes *before* calling it (self-dealing, delegator mismatch) exists only in logs and metrics. See "Not done". |

---

## Gaps closed in this pass (2026-09-22)

| Gap | Where it was | Status |
|---|---|---|
| **Expiry waited for somebody to read the register.** `ExpireDue` ran only from the three HTTP handlers and was tenant-scoped, so a tenant whose register nobody opened expired nothing at all — indefinitely. Its delegates kept authority past their window and `authority.expired` was never published, so identity-context-svc was never told to end their sessions. | `internal/handler/handler.go` (read-path only), `internal/store/pg_store.go` | **Done** — `internal/expiry` sweeps every tenant every `EXPIRY_SWEEP_INTERVAL` (30s). Proved live in audit §10: a grant seeded in a tenant nothing reads is expired with no request made. |
| **`expired_at` recorded when the sweep noticed, not when the authority ended.** A grant whose window closed on a Friday and was next swept on Monday asserted, in evidence, that its authority ran all weekend. The event carried the same wrong timestamp. | `ExpireDue` set `expired_at = now()` | **Done** — `expired_at = effective_to`; `updated_at` keeps the observation time. Migration `000004` backfills the misdated rows from `effective_to`, which was always present in the same row. |
| **The store suite skipped silently** when `TEST_DATABASE_URL` was unset, reporting `ok` having verified nothing. | `internal/store/pg_store_test.go` | **Done** — fails under `CI` or `REQUIRE_DB_TESTS`. |
| **The isolation tests could pass as a superuser**, which bypasses RLS unconditionally — `FORCE ROW LEVEL SECURITY` forces it for the table *owner*, not for a superuser. `TestCrossTenantIsolation` would have passed with every policy dropped. | same file | **Done** — the suite refuses to run as a superuser and says which role to use instead. |
| **No Prometheus scrape job and no alert rules**, on a service whose every interesting failure produces no 5xx and no latency. | `deployments/prometheus.yml`, `prometheus-rules.yml` | **Done** — scrape job plus six alerts, each one checked by the audit to read a series that exists and to point at a runbook section that exists. |
| **No contract or operational documents**: no `openapi.yaml`, `asyncapi.yaml`, `RUNBOOK.md`, `RELEASE_CERTIFICATE.md` or audit script — the four artifacts that define "complete" for a service here. | — | **Done** — all present; `scripts/audit.sh` re-proves the service against a running stack. |

### Two defects found in the audit script itself

Recorded because an audit that reports a false failure is worse than one that
does not run: it points at working code and costs someone a morning.

- Every `python` helper emitted **CRLF**. CR is not IFS whitespace, so each
  token the shell read back carried a trailing `\r` and every `grep` for it
  failed — five FAILs on checks whose subject was present in the output being
  searched. The queries now live in `scripts/spec_query.py`, which writes LF
  only.
- The runbook-pointer check scanned the **whole** rules file, so
  gateway-auth-svc's and secret-vault-integration-svc's `RUNBOOK section N.N`
  annotations were reported dangling against *this* service's runbook. Now
  scoped to this service's alert group.

---

## Not done, and deliberately

- **No workflow approval step.** §9.3 says "approval-bound", and what is bound
  here is *authorization* — the delegator must hold the authority, and an
  administrator needs a second grant. No route submits a delegation to
  workflow-svc for a second human to approve. Building one would mean choosing
  an approval policy (which action types need it, at what seniority) that
  belongs to the governance owners rather than to this service. It is the one
  §9.3 clause not fully discharged and it is named as such above.

- **No durable evidence row for refusals this service makes itself.** Doc 04
  §6.3 says denials matter evidentially. Refusals that reach authorization-svc
  are recorded there; the two this service decides alone — `self_dealing` and
  `delegator_mismatch`, which are precisely the escalation attempts — exist
  only as a metric and a log line. Those are the refusals most worth having in
  evidence. Adding an evidence table here is the right fix and was not made in
  this pass.

- **The pre-existing write paths are not migrated onto the outbox in full.**
  `CreateDelegation` and `RevokeDelegation` enqueue correctly. There is no
  other producer in this service, so this is complete for the paths that exist
  — noted only because the estate-wide row 63 tracks outbox rollout per
  service and this service should be counted as done.

- **`Idempotency-Key` is required but not used to deduplicate.** The envelope
  contract demands it on writes and the middleware enforces its presence;
  actual replay protection comes from `(tenant_id, correlation_id)`, which is a
  different key. A retried write with a fresh correlation id and a repeated
  idempotency key creates a second grant. Same gap as tenant-entity-registry-svc.

- **Expiry cannot be turned off.** There is deliberately no `false` setting for
  the sweeper, and an unparseable `EXPIRY_SWEEP_INTERVAL` falls back to the
  default rather than disabling it. Given what the missing sweep did, a config
  typo must not be able to reproduce it.

- **A second delegation surface exists elsewhere in the estate.**
  authorization-svc serves `/v1/admin/delegated-authorities` and the console
  renders it at `/admin/access-control`, separately from this service's
  `/v1/delegations/` at `/admin/delegations`. Doc 03 §9.3 makes *this* service
  the authoritative owner of the concept, and `docker-compose.yml` says so in a
  comment. Reconciling them is a breaking change to another service's console
  and needs a team decision; it is not this service's to make unilaterally.
  Recorded here so the duplication is not mistaken for an oversight.

---

## Re-proving this

```bash
cd services/delegated-authority-svc && bash scripts/audit.sh
```

`SKIP_FE=1` skips the console section. `TEST_DATABASE_URL` must point at a
**scratch** database owned by a **`NOSUPERUSER NOBYPASSRLS`** role — the suite
drops its tables, and a superuser would make the isolation assertions
meaningless.

One environment note that cost a run: `deployments/init-db.sh` applies
migrations **only on a fresh Postgres volume**. Migrations `000003` and `000004`
therefore do not reach an existing stack on `docker compose up`, and the symptom
is the outbox relay logging `relation "delegation_outbox" does not exist` every
250ms while the service reports healthy. Apply them by hand against a running
volume.
