# evidence-requirements-svc — Progress

## Status: NOT STARTED — design captured, zero code written

Spec §8.6 of `docs/architecture/03-microservices.md`; entity at
`docs/architecture/04-data-model.md` §7.1. Full design rationale,
ownership boundary, schema, and open decisions live in `context.md` —
this file tracks **execution state only** and deliberately does not
restate rationale.

**Port:** `8130` · **Group:** Governance Platform (§5.1), the 7th and last
· **Depends on:** `postgres`, `kafka`, `authorization-svc`, and
`document-vault-svc` (conditional — see D2)

---

## Legend — verification honesty

This project has a standing rule against claiming verification that did
not happen (origin: the Kubernetes `kind_run_log.md` fabricated-boot-
evidence incident, corrected in commit `7377db4`). Every box below carries
one of these, and **`[~]` must never be silently upgraded to `[x]`**:

| Mark | Meaning |
| --- | --- |
| `[ ]` | Not started |
| `[~]` | **Written but NOT executed/verified** — code exists, nothing proved it works |
| `[x]` | **Verified for real** — actually ran, against real infra, output observed |
| `[!]` | Blocked — see the note on the line |
| `[-]` | Out of scope for v1, deliberately (rationale in `context.md` §10) |

**Environment constraint as of 2026-07-27:** Docker daemon is live
(server 29.3.1) → compilation and full HTTP lifecycle **can** be verified
for real. Go toolchain is **not installed** → `go build`, `go vet`,
`go test`, `go mod tidy` cannot run locally. Consequence: test files will
reach `[~]` and stop there until a real CI run. Do not mark them `[x]`.

---

## Phase 0 — Decisions & prerequisites  ⟵ **START HERE, BLOCKING**

Four open decisions from `context.md` §11. D1 and D2 change the schema and
the dependency list, so resolving them after Phase 2 means rework.

- [ ] **D1 · `legal_entity_id` on `evidence_requirements`?** Data model
      §7.1 omits it; doctrine requires it on every material record.
      Proposed: nullable, NULL = tenant-wide. **Blocks Phase 2.**
- [ ] **D2 · Does `/evaluate` verify artifact references against
      `document-vault-svc`, or trust the caller's asserted list?**
      Proposed: verify `SUPPORTING_DOCUMENT` refs, fail-closed.
      **Blocks Phase 4** and determines whether document-vault is a
      runtime dependency.
- [ ] **D3 · Tier label.** §5.1 says Governance Platform; §23's Tier 0
      enumeration omits this service. Determines whether it gates other
      services' start under the doctrine Tier-0-exit-criteria rule.
      Non-blocking for code.
- [ ] **D4 · Retrofit posture: does the gate block on
      `NO_REQUIREMENTS_DEFINED`?** Proposed: no — permit, log loudly, and
      seed real catalog rows for both retrofit actions so the happy path
      is genuinely exercised rather than trivially permitted by an empty
      table. **Blocks Phase 10.**

Prerequisites:

- [ ] Confirm `8130` still free in `deployments/docker-compose.yml`
      (`8080`–`8129` were contiguous at last check; re-verify — another
      branch may have claimed it)
- [ ] Confirm action-code naming with `authorization-svc`'s existing
      convention before minting `EVIDENCE_REQUIREMENT_CREATE` /
      `EVIDENCE_REQUIREMENT_RETIRE`

---

## Phase 1 — Scaffold

Base skeleton on `jurisdiction-rules-svc` (effective-dated governance rule
catalog + admin namespace — closest structural analogue). Take store,
authz, and Kafka details from `purchase-order-svc`, which has the
corrected versions of all three (`context.md` §2.1).

- [ ] `go.mod` / `go.sum` — copy dependency set from
      `jurisdiction-rules-svc` (`go.sum` copies verbatim safely once the
      `require` block matches; checksums are version-specific, not
      module-name-specific)
