# ZS-JUR-001 Wave 2: runtime rule resolver

Builds on Waves [0](jurisdiction-pack-wave0.md), [1](jurisdiction-pack-wave1.md) and [3](jurisdiction-pack-wave3.md).
Implemented in `jurisdiction-rules-svc` (package `internal/resolver`, migration `000008`). Follows ZS-JUR-001 s9, s25, s27, s30
and JUR-NEG-03, 04, 07, 22, 24.

## What is built

`POST /v1/rule-resolutions:resolve` answers "which rule applies" for a jurisdiction code, rule domain, rule code and instant,
from **signed, certified pack artifacts only** (never live rule rows, never a government website), and records every answer as
evidence. Also: `GET /v1/rule-decisions/{id}`, `GET /v1/pack-coverage`, `POST /v1/admin/resolver-cache:invalidate`.

### How a pack is chosen
Eligible pack versions (status `RELEASED`) whose scope names the jurisdiction and whose own effective window contains the instant;
per pack, the **highest version**. Window start is inclusive, end exclusive. Several packs may be consulted. A request may restrict
to one `pack_ref`, or pin `pack_ref` + `pack_version` to **replay** a past decision against the exact version it used (allowed for
RELEASED and SUPERSEDED, never for WITHDRAWN or EMERGENCY_BLOCKED: s23).

### Verification before use, and failing closed
Before any answer, every consulted pack is verified: artifact digest recomputed, recorded digests compared, signature checked against a
registered non-revoked key, a certification must exist for exactly that digest, and the certification report's own signature must verify.
If **any pack that covers the jurisdiction** fails, the answer is `503 pack_unverified` listing the failures, **not** an answer from an
older or partial pack (which would silently apply an outdated rate). A security event (`jurisdiction-pack.verification-failed`) is
raised when a pack fails on load (JUR-NEG-03, JUR-NEG-04). No evidence row is written for a refusal, because no decision was made.

### Outcomes
| Outcome | HTTP | Meaning |
|---|---|---|
| RESOLVED | 200 | a rule won: by the only candidate, by **declared precedence**, or by a `supersedes` link |
| NO_RULE | 200 | the jurisdiction is supported but no rule applies at that instant (or no covering pack is in force then) |
| AMBIGUOUS | 409 | overlapping rules with no declared precedence, **or two packs defining the same rule** for that jurisdiction and instant (JUR-NEG-24). Blocked, never guessed |
| UNSUPPORTED_JURISDICTION | 422 | no eligible pack names the jurisdiction (JUR-NEG-22). Nothing is guessed |

Within a pack the jurisdiction hierarchy comes from the artifact's own parent links, and winners are chosen only by declared
precedence; there is no implicit "most specific wins" or "latest wins".

### Evidence (`rule_decision_evidence`, append-only)
Each recorded decision stores the canonical request and its digest, the outcome, pack ref/version/id, **artifact digest**,
certification id, rule id and rule content digest, basis, the full response, caller and correlation id. The response also names the
sources (authority, snapshot hash) and interpretation behind the rule, so an outcome can be traced backwards: decision ->
rule version -> pack release -> interpretation -> source (s30). A `RESOLVED` row cannot be stored without its pack and rule basis.
An optional `Idempotency-Key` (scoped to the caller) makes a retry return the original decision; the same key with a different
request is 409.

### Caching and invalidation
Verified artifacts and the eligible-pack list are cached for `RESOLVER_CACHE_TTL_SECONDS` (default 30; `0` = no caching). That TTL
is the **documented upper bound on how long a key revocation or a withdrawal can go unnoticed**, unless the operator calls
`resolver-cache:invalidate` (immediate, but per process: with several replicas, call each).

### Eligibility configuration
`RESOLVER_ELIGIBLE_STATUSES` defaults to `RELEASED`. `CERTIFIED` may be added **only outside production and staging** (the service
refuses to start otherwise), so a lower environment can exercise the resolver before the release workflow exists.

## Release (now built in Wave 7)
Wave 7 adds publish and the deployment rings (see [Wave 7](jurisdiction-pack-wave7.md)). Resolution is opt-in (`RESOLVER_ENABLED`); with a ring and
region configured, a resolver serves only what is deployed to it, and with nothing deployed every request answers UNSUPPORTED_JURISDICTION, which is the
intended fail-closed state.

## Deliberately not built
* **`POST /jurisdiction-decisions:resolve` (s9, s25).** A governed jurisdiction determination needs authoritative entity,
  registration, counterparty, establishment and transaction facts and place-of-supply rules. The entity registry is tenant-scoped and
  requires identity forwarding, and no product rules exist for it. Without them it could only echo the client's claim, which s9 forbids
  as the final authority (JUR-NEG-05). `GET /v1/pack-coverage` is offered instead as an explicit read model of what is supported.
* **`POST /regulatory-calculations:execute`.** Needs a rule formula language and exact-decimal execution (s38, undecided).
* Resolution-rate, deployment-skew and stale-pack telemetry (s29, JUR-NEG-29): only cache counters are exposed.
* Subdivision override only "where the regime permits" (s10): not modelled.
* A shared (multi-replica) cache or push-based invalidation.

## Authorization
`RULE_RESOLUTION_RESOLVE`, `RULE_DECISION_VIEW` (demo seed bundle `JURISDICTION_RESOLVER_CALLER`, for calling services; separate from
the registry/admin bundle) and `RESOLVER_CACHE_INVALIDATE` (operator). Callers need no authoring, signing or certification rights.

## Verification
Unit tests of the resolver (verified decisions, eligibility, unsupported vs not-in-force, version selection and pinned replay,
window boundaries, in-pack and cross-pack ambiguity, fail-closed on eight tamper/trust failures, an unverified newer pack never
bypassed for an older one, cache TTL/invalidation, request validation, config safety) and the integration suite `internal/registryit`
(real store, real certified packs, evidence immutability in SQL, idempotency, tampered artifact and revoked key, bounded cache
staleness, newest-version selection and withdrawn-pin refusal, the 000005 to 000008 down/up round trip).
