# Secret Vault Integration Service — Context

Compiled from `docs/architecture/03-microservices.md` §9.5 and §5.2,
`docs/architecture/05-security.md` §3.7–3.9 and §9.1–9.6, and
`docs/architecture/06-blueprint.md`'s **"PHASE 1 — THE SOVEREIGN SPINE"**
section (verbatim heading, line 466 of that file). No entity for this
service exists in `04-data-model.md` — confirmed by grepping the whole
`docs/architecture/` tree. §7 below is an original design filling that
gap, built directly against the real task spec that authorized this build
(quoted/cited throughout), mirroring `policy-svc` and
`governance-decision-log-svc` where their patterns genuinely apply, and
diverging from them explicitly where this service's own doctrine (secrets
must never be stored, must be rotation-aware, must separate leases from
audit) requires something different. This file has no independent
authority — if it ever disagrees with the source docs, the docs win.

## 0. Correction to this file's own history (2026-07-08)

An earlier draft of this file claimed "Phase 1 — The Sovereign Spine"
doesn't exist anywhere in the docs and classified this service under
`03-microservices.md` §25's separate "BUILD ORDER" list instead (which
puts it in "Phase 0 — Foundation" alongside `identity-context-svc` and
`tenant-entity-registry-svc"). That claim was wrong — caused by a
case-sensitive search miss (`"Phase 1"` vs. the doc's actual `"PHASE 1"`)
— not a real absence. **Both phase-numbering schemes genuinely exist in
this doc set and do not agree with each other**:

- `06-blueprint.md`'s "PHASE 1 — THE SOVEREIGN SPINE" groups this service
  with Identity Context, Tenant & Entity Registry, Policy, Jurisdiction
  Rules, Authorization, Workflow & Approvals, Obligations, Governance
  Decision Log, and Configuration & Feature Flag Service — ten services
  plus infrastructure (API gateway, schema registry, base Kubernetes,
  observability baseline, audit event pipeline bootstrap, Global Traffic
  & Residency Manager) — with an explicit Exit Criteria list including
  **"secrets are centrally managed."** This is the framing the actual
  build task for this service uses, and the one this file now treats as
  primary.
- `03-microservices.md` §25's independent "BUILD ORDER" list puts this
  service in a smaller "Phase 0 — Foundation" (with just Identity Context
  and Tenant & Entity Registry), separate from its own "Phase 1 —
  Governance Spine" (Policy, Jurisdiction Rules, Authorization, Workflow
  & Approvals, Governance Decision Log, Audit Event Store).

Not resolving which is "more correct" here — both are real doc content,
they simply weren't reconciled with each other when written. Recording
this so a future session doesn't rediscover the same confusion.

## 1. What it is

**Service Class:** Foundation / Governance-adjacent Platform Service.
**Tier:** 0. Named explicitly in `06-blueprint.md`'s Phase 1 Sovereign
Spine service list, whose Exit Criteria will not be met platform-wide
until this service exists ("secrets are centrally managed").
**Naming convention:** `-svc` suffix → `secret-vault-integration-svc`.

**Purpose** (`03-microservices.md` §9.5, verbatim):
> Provides secure brokering and controlled retrieval for sensitive
> credentials, bank tokens, signing keys, integration secrets, and
> encryption material references.

**Critical Constraint** (§9.5, verbatim):
> No service may store long-lived sensitive credentials in local
> configuration or source code.

**Scoping note — read this before designing anything else:** this
service does **not** store secrets itself. It is a broker/mediator in
front of a real external vault backend (cloud KMS, HashiCorp Vault, or
equivalent — `05-security.md` §9.2). Do not build a from-scratch secret
storage system. What actually gets built is: access-policy enforcement,
scoped/time-bounded lease issuance, rotation coordination, and an audit
trail — all backed by Postgres for metadata — while actual secret
material lives in whichever vault backend is configured behind a small
interface (§7.6).

## 2. Doctrine context

**`05-security.md` §3.7–3.9** (verbatim, the platform-wide principles this
service exists to operationalize):
- **§3.7 Encryption Is Mandatory, Not Optional** — sensitive data
  protected in transit, at rest, and (where risk justifies it) in use.
- **§3.8 Secrets Must Never Live in Code or Long-Lived Configuration** —
  "All sensitive credentials must be vaulted, rotated, scoped, and
  retrieval-audited."
- **§3.9 Security Must Fail Safely** — security-relevant failure must
  fail closed or degrade in a controlled, documented way.

**§9.1 Secrets Doctrine** (verbatim): no sensitive credential may be
embedded in source code, stored in plaintext config, copied into
deployment artifacts, or exposed to services without scoped retrieval
policy.

**§9.2 Secret Vault Architecture** (verbatim — names acceptable backend
choices, mandates none): "cloud KMS + secret manager; HashiCorp Vault or
equivalent; envelope-encryption patterns for selected data classes."

**§9.3 Secret Classes** (verbatim, complete list — corrected from an
earlier draft of this file which missed the eighth item by reading past
it): database credentials, integration tokens, bank credentials,
e-signature credentials, private keys, encryption-material references,
API signing secrets, **service-to-service trust material**. Per this
repo's "data-driven, not a code switch/case" doctrine (same as
`policy_type` in `policy-svc`), these are data values in a `secret_class`
column, never a Go enum — a new class is a data row, not a code change.

**§9.4 Secret Access Rules** (verbatim): secret retrieval must be scoped
to workload identity, time-bounded where feasible, logged, policy-gated,
and rotation-aware.

**§9.5 Key Management** (verbatim): KMS-backed key management must
support rotation, key versioning, key disable/revoke, region-aware
placement, and access auditability.

**§9.6 Sensitive Key Separation** (verbatim): where required, support
separating tenant encryption domains, document-signing keys,
evidence-integrity keys, and payment-related key scopes. Not designed
into v1 (§7.9) — flagged as a real future requirement, not forgotten.

## 3. Ownership boundary

**Owns** (`03-microservices.md` §9.5, verbatim): vault integration
policy; secret access brokering; secret lease metadata; access audit
references. Note these are **four distinct owned objects** — an earlier
draft of this spec conflated "lease metadata" and "access audit
references" into one table; §7.1 now splits them, per the real task's own
explicit instruction to model them separately.

**Explicitly does not own:** the secret values themselves (the real vault
backend does); authorization/RBAC decisions (Authorization Service,
which doesn't exist yet); evidence storage for other services' decisions
(that's `governance-decision-log-svc`, a peer service, not a dependency
of this one).

## 4. API surface

**Inbound APIs:** not enumerated in `03-microservices.md` §9.5 — §7.2
designs the concrete set, mirroring `policy-svc`'s "resolve applicable
rule for a scope, then act and record evidence" shape for the brokering
endpoint, and `policy-svc`'s CRUD/versioning shape for policy
administration.

**Published Events** (§9.5, verbatim): `secret.access.requested`,
`secret.access.granted`, `secret.rotation.completed`. **Unlike the
earlier draft of this file, `secret.rotation.completed` is in scope for
v1** — the real task requires "rotation coordination" as a first-class
capability, not a deferred nice-to-have, since v1 now has a real (if
local-only) backend capable of actually rotating something (§7.6).

**Consumed Events:** none specified. This service is closer to a leaf
dependency others call into than a consumer of upstream events.

## 5. Evidence & compliance obligations

Every access grant, denial, and retrieval must produce retrievable audit
evidence, at the same evidentiary bar as `governance-decision-log-svc`'s
`governance_decisions` table: append-only, no hard-delete, ever. This is
now a first-class, separately-modeled table (`secret_access_audit_log`,
§7.1) rather than folded into lease records — a denial has no lease to
attach to, and a table that only records grants would hide exactly the
signal (repeated denied attempts) an incident investigation most needs.

## 6. Idempotency & scaling

**Idempotency Requirement:** not stated explicitly in the doc. Resolved
the way `policy-svc`'s `Evaluate` endpoint *should have* shipped from day
one (that service's `decision_id` was optional at first, made required
only after a follow-up pass found the gap — §19–§20 in its own
`context.md`): a caller-supplied `request_id` is **required** on
`POST /v1/secrets/broker` from the start, so a network retry never mints
a duplicate lease or a duplicate audit entry for the same logical request.

**Scaling Characteristics:** not stated. Assume latency-sensitive (any
service needing a credential blocks on this call) and read-heavy on
policy resolution, write-light on lease/audit writes.

## 7. Concrete v1 implementation spec

### 7.1 Schema — four tables, matching the four owned objects in §3
(`secret_policies`+`secret_policy_versions` together model the one owned
object "vault integration policy" as a container+version pair, the same
two-table-per-object shape `policy-svc` uses)

**`secret_policies`** (mirrors `policies` in `policy-svc`):
- `secret_policy_id` — UUID, PK, server-generated
- `secret_class` — VARCHAR, data-driven (§2's full eight-value list)
- `secret_path` — TEXT, the opaque reference/path in the underlying
  vault backend (e.g. `"kv/payroll/db-credential"`) — never the secret
  value itself. **This is the table's unique natural key on its own**
  (not `(secret_class, secret_path)` — a vault path is already a unique
  address by construction, the same way a URL is unique without needing
  a type tag alongside it). This also directly enables
  `POST /v1/secrets/broker` to look a policy up by `secret_path` alone —
  an earlier draft of this endpoint incorrectly keyed off
  `(secret_class, tenant_id, legal_entity_id)`, which cannot distinguish
  between two different secrets of the same class in the same scope.
- `created_at`, `created_by_principal_id`

Idempotent creation dedup key: `secret_path`.

**`secret_policy_versions`** (mirrors `policy_versions`):
- `secret_policy_version_id` — UUID, PK
- `secret_policy_id` — FK
- `tenant_id` — nullable UUID (null = global)
- `legal_entity_id` — nullable UUID
- `allowed_workload_ids` — JSONB array of workload/service/principal
  identifiers permitted to broker this secret in this scope (renamed
  from an earlier draft's `allowed_principal_ids` to match the task's own
  "which workload/role can access which secret class" framing — same
  shape, JSONB array, data not schema)
- `max_lease_duration_seconds` — INTEGER
- `effective_from`, `effective_to` (nullable)
- `version_status` — `DRAFT | ACTIVE | SUPERSEDED | RETIRED`
- `created_at`, `created_by_principal_id`

No UPDATE/DELETE. Dedup key:
`(secret_policy_id, tenant_id, legal_entity_id, effective_from)`. Partial
unique index enforcing at most one ACTIVE version per
`(secret_policy_id, tenant_id, legal_entity_id)` scope — identical
pattern to `idx_policy_versions_one_active_per_scope`.

**`secret_leases`** — grants only, effective-dated and revocable, "same
doctrine as `DelegatedAuthority` elsewhere in the platform — no
hard-delete, ever" (direct instruction from the real task spec):
- `lease_id` — UUID, PK
- `request_id` — TEXT, **caller-supplied, required**, unique — the
  idempotency key (§7.3)
- `secret_policy_version_id` — FK, the version that approved this grant
- `secret_class`, `secret_path` — denormalized from the resolved policy
  at grant time, so this row is self-contained even if the policy is
  later superseded
- `requested_by_principal_id`, `tenant_id`, `legal_entity_id`
- `status` — `GRANTED | EXPIRED | REVOKED` (no `DENIED` here — denials
  never become leases, they only ever exist in the audit log below)
- `granted_at`, `expires_at`
- `revoked_at` — nullable
- `correlation_id`, `created_at`

Only one UPDATE ever allowed: the `GRANTED → REVOKED` transition,
mirroring `transitionVersionStatus`'s generic caller-parameterized shape.
`EXPIRED` is a computed read (`status = 'GRANTED' AND expires_at < NOW()`
reads as expired), never a background job flipping rows — avoids a
scheduler dependency this service doesn't otherwise need.

**`secret_access_audit_log`** — the fourth owned object
("access audit references"), modeled as its own append-only table,
**exactly mirroring `governance_decisions`'s shape and guarantees**
(`governance-decision-log-svc/deployments/migrations/000001_initial_schema.up.sql`):
- `audit_log_id` — UUID, PK
- `event_type` — VARCHAR, data-driven: `REQUESTED`, `GRANTED`, `DENIED`,
  `REVOKED`, `ROTATED`
- `secret_class`, `secret_path`
- `requested_by_principal_id`, `tenant_id`, `legal_entity_id`
- `lease_id` — nullable FK (null for `REQUESTED`/`DENIED` — nothing was
  granted to reference; set for every lease revoked by a `ROTATED` event
  too, §7.2)
- `secret_policy_version_id` — nullable (null for `DENIED` when no
  policy existed at all for that path/scope)
- `request_id` — nullable TEXT; only populated (and only deduped) for
  `ROTATED` entries — see §7.3 for why rotation needs its own dedup path
  distinct from `secret_leases.request_id`
- `outcome_detail` — TEXT, free-form (e.g. why a denial happened)
- `correlation_id`
- `recorded_at`

Partial unique index: `UNIQUE (request_id) WHERE event_type = 'ROTATED'
AND request_id IS NOT NULL` (§7.3).

**No UPDATE, no DELETE, ever** — this is the one table in this service
with zero mutation paths of any kind, matching `governance_decisions`'s
own "append-only evidence table" doc comment verbatim in spirit. Indexed
on `requested_by_principal_id`, `secret_path`, `event_type`, and
`recorded_at` — the same five-dimension queryability
`governance-decision-log-svc` provides.

### 7.2 Endpoints

Health probes (standard, every service in this repo has these):
- `GET /healthz` — liveness.
- `GET /readyz` — readiness (DB connectivity).

Policy administration (mirrors `policy-svc`'s CRUD/versioning exactly):
- `POST /v1/secret-policies` — create the named policy container.
- `POST /v1/secret-policies/{secret_policy_id}/versions` — create a new
  DRAFT version.
- `POST /v1/secret-policies/{secret_policy_id}/versions/{version_id}/activate`
  — DRAFT→ACTIVE, atomically supersedes whatever was previously ACTIVE
  in that scope.
- `GET /v1/secret-policies/{secret_policy_id}/versions` — full version
  history, newest first.
- `GET /v1/secret-policies?secret_class=X&tenant_id=Y&legal_entity_id=Z`
  — "get applicable secret policy set," most-specific-scope first (same
  precedence rule as `policy-svc`'s `FindApplicableVersions`).
  **`secret_class` is required** (`400` if missing) — same posture as
  `policy-svc` requiring `policy_type` on its equivalent endpoint;
  `tenant_id`/`legal_entity_id` are optional (omit for global scope).

The core value — brokering:
- `POST /v1/secrets/broker` — body: `{secret_path, tenant_id,
  legal_entity_id, requested_by_principal_id, request_id,
  correlation_id}`.
  1. Record a `REQUESTED` audit log entry and publish
     `secret.access.requested` — both happen regardless of outcome.
  2. Look up `secret_policies` by `secret_path` (its unique key), then
     the applicable ACTIVE version for `(tenant_id, legal_entity_id)`.
  3. **No policy for that path, or none ACTIVE for that scope → `404`**,
     record a `DENIED` audit entry (`secret_policy_version_id = NULL`).
     This service **defaults to deny-by-absence** — doctrine §9.1's
     "no service may be exposed to a secret without scoped retrieval
     policy" reads as an affirmative-grant requirement, not a caller's
     business judgment call (deliberately different from `policy-svc`'s
     `Evaluate`, which pushes fail-open/fail-closed to the caller because
     Policy Service has no opinion on business risk — this service does
     have an opinion, because the doctrine here is explicit).
  4. If `requested_by_principal_id` is not in the resolved version's
     `allowed_workload_ids` → `403 access_denied`, `DENIED` audit entry
     recorded (`secret_policy_version_id` set this time — a policy
     existed, it just didn't authorize this caller).
  5. Otherwise → grant: compute `expires_at = now() +
     max_lease_duration_seconds`, insert a `GRANTED` lease
     (`ON CONFLICT (request_id) DO NOTHING`, then re-select on conflict —
     idempotent replay returns the original grant verbatim, never a
     second lease), record a `GRANTED` audit entry, call the vault
     backend (§7.6) for the actual lease token, publish
     `secret.access.granted`, return `{lease_id, secret_path,
     lease_token, expires_at}`. **Never return the raw secret value** —
     a lease is a pointer plus a bounded window; redeeming it against the
     real vault is between the caller and that vault.
- `GET /v1/secrets/leases/{lease_id}` — one lease record.
- `GET /v1/secrets/leases?principal=&secret_class=&tenant_id=&from=&to=&limit=&offset=`
  — list/query leases, all filters optional, compose with AND, paginated
  (`limit` default 50 / max 200, `offset` zero-based — mirrors
  `governance-decision-log-svc`'s `List` exactly, including its
  pagination defaults, not just its five filter dimensions).
- `POST /v1/secrets/leases/{lease_id}/revoke` — `GRANTED → REVOKED`,
  idempotent no-op if already `REVOKED`, `409` if attempting to revoke a
  non-`GRANTED` lease. Records a `REVOKED` audit entry.
- `POST /v1/secret-policies/{secret_policy_id}/rotate` — body:
  `{request_id}` (**required** — see idempotency note below). Triggers
  the vault backend's `Rotate` (§7.6) for that policy's `secret_path`,
  records a `ROTATED` audit entry, publishes `secret.rotation.completed`.
  Does **not** change the policy's access rules (who's allowed) — only
  the underlying secret material. `404` if the policy doesn't exist.
  **Also transitions every currently-`GRANTED` lease for that
  `secret_path` to `REVOKED`** (same `revoked_at`, a `REVOKED` audit
  entry per lease) — this was missing from the first pass of this design
  entirely. Without it, rotating a secret would silently leave old
  leases reporting `GRANTED` with a `lease_token` pointing at material
  that no longer exists at the backend — a real security-relevant
  inconsistency, not a cosmetic gap. Rotation and mass-revocation happen
  in one transaction, same all-or-nothing guarantee as `policy-svc`'s
  supersede-then-activate.
  **Idempotency**: `request_id` required and deduped the same way as
  `POST /v1/secrets/broker` (§7.3) — without this, a retried rotate call
  would generate a second real rotation and a second wave of
  lease-revocations for what was meant to be one logical action.
- `GET /v1/secrets/audit?principal=&secret_path=&event_type=&from=&to=&limit=&offset=`
  — query the audit log directly (distinct from the leases list — this
  surfaces `DENIED`/`REQUESTED`/`ROTATED` entries too, not just grants).
  Same pagination contract as the leases list above.

**Correction found during live verification, not part of this section's
original design (2026-07-08):** nothing above ever gave a way to actually
call `VaultBackend.Put` — every endpoint either reads material (`Get`) or
replaces it (`Rotate`), and neither works without material already
existing. `Broker`'s grant path was completely unreachable end to end
until this was caught by actually running the service, not by re-reading
the spec. Added:
- `POST /v1/secret-policies/{secret_policy_id}/material` — body:
  `{material_base64}`. Administrative seeding only, never invoked from
  the broker request path. `404` if the policy doesn't exist, `400` if
  `material_base64` is missing or not valid base64. Calls
  `VaultBackend.Put` with the decoded bytes against the policy's
  `secret_path`.

### 7.3 Idempotency design

`request_id` is required on `POST /v1/secrets/broker` from day one (§6) —
`INSERT ... ON CONFLICT (request_id) DO NOTHING` on `secret_leases`,
mirroring `governance-decision-log-svc`'s `CreateDecision` dedup exactly.

`POST /v1/secret-policies/{id}/rotate` needs its own dedup mechanism —
it never creates a `secret_leases` row, so it can't reuse that table's
`ON CONFLICT`. `secret_access_audit_log` (§7.1) gains a nullable
`request_id` column with a **partial** unique index,
`UNIQUE (request_id) WHERE event_type = 'ROTATED' AND request_id IS NOT NULL`
— only rotation events are deduped this way; `REQUESTED`/`GRANTED`/`DENIED`/
`REVOKED` entries don't need it (they're already implicitly deduped via
the `secret_leases.request_id` uniqueness one layer up, or via the
`GRANTED`-only allowed-prior on revoke). A retried rotate with the same
`request_id` hits the conflict, and the handler returns the original
`ROTATED` entry's outcome instead of rotating again.

Policy administration endpoints reuse `policy-svc`'s exact dedup/transition
patterns verbatim — see that service's `context.md` §13.3.

### 7.4 Failure mode

- Store unreachable → `503`, fail closed — never grant when the ability
  to durably record the grant is in doubt (`05-security.md` §3.9).
- No applicable policy → `404`, treated as an implicit deny (§7.2 item 3).
- Not an authorized workload → `403`.
- Vault backend call fails (real backend, post-v1) → must also fail
  closed (error, never a fabricated lease token) — the v1 local-file
  backend (§7.6) has no real failure mode to design around yet since it's
  a trivial local implementation, but this contract must hold once a real
  KMS/Vault client replaces it.

### 7.5 Evidence

`secret_access_audit_log` (§7.1) **is** this service's evidence
obligation, fully met from day one — no follow-up pass needed the way
`policy-svc` needed one (§19 in that service's `context.md`) to bolt
evidence recording on after the fact. Whether to *additionally* forward
entries to `governance-decision-log-svc` is a separate, deferred decision
— not required by anything in this doc, and this service's own audit log
already meets the same evidentiary bar independently.

### 7.6 The vault backend — real local implementation, not a fake stub

**This is the one place the real task spec is meaningfully stricter than
this file's first draft.** The earlier version proposed a trivial stub
returning a fake deterministic token. The actual instruction: "define a
small backend interface (e.g. `VaultBackend` with `Get`/`Put`/`Rotate`)
and ship a simple local implementation (encrypted-at-rest file or
similar) behind it — production would swap in a real Vault/KMS client.
This mirrors how `identity-context-svc`'s `envelope_signing_key.pem`
already works as a local stand-in for what should eventually be
KMS-backed."

```go
type VaultBackend interface {
    Get(ctx context.Context, secretPath string) (leaseToken string, err error)
    Put(ctx context.Context, secretPath string, material []byte) error
    Rotate(ctx context.Context, secretPath string) error
}
```

v1 ships a `LocalFileVaultBackend`: secret material is stored
encrypted-at-rest in a local file (AES-GCM, key from an env var —
**itself a local stand-in for the exact problem this service exists to
eventually solve**, same bootstrapping compromise
`identity-context-svc`'s `JWT_SIGNING_PRIVATE_KEY_PATH`/`envelope_signing_key.pem`
already accepted). `Get` decrypts and returns an opaque lease token
(never the raw material itself — see §7.2 item 5). `Rotate` generates
new material and re-encrypts. This makes rotation genuinely testable
end-to-end against real Postgres + a real (if local) backend, not
mocked away — a materially stronger v1 than the original stub-only plan.

**Still true, and still worth saying plainly**: swapping in a real
HashiCorp Vault or cloud KMS client behind this same interface is future
work requiring an infra decision this service cannot make unilaterally
(§7.7). What v1 proves is that the brokering/leasing/policy/audit logic
is correct — not that production secret custody is solved.

### 7.7 Concrete first consumer (already exists, already blocked on this)

`identity-context-svc` has three live TODO comments naming this exact
service as the intended fix, confirmed by reading the actual source:

- `internal/config/config.go:16` — `"Production: RS256 via KMS-backed
  keypair through Secret Vault Integration Service."`