- [ ] `cmd/server/main.go` — wiring, graceful shutdown
- [ ] `cmd/healthcheck/main.go`
- [ ] `internal/config/config.go` — env-driven, no hardcoded hosts
- [ ] `internal/domain/types.go` — `EvidenceRequirement`,
      `EvidenceEvaluation`, outcome enum, request/response types
- [ ] `internal/middleware/tenant.go` — `X-Tenant-Id` required, **`400` on
      absent; never a `"default-tenant"` fallback** (live defect in
      `offboarding-severance-svc` / `workforce-compliance-svc`)
- [ ] `internal/health/health.go`
- [ ] `Dockerfile` + `.dockerignore`
- [ ] `context.md` ✅ **done** · `progress.md` ✅ **done**

---

## Phase 2 — Schema & store  ⟵ gated on D1

- [ ] `deployments/migrations/000001_initial_schema.up.sql`
  - [ ] `evidence_requirements` (`context.md` §6.1)
  - [ ] `evidence_evaluations`, append-only (`context.md` §6.2)
  - [ ] Unique `(tenant_id, domain_code, action_type, evidence_type,
        effective_from)`
  - [ ] Unique `(tenant_id, correlation_id)` on **both** tables
  - [ ] RLS enabled + `tenant_isolation_policy` on both
  - [ ] Indexes for the effective-date lookup path
        (`domain_code, action_type, effective_from, effective_to`)
- [ ] `000001_initial_schema.down.sql`
- [ ] `internal/store/pg_store.go`
  - [ ] ⚠️ **`SELECT set_config('app.tenant_id', $1, true)`** — *not*
        `SET LOCAL app.tenant_id = $1`. The latter is invalid Postgres and
        currently hard-breaks 12 services. Reference:
        `purchase-order-svc/internal/store/pg_store.go:46`
  - [ ] Explicit `tenant_id` filter in **every** query — RLS alone is
        insufficient (superuser connection bypasses it)
  - [ ] No UPDATE/DELETE path on `evidence_evaluations`
  - [ ] End-date via atomic CAS `WHERE effective_to IS NULL`

---

## Phase 3 — Catalog API

- [ ] `internal/authz/client.go` — copy `purchase-order-svc`'s
      `HTTPClient.CheckAllowed`, **fail-closed on network error**
      (Phase 5's and two Phase 4 services' clients fail *open*)
- [ ] `POST /v1/admin/evidence-requirements` — authz-gated, idempotent
- [ ] `POST /v1/admin/evidence-requirements/{id}/end-date` — authz-gated,
      CAS, `422` on already-retired
- [ ] `GET /v1/evidence-requirements` — `tenant_id` required, `as_of`
      defaults to now
- [ ] `GET /v1/evidence-requirements/{id}`
- [ ] ⚠️ **Verify `.Authorize`/`.CheckAllowed` is actually *called* on
      every mutating route** — not merely constructed and injected. All
      ten Phase 5 services failed exactly here. Grep the handler before
      ticking this.

---

## Phase 4 — Evaluation engine  ⟵ gated on D2, the core of the service

- [ ] Effective-requirement resolution for
      `(tenant_id, legal_entity_id?, domain_code, action_type)` at
      `as_of`
- [ ] Sufficiency matching of `present_artifacts` against effective
      requirements
- [ ] `requirement_payload` interpretation (min-count, artifact subtype,
      signature class) driven **entirely by data** — zero jurisdiction,
      country, or currency branches in code (doctrine, non-negotiable)
- [ ] Three distinct outcomes: `SATISFIED` / `MISSING` /
      `NO_REQUIREMENTS_DEFINED` — the third must never be collapsed into
      `SATISFIED` (`context.md` §5.3)
- [ ] `unmet[]` names each unmet requirement individually with a reason —
      a bare boolean is not explainable evidence
- [ ] `POST /v1/evidence/evaluate` — idempotent on
      `(tenant_id, correlation_id)`; replay returns the original
      determination
