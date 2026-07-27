# Evidence Requirements Service — Context

Compiled from `docs/architecture/03-microservices.md` §5.1 and §8.6,
`docs/architecture/04-data-model.md` §7.1 (`EvidenceRequirement`) and §15.1
(`EvidenceManifest` / `EvidenceManifestItem` / `Document`),
`.agents/rules/doctrine.md`, and direct inspection of the already-built
governance-plane and finalization-path services named below.

This file has no independent authority — if it ever disagrees with the
source docs, **the docs win**. Where the docs are silent or internally
inconsistent, that is called out explicitly in §11 as an open decision
rather than resolved silently by this file.

---

## 1. What it is

**Service name:** `evidence-requirements-svc` (`-svc` suffix, platform
convention).

**Service class:** Governance Platform service. `03-microservices.md` §5.1
names exactly seven, and this is the seventh:

| §5.1 Governance Platform Service | Built |
| --- | --- |
| Policy Service | ✅ `policy-svc` |
| Jurisdiction Rules Service | ✅ `jurisdiction-rules-svc` |
| Authorization Service | ✅ `authorization-svc` |
| Workflow & Approvals Service | ✅ `workflow-svc` |
| Obligations Service | ✅ `obligations-svc` |
| **Evidence Requirements Service** | ❌ **this service** |
| Governance Decision Log Service | ✅ `governance-decision-log-svc` |

Building it closes the Governance Plane at 7/7. It is the only remaining
gap in the group the entire platform doctrine rests on.

**Tier:** ambiguous in the spec, and deliberately not resolved here — see
§11.3. `03-microservices.md` §23 enumerates Tier 0 as "Identity,
authorization, policy, jurisdiction, tenant/entity, workflow, audit event
store, secret vault integration" and **does not name this service**, even
though §5.1 places it squarely in the Governance Platform group alongside
five services that *are* Tier 0. This service is built to Tier 0
discipline (non-bypassable, fail-closed, evidential) because that is what
its own §8.6 critical constraint demands, but the tier label itself needs
a human decision.

**Port:** `8130` — `deployments/docker-compose.yml` is contiguously
populated `8080`–`8129` with no gaps (verified by direct enumeration, not
assumed; note `8091` maps to container port `8080`, so the host-port
sequence is unbroken). `8129` is `purchase-order-svc`.

**Purpose** (`03-microservices.md` §8.6, verbatim):
> Determines what supporting evidence must exist before an action may be
> completed.

**Owns** (§8.6, verbatim):
- evidence preconditions
- document requirements
- signature requirements
- supporting artifact rules
- evidence sufficiency logic

**Published Events** (§8.6, verbatim):
- `evidence.requirement.missing`
- `evidence.requirement.satisfied`

**Critical Constraint** (§8.6, verbatim):
> No finalization path may skip required evidence states.

That last line is the whole reason this service is worth building now.
As of today **nothing in this platform enforces it** — see §9.

---

## 2. Doctrine context

Platform-wide invariants from `.agents/rules/doctrine.md` and how each
applies here:

| Doctrine rule | How this service satisfies it |
| --- | --- |
| No domain service self-authorizes | This service *is* governance plane, but it still does not self-authorize its own catalog writes — every admin mutation is gated on `authorization-svc` `/v1/authorize`, fail-closed. Mirrors `jurisdiction-rules-svc`. |
| Every state-changing API and event consumer is idempotent | Catalog writes idempotent on a real unique constraint; evaluations idempotent on `(tenant_id, correlation_id)`. See §7. |
| No soft-delete on material objects | Requirements are retired by effective end-dating (`effective_to`), never deleted. Evaluation records are append-only. |
| Every material record carries `tenant_id`, `legal_entity_id`, `effective_from`/`effective_to` | The spec'd entity has `tenant_id` + effective dates but **omits `legal_entity_id`** — a genuine conflict between §7.1 of the data model and the doctrine line. See §11.1. |
| Events are facts, append-only; never mutate source truth downstream | This service publishes outcome facts only. It never writes to `document-vault-svc`, `workflow-svc`, or any caller. |
| No hardcoded country / jurisdiction / currency / tax value | Nothing jurisdiction-specific is compiled in. Requirements are **data** — `requirement_payload` rows, addable per tenant with no code change or redeploy. This is the single most important doctrine rule for this service: a jurisdiction demanding a notarized signature must be a row, never a `switch` branch. |

### 2.1 Five specific mistakes this service must not repeat