- `internal/config/config.go:20` — `"TODO: replace JWTSigningSecret with
  KMS key reference before Phase 1 production cutover."`
- `internal/auth/jwt.go:74–76` — `"TODO: migrate to RS256 with a
  KMS-backed private key via Secret Vault Integration Service before
  Phase 1 production cutover. The public key must be published to a JWKS
  endpoint..."`

Today `identity-context-svc` signs its envelope JWTs with HS256 using a
shared secret (`JWT_SIGNING_SECRET` env var) or reads an RSA private key
directly off local disk (`JWT_SIGNING_PRIVATE_KEY_PATH`, default
`./envelope_signing_key.pem`) — exactly the kind of "local file" pattern
this service is meant to replace. **Not part of this build's v1 scope**
(wiring `identity-context-svc` to actually call this service is a
separate follow-up task, once this service exists) — but it's the
concrete proof this isn't a speculative service nobody will use.

### 7.8 `cmd/healthcheck` — a note on the scaffold instruction

The task's scaffold list includes `cmd/healthcheck/main.go` "for the
distroless-compatible healthcheck, same pattern used platform-wide."
**Checked directly: this pattern does not actually exist anywhere in the
repo yet** — no service has a `cmd/healthcheck` binary, and no
`Dockerfile` in this repo has a `HEALTHCHECK` instruction (confirmed by
grepping every service). This is the same class of stale assumption as
`configuration-feature-flag-svc`'s port-8084 mix-up — build it anyway,
since distroless images genuinely have no shell/`wget`/`curl` for a
Docker `HEALTHCHECK` directive to invoke, so the need is real even though
"platform-wide" overstates how established the pattern currently is.
This will be the **first** service in the repo with this pattern, not a
copy of an existing one — mirror the shape (`http.Get` against
`http://localhost:$PORT/healthz`, exit `0` on `200`, exit `1` otherwise)
but don't go looking for a reference implementation that isn't there.

