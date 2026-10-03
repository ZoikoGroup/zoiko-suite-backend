# ZS-JUR-001 Wave 8: jurisdiction rollout governance

Builds on Waves [0](jurisdiction-pack-wave0.md) to [7](jurisdiction-pack-wave7.md). Implemented in `jurisdiction-rules-svc` (migration `000015`).
Follows ZS-JUR-001 s32, s35, s36, s37, s38 and JUR-NEG-22, 28.

## What Wave 8 is, and what this change is not
s37 describes Wave 8 as "Country/subdivision packs produced one-by-one under separate certification packs." s32 is explicit that the standard "defines the architecture, not the
substantive local law": **a jurisdiction named in the portfolio does not mean ZoikoSuite has certified rules for it, and production support begins only after local expert review
and certification.**

**This change creates no regulatory content.** No tax rate, filing rule, calendar, retention period or mapping for any country was written, because that needs qualified local experts
and would otherwise be an unreviewed claim about the law. What is built is the **governance of the rollout**: the thing that decides, with evidence, when a jurisdiction may be called supported.
Real country packs are authored with the Wave 0-7 machinery, one jurisdiction at a time, by the people qualified to do it.

## The model (migration 000015)
* **Portfolio** (`jurisdiction_rollouts`): one entry per pack family (`gb`, `us.state.ca`, `global.tax.core`), with layer (GLOBAL_REFERENCE, GLOBAL_CORE, REGIONAL_FRAMEWORK, COUNTRY, SUBDIVISION),
  the jurisdiction code it covers, an accountable owner and a support owner. `POST /v1/admin/rollouts:seed-portfolio` adds the ten s32 families as PLANNED with no owner (idempotent);
  per-state and per-province families are added one at a time as they are taken on.
* **Lifecycle:** PLANNED, AUTHORING, READY, LAUNCHED, SUSPENDED, RETIRED. Nothing skips a step; RETIRED is final.
* **Definition of Ready** (s35, `DOR_01..10`) and **Definition of Done** (s36, `DOD_01..11`): each item is attested with an evidence reference by someone who is **not the rollout owner**.
  Append-only: the latest answer for an item is current, so a retracted item blocks the next launch.
* **Qualified expert approvals:** a named expert records their qualification, scope and APPROVE or REJECT. The expert cannot be the owner. An expert's latest decision counts.
* **Pack links:** the packs that make up the rollout.
* **History:** every status change with actor, reason and time.

## The launch gate
* **READY** needs the whole Definition of Ready.
* **LAUNCHED** additionally needs the whole Definition of Done, at least one approving expert, at least one linked pack with a **RELEASED** version, and a launcher who is neither the owner nor an expert on the
  rollout. SUSPENDED and RETIRED (and READY back to AUTHORING) need a reason. A refused move returns 409 with **every** blocker at once, and `GET /v1/admin/rollouts/{id}` shows what blocks each next step.
* **The database enforces the same gates** (status edges, DoR and DoD completeness, expert approval, a released linked pack, owner independence, append-only evidence, no deletes), so a direct SQL
  update cannot launch a jurisdiction. Attacked in the integration tests.

## "Is this jurisdiction supported?" (JUR-NEG-22, JUR-NEG-28)
`GET /v1/jurisdiction-support/{code}` is explicit and never guesses: `SUPPORTED` only when a LAUNCHED rollout names the code **and** a linked pack has a RELEASED version covering it; otherwise `NOT_YET_SUPPORTED`,
`SUSPENDED`, `NO_RELEASED_PACK` (launched, but nothing released covers it, for example after a withdrawal) or `NO_ROLLOUT`, each with an explanation. The match is on the exact code: a subdivision is not
supported because its country is.

## Authorization (demo seed)
Admin bundle `JURISDICTION_PACK_REGISTRY_FULL`: `JURISDICTION_ROLLOUT_CREATE|SEED|VIEW|SET_OWNER|LINK_PACKS|ATTEST|EXPERT_APPROVE|TRANSITION`, `JURISDICTION_SUPPORT_VIEW`. Caller bundle:
`JURISDICTION_SUPPORT_VIEW`. In production these should be held by different roles (owner, attester, expert, launcher); the service enforces the independence, not the role design.

## Not built
* **Any country's rules.** See above. Wave 8 is complete as governance; the content is a continuing programme.
* **The runtime does not yet refuse unsupported jurisdictions by itself.** Resolution still serves any verified RELEASED pack; the support endpoint is the explicit answer clients can use, and an
  opt-in gate in the resolver ("only serve jurisdictions whose rollout is LAUNCHED") is a small follow-up if you want it. Left out because it changes what existing callers get.
* **The attestations are claims with evidence references, not machine checks.** Some items could be derived from the system (DOD_04 certification record, DOD_05 signature verifies, DOD_06 ring
  and region deployment); they are attested by a person today. Automating them would couple the checklist to Wave 3 and Wave 7 state.
* **Expert qualification is free text.** There is no directory of qualified experts or approval model; that is an open pre-production decision (s38, "Jurisdiction expert approval model").
* **Subdivision inheritance, rollout events and dashboards:** no `rollout.*` domain events are published, and there is no portfolio dashboard.
* Rolling a rollout back (RETIRED is final, SUSPENDED is the reversible pause) is separate from pack rollback, which is Wave 7.

## Verification
Unit tests: checklist sizes and portfolio, transition blockers (launch needs every gate, owner and expert cannot launch, retracted item blocks, edges, reasons). Integration (embedded Postgres 16): seeding
and idempotency, explicit support outcomes, the full path from PLANNED to LAUNCHED with every blocker reported, owner and expert independence, evidence required, the retract-and-relaunch cycle,
loss of support when the pack is withdrawn, RETIRED being final, the database guards attacked in SQL, and the 000005-000015 down/up round trip.