- [ ] `GET /v1/evidence/evaluations/{evaluation_id}`
- [ ] Artifact verification against `document-vault-svc`
      `GET /v1/documents/{id}` — **only if D2 = (b)**; fail-closed
- [ ] Fail-closed matrix implemented as specified (`context.md` §5.4)

---

## Phase 5 — Events

- [ ] `internal/events/publisher.go` — platform-standard envelope
- [ ] `evidence.requirement.satisfied`
- [ ] `evidence.requirement.missing`
- [ ] ⚠️ **`AllowAutoTopicCreation: true` on the `kafka.Writer`** —
      without it every publish silently fails
      `[3] Unknown Topic Or Partition`. 31 of 42 producer services are
      currently in that state. Reference:
      `purchase-order-svc/cmd/server/main.go:128`
- [ ] Replayed evaluation does **not** republish
- [ ] No events beyond the two §8.6 names

---

## Phase 6 — Observability

- [ ] `internal/telemetry/telemetry.go` — copy verbatim from
      `purchase-order-svc` (OTel tracing + Prometheus), consistent with
      the 2026-07-09 platform-wide retrofit
- [ ] `/healthz`, `/readyz` (real DB connectivity check)
- [ ] Structured zap logging with `correlation_id` on every request

---

## Phase 7 — Tests

- [ ] `internal/handler/handler_test.go` — stub store; covers all three
      outcomes, authz deny → `403`, authz unreachable → `503`, missing
      tenant → `400`, idempotent replay
- [ ] `internal/store/tenant_isolation_test.go` — embedded-postgres; RLS +
      explicit-filter isolation, and **directly asserts `set_config`
      works**, which is the one test that would have caught the defect
      shipped in 12 services
- [ ] `[!]` **Cannot execute either locally — no Go toolchain.** These stay
      at `[~]` until a real CI run. Do not upgrade to `[x]` on the basis
      of "looks right."
- [ ] When wiring CI: check how sibling services currently pin
      `embedded-postgres` first — that pin churned through 10 commits in
      ~4 days and the current state is "no pin, use library default,"
      which is the same approach that failed when the default resolved to
      an unreleased Postgres 18.3.0

---

## Phase 8 — Platform wiring

- [ ] `deployments/docker-compose.yml` — service entry, port `8130`,
      `depends_on`, env
  - [ ] ⚠️ Use **compose service keys**, not `container_name` values, in
        `depends_on` — that exact confusion
        (`jurisdiction-rules-svc` vs `jurisdiction-svc`) was a hard schema
        error that blocked `docker compose up` for the entire file
  - [ ] ⚠️ If a `JURISDICTION_RULES_URL` is ever needed here, it is
        `http://jurisdiction-svc:8082` — **not**
        `http://jurisdiction-rules-svc:8081`, which is wrong in both host
        and port and is currently wrong in 12 places in that file
- [ ] `deployments/init-db.sh` — `evidence_requirements` database
- [ ] `.github/workflows/ci.yml` — matrix entry, `TEST_DATABASE_URL`
      condition, store-isolation-test condition
- [ ] `deployments/kubernetes/manifests/24-app-evidence-requirements.yaml`
      (next free index; `23` is purchase-order)
- [ ] `docs/postman/ZoikoSuite_EvidenceRequirements.postman_collection.json`
- [ ] `[-]` `services/README.md` — already stale past `8102`; not fixing
      that pre-existing gap here

---

## Phase 9 — Live verification (Docker is available — do this for real)

Everything here is genuinely achievable in this environment. Nothing below
gets ticked from inference.

- [ ] `docker compose up -d --build evidence-requirements-svc` — first
      real proof it compiles
- [ ] Migration applies cleanly against real Postgres 16 (tables,
      indexes, both RLS policies, both unique constraints)