### 7.9 Port

**8087** — checked `services/README.md` directly (both the current table
and an older stray fragment further down that file still listing
`policy-svc`): `8080`–`8086` are all claimed
(`identity-context-svc`=8080, `tenant-entity-registry-svc`=8081,
`jurisdiction-rules-svc`=8082, `governance-decision-log-svc`=8083,
`audit-event-store-svc`=8084, `policy-svc`=8085,
`configuration-feature-flag-svc`=8086). `8087` is the next free port,
confirmed by direct check, not assumed.

### 7.10 Explicit non-goals for v1

- **No real Vault/KMS backend** (§7.6) — the local-file implementation is
  real and rotation-capable, but swapping in HashiCorp Vault or a cloud
  KMS is an infra decision, not this build's call to make.
- **No Authorization Service integration** for policy admin writes —
  doesn't exist yet, same deferred posture as every other service here.
- **No `identity-context-svc` wiring** (§7.7) — the consumer exists and
  is documented, but actually connecting it is a separate follow-up task.
- **No `secret_class`-level cross-tenant analytics or reporting** —
  nothing in the doc asks for this; `GET /v1/secrets/audit` is a raw
  query surface, not a reporting layer.
- **No caching, ever, not just "not required for v1"** — same permanent
  stance as documented for the parallel gap in
  `configuration-feature-flag-svc`'s own `context.md`: caching anything
  from this service (even a lease token) is a standing security
  anti-pattern here, not a performance decision to revisit later.
