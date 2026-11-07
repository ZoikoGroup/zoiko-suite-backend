# ZS-JUR-001 s9: jurisdiction determination, and the opt-in "launched only" resolver gate

Builds on Waves [2](jurisdiction-pack-wave2.md) and [8](jurisdiction-pack-wave8.md) and the entity applicability projection (migration `000019`). Implemented in `jurisdiction-rules-svc`
(migration `000022`; action `JURISDICTION_DECISION_RESOLVE` registered in `access-control-svc` migration `000025`). No new service. Follows ZS-JUR-001 s9, s25, s26 and JUR-NEG-05.

## What Wave 2 left out, and what changed
Wave 2 deliberately did not build `POST /jurisdiction-decisions:resolve`: without authoritative entity facts it could only echo the client's claim, which s9 forbids. That blocker is gone: the
service now holds the entity's authorised assignments (the `entity_jurisdiction_applicability` projection of `tenant-entity-registry-svc`'s `entity.created` and `entity.jurisdiction.changed`).

## `POST /v1/jurisdiction-decisions:resolve`
Headers `X-Tenant-Id` (required: assignments are tenant data) and optional `Idempotency-Key`. Body `legal_entity_id`, `purpose` (`TAX`, `FILING`, `ANY`), `effective_at`, optional `claimed_jurisdiction`.

The answer rests on the entity's assignments in force at `effective_at` (end instants are exclusive), filtered by purpose (TAX: `ENTITY_PRIMARY`, `PRIMARY`, `SECONDARY`, `TAX_ONLY`; FILING: `ENTITY_PRIMARY`, `PRIMARY`,
`SECONDARY`, `FILING_ONLY`), with deactivated jurisdictions excluded and the same jurisdiction assigned twice counted once.

| Situation | Outcome | HTTP |
|---|---|---|
| exactly one authorised jurisdiction | `JURISDICTION_DETERMINED` (basis `SINGLE_AUTHORISED_JURISDICTION`) | 200 |
| several authorised, claim selects one of them | `JURISDICTION_DETERMINED` (basis `CLAIM_WITHIN_AUTHORISED_SET`) | 200 |
| several authorised, nothing selects between them | `AMBIGUOUS_JURISDICTION`: candidates listed, nothing guessed | 409 |
| a claim outside the authorised set | `CLAIM_NOT_AUTHORISED`: refused, the authorised set is shown | 422 |
| none authorised | `NO_APPLICABLE_JURISDICTION`: the claim is **never** used as a fallback | 422 |

**The claim is an input fact, not an authority (JUR-NEG-05).** It can choose among jurisdictions the entity is already authorised for, and it can never add one. Each candidate carries its rollout support answer
(`SUPPORTED`, `NOT_YET_SUPPORTED`, `SUSPENDED`, `NO_RELEASED_PACK`, `NO_ROLLOUT`) and the assignment records (id, source event, source version) it rests on.

**Tenant data stays out of the platform ledger.** The decision evidence ledger is platform-wide, not tenant-scoped, so the recorded request holds a sha256 digest of tenant and entity, never their ids, and the answer
holds no entity id. Another tenant's entity id is indistinguishable from an unknown one. Decisions are recorded as `decision_kind JURISDICTION` and announced as `jurisdiction-decision.resolved` (once per decision,
refusals included, not on an idempotent replay).

The route exists only where both the entity projection and the registry are wired, so an unwired deployment exposes exactly what it did before.

## Opt-in gate: serve only LAUNCHED jurisdictions (`RESOLVER_REQUIRE_LAUNCHED=true`)
Closes the Wave 8 follow-up. Off by default, because it changes what existing callers get. When on, every resolver entry point (rule resolution, due-date calculation, payroll parameter sets, and everything built on them:
calculations, retention, mappings, profiles, the s16 evaluations) answers `UNSUPPORTED_JURISDICTION` for a jurisdiction that is not `SUPPORTED` (a LAUNCHED rollout and a RELEASED pack), with the rollout outcome in the explanation.
The gate sits in the resolver's pack selection, so none of those paths can bypass it; if it cannot answer, resolution **fails closed**. Answers are cached for the resolver TTL (`RESOLVER_CACHE_TTL_SECONDS`), so a
suspension takes effect within the TTL, or at once after `POST /v1/admin/resolver-cache:invalidate`.

Trade-off: the gate also applies to pinned replays. Recorded answers remain reproducible through the evidence ledger (`GET /v1/rule-decisions/{id}`); re-running a historical question for a suspended jurisdiction
needs the gate off or the rollout back to LAUNCHED.

## Not built
* **Place of supply, situs and any transaction-level determination** (counterparty location, ship-from and ship-to, supply type, product class): no such rules exist (Wave 4 *Not built*). A transaction fact the caller supplies
  is checked against the authorised set and never decides. s9's "several authorised, nothing selects between them" case therefore stays a review item (409).
* **Counterparty and external facts** (the counterparty's registration, authority acknowledgement, network capability): not inputs yet.
* **Regime determination:** the answer names the jurisdiction; the regime follows from the pack rules resolved for it.
* **Subdivision inheritance:** an entity assigned to a subdivision is determined as that subdivision; ancestor jurisdictions are not added.
* **Event delivery** is the same outbox path as the other registry events; the tests prove emission, not Kafka delivery.

## Deploy order
Apply `jurisdiction-rules-svc` migration `000022` before the new binary, and `access-control-svc` migration `000025` so the action is known. The `000022` down script re-narrows the evidence-kind check as `NOT VALID`
(evidence is append-only). Evidence migrations form a chain: `000021` cannot be reverted and re-applied on its own once `JURISDICTION` evidence exists, so revert and re-apply `000022` before `000021`.

## Verification
Unit: determination tables (single, claim confirmed, claim refused, none, ambiguous, claim selecting, purpose filtering, duplicates, stable ordering); the gate (refuses before looking at packs, covers due dates and parameter sets, fails
closed, caches for the TTL, `Invalidate`, off by default). Integration (embedded Postgres 16): the full claim and ambiguity scenario, tenant isolation, temporal boundaries including the exclusive end instant, deactivated
jurisdictions, evidence holding no tenant or entity ids, idempotent replay without a second event, request validation, route wiring, the gate against a real rollout (refused with no rollout, refused at READY, served when LAUNCHED,
refused again when SUSPENDED, other entry points gated), and the `000005`-`000022` down/up round trip plus `000022` alone.
