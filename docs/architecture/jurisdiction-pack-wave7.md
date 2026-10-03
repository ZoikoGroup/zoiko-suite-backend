# ZS-JUR-001 Wave 7: release, rollout, hotfix, operations

Builds on Waves [0](jurisdiction-pack-wave0.md), [1](jurisdiction-pack-wave1.md), [2](jurisdiction-pack-wave2.md) and
[3](jurisdiction-pack-wave3.md). Implemented in `jurisdiction-rules-svc` (migration `000009`, which also replaces the 000007
certification guard). Follows ZS-JUR-001 s23, s24, s25, s27, s29, s31 and JUR-NEG-14, 15, 19, 21, 28.

## Release lifecycle
`CERTIFIED` -> **publish** -> `RELEASED`; **block** / **unblock** (`RELEASED` <-> `EMERGENCY_BLOCKED`); **withdraw** (from CERTIFIED,
RELEASED, EMERGENCY_BLOCKED or SUPERSEDED to `WITHDRAWN`). Each is a separate authorization action.

* **Publish** re-verifies the artifact and certification (the single trust definition `domain.VerifyLoadedPack`, shared with the
  runtime resolver, so the two cannot disagree), requires every registered-pack dependency to be RELEASED, and for a hotfix its
  rollback target to be RELEASED. All unmet gates are returned together (`409 operation_blocked`).
* **Withdraw** needs a reason **and** an evidence reference (s25). It is refused while another RELEASED version depends on this one,
  and it rolls the version's ACTIVE deployments back with the reason. The artifact and every past decision stay resolvable (JUR-NEG-28).
* RELEASED, WITHDRAWN and EMERGENCY_BLOCKED are reachable **only** through these commands: a database trigger refuses a bare status
  update without a matching `pack_release_events` row written in the same transaction. The event log is append-only.
* `SUPERSEDED` has no command yet (it is still an allowed database edge); it is not built here.

## Rings, regions and rollout
Rings (ordered, each with a `min_soak_seconds`) and regions are registered append-only. **Deploy** puts a RELEASED version into a ring and
region. Gates, all reported together:
1. the version re-verifies;
2. the **previous ring** has the version active in the same region, for at least that ring's soak time, with **no runtime verification
   failure** recorded since it was deployed there (the health gate);
3. every registered-pack dependency is active in the **same ring and region** (JUR-NEG-15);
4. for a hotfix: its rollback target is active there, and two independent approving reviewers exist (JUR-NEG-14).

A resolver instance serves one ring and region (`RESOLVER_RING`, `RESOLVER_REGION`) and uses **only** versions with an ACTIVE deployment
there, so rings actually control exposure rather than being labels. Each recorded decision names the ring and region that made it.
Deploying a version whose rule window starts in the future is allowed; the resolver still selects it only once effective (JUR-NEG-21).

**Rollback** takes a version out of one ring and region and restores a named older release there. The restore version must be the same
pack, strictly older, RELEASED, re-verify, and have been active in that scope before. Rollback changes **future** resolution only
(JUR-NEG-19): the response counts the decisions already made with the rolled-back version in that scope, says remediating them is a
separate workflow that **does not exist**, and states that **no reconciliation was performed**.

## Emergency hotfix (s24)
`hotfix` is declared while the version is DRAFT or REVIEW (never after certification), with severity P0 or P1, scope, incident,
**incident commander** and the **rollback target**, which must be an older RELEASED version named before promotion. It then:
* requires **two** independent approving reviewers to certify, regardless of `PACK_CERT_MIN_REVIEWS` (checked in the service and by the
  database certification guard);
* must still pass the same verification and the ring order; the **only** exception is skipping ring order with a documented
  `exception_reason`, accepted solely from the incident commander and recorded on the deployment;
* starts the **retrospective** clock (`HOTFIX_RETRO_SLA_HOURS`); an overdue retrospective shows in the metrics. The hotfix record is
  immutable except that the retrospective can be completed once.

## Source-change intake (s29, s31)
A notice records that an authority's material may have changed, with one of the s31 change classes. It opens a controlled review:
an independent reviewer (not the author) takes it, only that reviewer closes it with `NO_CHANGE`, `INTERPRETATION_RECORDED` (needs an APPROVED
interpretation), `RULE_CHANGE_PLANNED` or `CAPABILITY_BLOCKED`. **It never edits, activates or publishes a rule**; a test asserts a rule is
byte-identical after a notice is processed.

## Operations metrics (`GET /v1/ops/metrics`)
Resolutions by outcome and by pack version, runtime verification failures, upcoming effective pack windows, deployment skew between regions
per pack and ring, released-but-never-deployed versions, certification age with a stale flag, versions by status, open source-change
notices and the oldest age, overdue hotfix retrospectives. The payload also lists `not_measured`. It is a data feed; **no dashboard exists**.

## Configuration
`RESOLVER_ENABLED` (default **false**, so an existing production deployment keeps starting unchanged), `RESOLVER_RING` / `RESOLVER_REGION`
(both or neither; **required in production and staging when the resolver is enabled**, because without them it would use every released pack),
`HOTFIX_RETRO_SLA_HOURS` (120), `PACK_CERT_AGE_WARN_DAYS` (180). **Deploy order:** apply migrations 000005-000009 before the new binary; the
registry routes are always mounted and the resolver's evidence insert needs the 000009 columns.

## Decisions the document leaves open, and what this wave assumed
* **Ring names, count, soak times, regions.** Not specified; they are data you register, nothing is seeded.
* **Health gate definition (s23 "telemetry/reconciliation healthy").** Assumed: soak time elapsed plus no runtime verification failure in the previous
  ring and region. No business or reconciliation signal exists in this service, so none is checked.
* **Hotfix criteria and SLA (s24).** P0 and P1 are the only accepted severities; "criteria defined before use" is not modelled beyond that. The
  retrospective SLA is configuration.
* **Who is the incident commander.** A principal id named at declaration; the service does not check it against an on-call roster.
* **Region residency (s3).** Packs are not tied to regions by residency rules; a region is only a deployment scope.

## Not built
* Remediation workflow for outcomes made under a rolled-back or withdrawn version (named in every rollback response).
* A reconciliation or business-health signal for promotion; automatic promotion (every promotion is a human command).
* Supersede command; compatibility matrices between pack families (s2); tenant elections/overrides (s28).
* Reporting of the pack version each running service actually uses (metrics show skew between regions in the registry, not between processes).
* Push-based notification of resolvers; a block or withdrawal reaches a resolver within its cache TTL unless the cache is invalidated.
* Source-change **detection** (watching authorities): intake is manual.
* CI coverage of the integration suite (`ci.yml` was not changed).

## Verification
Integration suite `internal/registryit` (embedded Postgres 16): release/withdraw/block with DB guards; dependency gates for publish, deploy and
withdraw; ring order, soak and failure gates; resolver served by ring and region (including block and withdrawal taking effect); rollback with
decision counting and restore rules; hotfix enhanced review, DB-level two-reviewer guard, ring exception, retrospective and overdue metric;
source-change independence and the proof a notice changes no rule; metrics (skew, certification age, undeployed, limits); 000005-000009 down/up
round trip. Unit tests for configuration safety (opt-in resolver, production scope, policy bounds).