Each of these is a real, currently-live defect found in a full-codebase
audit (2026-07-23, re-verified 2026-07-27) and tracked in the
`zoikosuite-known-issues` project memory. They are listed here because
this service is the next one built and has the chance to not inherit them.

1. **RLS tenant scoping.** Use
   `SELECT set_config('app.tenant_id', $1, true)`. Do **not** use
   `SET LOCAL app.tenant_id = $1` — Postgres does not accept bind
   parameters in `SET`, so it raises a syntax error, and because the
   error is returned and checked, every enclosing write aborts. Twelve
   services currently ship this (all ten Phase 5 services plus
   `offboarding-severance-svc` and `workforce-compliance-svc`). Correct
   reference: `services/purchase-order-svc/internal/store/pg_store.go:46`.
2. **Actually call the authz client.** All ten Phase 5 services construct
   an `authz.Client`, inject it into the handler, and never invoke
   `.Authorize(...)` on any route — the gate is dead code. Correct
   reference: `purchase-order-svc`'s `authz.HTTPClient.CheckAllowed(...)`
   (`internal/authz/client.go:65`), called before the store on every
   mutation.
3. **Kafka `AllowAutoTopicCreation: true`.** `segmentio/kafka-go`'s
   `Writer` defaults it to `false` and never requests auto-creation in
   its metadata calls, so the broker's `KAFKA_AUTO_CREATE_TOPICS_ENABLE`
   is never exercised and every publish fails with
   `[3] Unknown Topic Or Partition` — logged only, never surfaced.
   31 of 42 producer services are still in this state. Correct
   reference: `purchase-order-svc/cmd/server/main.go:128`.
4. **Real idempotency, not a stored `correlation_id`.** Most Finance /
   Workforce / Legal services persist `correlation_id` without a unique
   constraint, so retries mint duplicates. A DB-level constraint is
   required here, not a convention.
5. **Ship a store test.** Phase 5 shipped **zero** store-layer tests
   across all ten services — only `pg_store.go`, no
   `tenant_isolation_test.go`. That absence is the sole reason defect (1)
   survived ten consecutive service builds. A store test is a v1
   deliverable here, not a follow-up.

---

## 3. Ownership boundary

The evidence area already has three built services plus a workflow
engine, so the boundary needs to be exact. The clean framing:

> `evidence-requirements-svc` is the **before**.
> `evidence-manifest-svc` is the **after**.

| Question | Owner |
| --- | --- |
| *May this action happen at all, per policy?* | `policy-svc` |
| *May this principal perform it?* | `authorization-svc` |
| *Has it been approved by the right people?* | `workflow-svc` |
| ***Does the required supporting evidence exist yet?*** | **this service** |
| *Where is the artifact stored?* | `document-vault-svc` |
| *What evidence was ultimately produced?* | `evidence-manifest-svc` |
| *What did governance decide, and why?* | `governance-decision-log-svc` |

**Owns:**
- `EvidenceRequirement` — the effective-dated catalog of preconditions,
  keyed on `(domain_code, action_type, evidence_type)`.
- `EvidenceEvaluation` — an append-only record of every sufficiency
  determination made, so the decision is itself auditable evidence.

**Explicitly does not own:**
- **`Document` / artifact storage / versions / virus-scan status /
  digital signatures** — owned by `document-vault-svc`
  (`POST /v1/documents`, `GET /v1/documents/{documentID}`,
  `.../versions`, `.../access-log`). This service never stores artifact
  bytes. It is told which artifacts exist, or it asks — see §11.2.
- **`EvidenceManifest` / `EvidenceManifestItem`** — owned by
  `evidence-manifest-svc` (`POST /v1/evidence-manifests`,
  `GET /{manifestID}`, `GET /{manifestID}/records`). That service
  *assembles a pack after the fact*, with `integrity_hash` per item. This
  service *blocks an action before the fact*. Different verb, different
  time, no overlap. Note the two are complementary and should stay
  decoupled in v1: this service does **not** call
  `evidence-manifest-svc`, and manifest generation is not a precondition.