- **No §9.6 "Sensitive Key Separation"** (tenant encryption domains,
  document-signing keys, evidence-integrity keys, payment-related key
  scopes as separately-managed key classes) — real, named in the doc, but
  a v2+ concern once basic brokering/leasing/rotation is proven; v1's
  `secret_class` + `tenant_id`/`legal_entity_id` scoping is the
  foundation this would build on, not a replacement for it.

## 8. Tech stack & model policy

Go, consistent with every service in this repo and this service's Tier 0
placement.

## 9. Build sequencing

Named in `06-blueprint.md`'s Phase 1 Sovereign Spine service list (§0
above), whose Exit Criteria explicitly will not be met until "secrets are
centrally managed." This is the last unbuilt service in that ten-service
list to have a concrete implementation spec — `identity-context-svc`,
`tenant-entity-registry-svc`, `policy-svc`, `jurisdiction-rules-svc`,
`governance-decision-log-svc`, and `configuration-feature-flag-svc` all
already exist; `authorization-svc`, `workflow-approvals-svc`, and
`obligations-svc` remain unbuilt and unspecified beyond their own doc
sections.

## 10. Implementation record

Not yet built. Workflow: one branch for the whole service
(`feat/secret-vault-integration-svc`) off `main`, PR when done — never
commit directly to `main`. Verify against real Postgres (Docker) before
calling any batch done, same bar as every other Tier 0 service in this
repo. See `progress.md` (once created) for the batch-by-batch build log.