- [ ] `/healthz` + `/readyz` → `200`
- [ ] Seed real `EVIDENCE_REQUIREMENT_CREATE` / `_RETIRE` role +
      permission bundle + assignment via `authorization-svc`'s actual
      admin API — no stubs
- [ ] Authz gate proved **both** ways: `403` deny-by-default before the
      role is granted, `503` when authorization-svc is unreachable
- [ ] Catalog create → `201`; replay same `correlation_id` → `200`,
      identical body
- [ ] `/evaluate` → `NO_REQUIREMENTS_DEFINED` on an empty catalog
- [ ] `/evaluate` → `MISSING` with a correctly named unmet requirement
- [ ] `/evaluate` → `SATISFIED` once the artifact is supplied
- [ ] Evaluation replay → original determination, **no duplicate event**
- [ ] End-date → `200`; second end-date → `422`
- [ ] Missing `X-Tenant-Id` → `400` (never `"default-tenant"`)
- [ ] ⚠️ **Kafka events confirmed by a real `kafka-console-consumer` read**
      of the topic — full envelope inspected. A mock-publisher counter
      does **not** count; that is precisely how the platform-wide silent
      Kafka failure went unnoticed across many "verified live" sign-offs
- [ ] `[!]` `go test` — still not runnable (no Go toolchain). Needs CI.

---

## Phase 10 — Retrofit two finalization paths  ⟵ gated on D4

This is what makes §8.6's constraint real instead of theoretical.
Two call sites only — deliberately, see `context.md` §9.1.

- [ ] `general-ledger-svc` → `POST /v1/journals/{journal_id}/post`
      (`internal/handler/handler.go:68`)
  - [ ] Call `/v1/evidence/evaluate` before the state transition
  - [ ] Block on `MISSING` → `422` naming the unmet requirements
  - [ ] Fail-closed `503` on unreachable
  - [ ] Seed real catalog rows for `FINANCE` / `JOURNAL_POST` so the
        happy path is genuinely exercised, not trivially permitted
- [ ] `financial-close-svc` → `POST /v1/close/periods/{id}/lock`
      (`internal/handler/handler.go:80`) — same four steps,
      `FINANCE` / `PERIOD_LOCK`
- [ ] Live end-to-end: a journal post **blocked** for missing evidence,
      then **permitted** once the evidence exists
- [ ] `[-]` `board-resolutions-svc`, `corporate-actions-svc`,
      `vat-gst-svc`, `corporate-tax-svc` — deliberate follow-up once the
      pattern is proven; mechanical repetition, not v1 scope

---

## Phase 11 — Handoff

- [ ] Update this file: every `[~]` accurately distinguished from `[x]`
- [ ] Record the four D-decisions and their resolutions in `context.md`
      §11 (replace "proposed" with what was actually decided)
- [ ] Push the branch and get a **real CI run** — the only way Phase 7
      leaves `[~]`
- [ ] Note in the PR that Governance Platform §5.1 is now 7/7
- [ ] Update the `project-zoikosuite-overview` memory: pending count
      25 → 24

---

## Running notes

*(Append dated entries as work proceeds — findings, surprises,
pre-existing bugs tripped over. Keep the fabricated-evidence lesson in
mind: write what happened, not what should have happened.)*

**2026-07-27** — Service scoped and documented; no code yet. Chosen as the
next build because it is the last gap in the Governance Platform group and
its own critical constraint is currently unenforced platform-wide (zero
references to `evidence-requirements` anywhere in `services/`, six
finalization paths with no evidence gate). Docker verified available
(29.3.1); Go toolchain confirmed absent. Four decisions in Phase 0 are
blocking and should be resolved before Phase 2 begins.

**Standing caveat, carried deliberately:** this becomes service #51 while
the 12 services from `context.md` §2.1(1) still cannot write to Postgres.
Building forward is a legitimate choice, but that number is growing on
unverified ground and the RLS sweep remains the higher-priority fix for
what already exists.