- **Approval state / signature collection workflow** — owned by
  `workflow-svc`. §8.6 gives this service "signature requirements",
  which is the *rule that a signature is required*, not the process of
  collecting one and not the signature artifact itself
  (`Document.digital_signature_id` is document-vault's).
- **Obligation tracking** — `obligations-svc` (Phase 1) and
  `obligation-tracking-svc` (Phase 5). An obligation is a duty with a
  deadline; an evidence requirement is a precondition on an action.
  Related but distinct. Note these two existing services already overlap
  each other with no agreed boundary — do **not** add a third
  participant to that ambiguity by absorbing obligation semantics here.

---

## 4. API surface

Mirrors `jurisdiction-rules-svc`'s proven split: public read paths plus a
separate `/v1/admin/...` namespace for catalog mutation
(`internal/handler/handler.go:112-121`).

| Method | Path | Purpose |
| --- | --- | --- |
| `POST` | `/v1/evidence/evaluate` | **The point of the service.** Sufficiency determination for one pending action. Publishes `evidence.requirement.satisfied` or `evidence.requirement.missing`. Idempotent on `(tenant_id, correlation_id)`. See §5. |
| `GET` | `/v1/evidence-requirements` | List effective requirements. Filters: `tenant_id` (required), `domain_code`, `action_type`, `as_of` (defaults to now). |
| `GET` | `/v1/evidence-requirements/{evidence_requirement_id}` | Get one. |
| `GET` | `/v1/evidence/evaluations/{evaluation_id}` | Retrieve a past determination as evidence. |
| `POST` | `/v1/admin/evidence-requirements` | Create a requirement. Authz-gated (`EVIDENCE_REQUIREMENT_CREATE`). Idempotent. |
| `POST` | `/v1/admin/evidence-requirements/{id}/end-date` | Retire by setting `effective_to`. Authz-gated (`EVIDENCE_REQUIREMENT_RETIRE`). Never a DELETE. |
| `GET` | `/healthz` | Liveness. |
| `GET` | `/readyz` | Readiness (DB connectivity). |

**Consumed events:** none in v1. Evaluation is synchronous request/response
because it is a **blocking precondition** on a caller's in-flight
transaction — an async consumer cannot gate a finalization the way §8.6
requires. This matches how every built service performs cross-service
verification today (`accounts-receivable-svc` → `general-ledger-svc`,
`purchase-order-svc` → `purchase-request-svc`): synchronous HTTP, not
event-driven.

**Published events:** `evidence.requirement.missing`,
`evidence.requirement.satisfied` (§8.6, verbatim — no others invented).
Envelope shape identical to every other producer on the platform; see any
`internal/events/publisher.go`.

---

## 5. Evaluation semantics — the core contract

### 5.1 Request

```
POST /v1/evidence/evaluate
X-Tenant-Id, X-Principal-Id, X-Correlation-ID

{
  "legal_entity_id":  "<uuid>",
  "domain_code":      "FINANCE",
  "action_type":      "JOURNAL_POST",
  "present_artifacts": [
    { "evidence_type": "SUPPORTING_DOCUMENT", "reference_id": "<document_id>" },
    { "evidence_type": "APPROVAL_RECORD",     "reference_id": "<workflow_instance_id>" }
  ]
}
```

### 5.2 Response

```
{
  "evaluation_id": "<uuid>",
  "outcome":       "SATISFIED" | "MISSING" | "NO_REQUIREMENTS_DEFINED",
  "unmet": [
    {
      "evidence_requirement_id": "<uuid>",
      "evidence_type":           "SIGNATURE",
      "reason":                  "required signature evidence not present"
    }
  ],
  "evaluated_at":   "<rfc3339>",
  "correlation_id": "<passthrough>"
}
```

### 5.3 Three outcomes, not two — and why

`SATISFIED` and `MISSING` are the two events §8.6 names. A third
**response** outcome is needed to stay honest:

- **`SATISFIED`** — every effective requirement matched by an artifact.
  Publishes `evidence.requirement.satisfied`.
- **`MISSING`** — one or more requirements unmet, each named
  individually. Publishes `evidence.requirement.missing`. Callers must
  treat this as a block.
- **`NO_REQUIREMENTS_DEFINED`** — no effective requirement rows exist for
  this `(domain_code, action_type)`. An empty catalog is a **legitimate
  data state**, not a failure, so it must not be reported as `MISSING`.
  But it must equally not be silently collapsed into `SATISFIED`, because
  that makes "nobody configured this yet" indistinguishable from
  "verified complete". It is returned as its own value and logged.

That distinction is drawn deliberately in reaction to a real defect:
`tax-determination-svc` returns a synthetic `"ZERO-TAX"`/`"FALLBACK-TAX"`
rule at 0% when `tax-rules-svc` is unreachable or has no match
(`internal/rules/client.go`), which lets a transaction post with silently
zeroed tax. The lesson is not "never return a permissive answer" — it is
**never fabricate an authoritative-looking answer to paper over absent
data**. Hence a distinct third value the caller cannot mistake for
verification. What a caller should *do* with it is §11.4.

### 5.4 Failure modes — fail-closed

| Condition | Result |
| --- | --- |
| Store unreachable | `503`. Never a permissive default. |
| `authorization-svc` unreachable (admin routes) | `503`. Deny on unreachability — note the audit found Phase 5 and two Phase 4 authz clients fail **open** on network error; this one must not. |
| `authorization-svc` denies | `403`. |
| Malformed / missing `tenant_id` | `400`. Never a `"default-tenant"` fallback (a real defect in `offboarding-severance-svc` / `workforce-compliance-svc`). |
| Asserted document verified **absent** or out of scope | The artifact does not count → contributes to `MISSING`, with the reason stated. Never assumed present. |
| `document-vault-svc` **unreachable** | `503`, and **no evaluation is recorded**. This is deliberately different from the row above: the determination could not be made, and writing `MISSING` off the back of an infrastructure outage would put a false fact into an append-only evidence ledger. An honest 503 is better than a durable lie. |
| Malformed `requirement_payload` | The requirement is reported **unmet**, not skipped — a rule that cannot be evaluated must block the action rather than silently vanish from the gate. |
| End-dating an already-retired requirement | `422`, not a silent no-op. |

---

## 6. Schema

Two tables. `evidence_requirements` fields come from
`04-data-model.md` §7.1 verbatim; anything beyond it is flagged.

### 6.1 `evidence_requirements`

| Column | Notes |
| --- | --- |
| `evidence_requirement_id` | UUID, PK, server-generated |
| `tenant_id` | UUID, NOT NULL |
| `legal_entity_id` | UUID, **nullable — NOT in the spec'd entity**; see §11.1. NULL = applies tenant-wide |
| `domain_code` | VARCHAR, NOT NULL (e.g. `FINANCE`, `TAX`, `LEGAL`, `WORKFORCE`) |
| `action_type` | VARCHAR, NOT NULL (e.g. `JOURNAL_POST`, `PERIOD_LOCK`) |
| `evidence_type` | VARCHAR, NOT NULL (e.g. `SUPPORTING_DOCUMENT`, `SIGNATURE`, `APPROVAL_RECORD`) |
| `requirement_payload` | JSONB, NOT NULL, default `{}` — the extensibility seam. Minimum-count, artifact subtype, signature class, etc. live here as **data**, never as code branches (doctrine §2). |
| `effective_from` | TIMESTAMPTZ, NOT NULL |
| `effective_to` | TIMESTAMPTZ, nullable — NULL = currently effective. Retirement path; no soft-delete flag, no DELETE. |
| `created_at`, `created_by_principal_id` | Provenance |
| `correlation_id` | NOT NULL, unique per tenant (idempotency) |

Unique: `(tenant_id, domain_code, action_type, evidence_type,
effective_from)` — makes the effective-dated catalog genuinely
non-duplicating, in the same spirit as `tax-rules-svc`'s
`(tenant_id, jurisdiction_id, rule_code, version)`, one of the few real
DB-level idempotency constraints on the platform.

### 6.2 `evidence_evaluations` (append-only — no UPDATE, no DELETE, ever)

| Column | Notes |
| --- | --- |
| `evaluation_id` | UUID, PK |
| `tenant_id`, `legal_entity_id` | NOT NULL |
| `domain_code`, `action_type` | The action being gated |
| `outcome` | `SATISFIED` / `MISSING` / `NO_REQUIREMENTS_DEFINED` |
| `unmet_payload` | JSONB — which requirements were unmet, frozen at decision time |
| `present_artifacts_payload` | JSONB — what the caller asserted was present |
| `evaluated_at`, `evaluated_for_principal_id` | |
| `correlation_id` | NOT NULL, unique per tenant → replay-safe |

This table is what makes the service's own decisions auditable, satisfying
`03-microservices.md` §17.6 ("Every Material Service Must Be Evidential")
rather than only enforcing evidence on others.

### 6.3 RLS

Both tables: RLS enabled with a `tenant_isolation_policy`, **plus** an
explicit `tenant_id` filter in every store query. Defense-in-depth is
mandatory, not redundant — this platform connects as a Postgres superuser,
which unconditionally bypasses RLS. That is the lesson
`general-ledger-svc` and `tenant-entity-registry-svc` learned through CI
failures. And the `set_config` form from §2.1(1), not `SET LOCAL`.

---

## 7. Idempotency

- **Catalog create:** `INSERT ... ON CONFLICT (tenant_id, correlation_id)
  DO NOTHING`, then re-select — `201` first time, `200` with an identical
  body on replay.
- **Evaluate:** same pattern on `evidence_evaluations`. A replayed
  evaluation returns the original determination and **does not republish**
  its Kafka event. `purchase-order-svc` proved this behaviour against a
  real Kafka consume rather than a mock counter; do the same here.
- **End-date:** atomic CAS, `WHERE effective_to IS NULL`. Second call →
  `422`, never a double-apply. `invoice-approval-svc`'s non-atomic
  read-then-write is the anti-pattern to avoid.

---

## 8. Tech stack

Go, chi, pgxpool, `segmentio/kafka-go`, zap, OpenTelemetry tracing +
Prometheus metrics via a copied `internal/telemetry` package — consistent
with all 50 built services.

Note the known on-paper divergence: `.agents/rules/tech-stack.md`
nominally calls for Node/TypeScript outside Tier 0. Every built Phase
1–5 service is Go. This service follows **actual platform convention**,
and being a Governance Plane service it is on the Go side of that rule
under either reading.

Scaffold from `jurisdiction-rules-svc` (closest analogue: effective-dated
governance rule catalog + evaluation endpoint + admin namespace), taking
the store/authz/Kafka corrections from `purchase-order-svc` per §2.1.

---

## 9. Why now: the constraint is currently unenforced

§8.6 says *"No finalization path may skip required evidence states."*
Every finalization path built to date skips them, because there is nothing
to call. Verified by direct route inspection, with **zero references to
`evidence-requirements` anywhere in `services/`**:

| Service | Terminal route | Evidence gate today |
| --- | --- | --- |
| `general-ledger-svc` | `POST /v1/journals/{journal_id}/post` (`handler.go:68`) | none |
| `financial-close-svc` | `POST /v1/close/periods/{id}/lock` (`handler.go:80`) | none |
| `board-resolutions-svc` | resolution pass | none |
| `corporate-actions-svc` | action execute | none |
| `vat-gst-svc` | return filing | none |
| `corporate-tax-svc` | return submission | none |

### 9.1 Retrofit exactly two call sites in v1

**`general-ledger-svc` `/post` and `financial-close-svc` `/lock`** — the
two hardest and highest-value finalization paths, both already correct
about authz and both already using the good `set_config` store pattern.

The remaining four are a deliberate follow-up, not an oversight. Wiring
all six in one branch turns this into a multi-week cross-service change
with a large blast radius and no incremental proof. Two call sites is
enough to prove the gate holds against real infrastructure; the rest is
then mechanical repetition of a validated pattern.

---

## 10. Explicit non-goals for v1

- **No artifact storage.** Never stores bytes; `document-vault-svc` owns
  `Document`.
- **No manifest generation.** `evidence-manifest-svc` owns
  `EvidenceManifest`. No call to it, in either direction.
- **No signature collection or e-signature integration.** E-Signature
  Integration Service (§16.5) is unbuilt Phase 7. This service owns the
  *rule* that a signature is required, nothing more.
- **No requirement-authoring UI or bulk import.** API only.
- **No Kafka consumer** (§4).
- **No jurisdiction-conditional requirement logic in code.** If a
  requirement varies by jurisdiction it is expressed as separate
  effective-dated rows, or inside `requirement_payload` — never a code
  branch. Non-negotiable per doctrine.
- **Only the two events §8.6 names.** No `evidence.requirement.created`
  or similar invented alongside them.

---

## 11. Decisions — two taken and built, two still open

Recorded rather than improvised, per the doctrine line *"When in doubt
about ownership or contract shape, stop and ask — do not improvise
architecture."*

D1 and D2 had to be resolved to write the schema and the evaluator at all,
so they were taken on the stated defaults and **built that way**. They are
reversible but not free — D1 in particular is a schema change once rows
exist. Both still want a human confirmation.

### 11.1 `legal_entity_id` on `EvidenceRequirement` — DECIDED, BUILT
`04-data-model.md` §7.1 lists `tenant_id` but **no `legal_entity_id`**,
while `.agents/rules/doctrine.md` requires *"every material record carries
tenant_id, legal_entity_id, and effective_from/effective_to."* A direct
conflict.

**Decided:** nullable `legal_entity_id`, NULL meaning tenant-wide. It reads
as a rules-catalog entity (like `JurisdictionRule`, which is also
entity-agnostic), and a per-entity override is a plausible real
requirement. `EffectiveRequirements` resolves both — entity-scoped rows for
the entity in question, plus every tenant-wide row.

One consequence worth knowing: the natural-key unique index folds NULL onto
the zero UUID via `COALESCE`, because Postgres treats NULLs as distinct in
a unique index and would otherwise permit unlimited duplicate tenant-wide
rows. See the migration comment.

**Still wants confirmation** — expensive to reverse after rows exist.

### 11.2 Does evaluation verify artifacts, or trust the caller's list? — DECIDED, BUILT
§5.1 has the caller assert `present_artifacts`. Trusting that list means a
caller can claim evidence it does not have — precisely the anti-pattern
the audit flagged in `corporate-actions-svc` (trusting an unverified
`resolution_id`), and which `purchase-order-svc` fixed by synchronously
verifying against the real upstream.

**Decided: verify.** Artifacts with `evidence_type =
SUPPORTING_DOCUMENT` are checked against `document-vault-svc`
`GET /v1/documents/{id}` for existence **and** tenant/legal-entity match
(`internal/documentvault`). A document that is absent or out of scope does
not count toward its requirement. Results are memoised per `reference_id`
within one evaluation, so a requirement set naming the same document twice
does not double-call.

Two scope limits, both deliberate:
- Other evidence types are taken on the caller's word — no service owns
  those references yet. When Delegated Authority / Notification / an
  e-signature service exist, those types become verifiable the same way.
- Document **status** is not gated on. `document-vault-svc` owns that
  lifecycle and no spec section says which statuses count as valid
  evidence, so asserting one here would be scope invention.

### 11.3 Tier classification — STILL OPEN
§5.1 puts this in the Governance Platform group; §23's Tier 0 enumeration
omits it (§1). This matters concretely because doctrine says *"Do not
start a Tier 1 service until its Tier 0 dependency has met its exit
criteria in `06-blueprint.md`"* — so the label determines whether this
service gates others. Either the §23 list is incomplete or the omission is
intentional. **Needs a human answer.** Nothing in the code depends on it.

### 11.4 Is the retrofit advisory or blocking on day one? — STILL OPEN, blocks §9.1
§8.6's constraint is absolute: no finalization path may skip required
evidence. Taken literally, `NO_REQUIREMENTS_DEFINED` on an empty catalog
should still permit the action, or every journal post on the platform
breaks the moment the gate is wired.

*Proposed:* the gate blocks on `MISSING`, permits on `SATISFIED` and on
`NO_REQUIREMENTS_DEFINED`, and **logs loudly** on the latter — with a
seeded catalog for the two retrofit actions so the happy path is genuinely
exercised rather than trivially permitted by an empty table. **Confirm
before wiring `general-ledger-svc` / `financial-close-svc`**, because
"permits on empty catalog" is exactly the shape of the
`tax-determination-svc` defect if nobody ever seeds rows.

This service already does its half honestly: it returns three distinct
outcomes and never disguises an unconfigured gate as a satisfied one. What
a *caller* does with `NO_REQUIREMENTS_DEFINED` is the open question.

---

## 12. Build sequencing & environment

Dependencies, all already built and running in
`deployments/docker-compose.yml`: `postgres`, `kafka`, `authorization-svc`
(admin gating), and `document-vault-svc` (only if §11.2 resolves to (b)).

**Environment as of 2026-07-27, stated plainly:**
- **Docker: available**, daemon live (server 29.3.1). So compilation *can*
  be genuinely verified via the Dockerfile build stage, and a full HTTP
  lifecycle *can* be exercised against real Postgres/Kafka — the same
  route `purchase-order-svc` took.
- **Go toolchain: not installed.** `go build`, `go vet`, `go test`, and
  `go mod tidy` cannot be run locally. `handler_test.go` and
  `tenant_isolation_test.go` will be written but **cannot be executed
  here** — they need a real CI run or a machine with Go.

That gap is recorded here and tracked in `progress.md` rather than glossed
over, per this project's own hard-won lesson from the Kubernetes
`kind_run_log.md` fabricated-boot-evidence incident (commit `7377db4`):
**do not claim verification that did not actually happen.** `progress.md`
distinguishes "written" from "verified" for exactly this reason.
