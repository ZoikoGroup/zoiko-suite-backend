# Release Certificate — jurisdiction-rules-svc (D2-1)

**Date:** 2026-11-13
**Scope:** the backend of Jurisdictions & Rules. The console page (`/admin/jurisdictions`, JR-FE-1…3)
is not built yet; its summary route is.
**Result:** every suite green, with 0 skips. The live dependency e2e passed 82 of 82 checks, with
workflow-svc and the regulatory feed consumer in the stack, and the live audit passed 24 of 24, both on
the local compose stack.

This certificate records what was verified, against what, and what was not. Tracker status for every
row stays 🔵 (built, verified by its implementer) until an independent audit sets ✅.

---

## What this service is

Doc 03 §8.2 gives this service ownership of:

- jurisdiction profiles;
- effective-dated rule sets;
- entity-jurisdiction applicability;
- compliance calendars;
- external rule references and legal drift.

ZS-JUR-001 adds the pack architecture:

- regimes, sources and interpretations;
- signed and certified packs;
- release rings and hotfixes;
- the runtime resolver with decision evidence;
- remediation.

PDC-02, REF-01 and REF-10 add the following:

- PDC-02: the resolution outcomes and their negative paths;
- REF-01: the jurisdiction registry commands;
- REF-10: business calendars and payment terms.

`progress.md` §2 lists every operation with its route and status.

## Defects the live run found

Every unit and integration suite was green before the live run. The run still found four defects. Each
one now has a test that fails on the old code.

### 1. The outbox relay never published an event (pre-existing)

The relay set `Topic` on every `kafka.Message`, and the writer also names the topic. kafka-go refuses
that combination: `Topic must not be specified for both Writer and Message`. Every event therefore
failed and was retried. After 10 attempts each event was parked, and the relay moved on to the next.

Writes still committed, because the outbox did its job. But no consumer ever received a jurisdiction
event. The test writer now refuses a message that names a topic, the same way kafka-go does.

On the live stack, 4 events had been parked by this bug. They were requeued through the J-16 route,
which recorded a reason and evidence for each. The outbox now holds 0 pending and 0 parked events.

### 2. Asynchronous compile answered 503

An API-5 operation is recorded outside the request's transaction. That is correct, because the
background run must be able to find it. But the operation was written without its tenant, so the
`async_operations` RLS policy refused the insert.

The integration suite had missed this because it connected as a superuser, which bypasses RLS. The
operation store in that suite now runs as `jur_rls_probe`, which cannot bypass RLS.

### 3. Rule provenance and pack compile answered 503 on older databases

Some databases were migrated before 2 Nov 2026, on the earlier migration lineage:

- On those databases, `000008_add_precedence_metadata` had already added `supersedes_rule_id`.
- `000005_jurisdiction_pack_registries` then failed on its plain `ADD COLUMN`.
- It was recorded as applied by hand, but its `ALTER` of `jurisdiction_rules` never ran.

As a result, those databases lack:

- five rule columns;
- ten foreign keys;
- the provenance guard trigger.

Migration 000037 adds each missing object only when it is absent. On a database built from the
migrations directory it changes nothing. Its integration test does three things:

1. checks that it changes nothing on a fresh database;
2. converges a simulated old-lineage database;
3. reruns it to show it is idempotent.

### 4. A rule's author was told to retry their own activation

With workflow-svc down, the author's own activation answered 503 `approval_service_unavailable`. That
request can never succeed. The handler now refuses it with 403 `self_approval_forbidden` before it asks
workflow-svc. The store still refuses it as well.

## Verification performed

### Suites

The suites ran on 13 Nov 2026, against fresh scratch databases. Windows Application Control blocks
test binaries on the host, so they ran in a `golang:1.26-alpine` container.

| Suite | Pass | Fail | Skip |
|---|---|---|---|
| unit, all packages | 569 | 0 | 85 (the database tests, run as the store suite) |
| store | 92 | 0 | 0 |
| registryit | 143 | 0 | 0 |
| openapi ↔ router parity, kin-openapi validity, asyncapi | ✔ | | |

Migrations 000032 to 000036 each pass an up, down, up round trip on a scratch database. Migration
000037 has a no-op down; its test checks the fresh no-op, the convergence and the rerun. Every spec
negative path has a
test named after its id: JUR-NEG-01…30, REF-01 #22/#23, REF-10 #40/#41 and PDC-02 NP-11…20. The full
list is in `progress.md` §4.

### Live, against the running stack

`deployments/scripts/e2e-jurisdiction-rules-deps.py` passed 82 of 82 checks. The stack was the
smallest one that covers every dependency edge:

- postgres, kafka, redis;
- identity-context;
- mtls-management;
- access-control;
- authorization;
- tenant-entity-registry;
- workflow-svc, started with `--no-deps`: document-vault-svc, a teammate's service, is a compose
  dependency only and is not needed for approvals;
- this service, with the dev signing key and the feed consumer on
  (`JURISDICTION_REGULATORY_FEED_TOPIC=zoiko.regulatory.feed.e2e`).

governance-decision-log-svc was started after the run, so workflow-svc's ledger relay delivered the 15
approval decisions the run made. Its outbox ended at 0.

With workflow-svc stopped, `--no-workflow` passed 59 checks with 2 skipped (the workflow and feed
sections), and activation failed closed with 503.

| Edge | What was proven |
|---|---|
| postgres | All 42 migrations are applied. RLS is enabled and forced on the five tenant tables. The runtime role is not a superuser and cannot bypass RLS. With no tenant set, that role reads 0 rows. |
| identity-context | The admin and the approver each receive a signed envelope. J-01: an admin write with headers only answers 401, and so does one with a forged `X-Identity-Basis`. Reads without an envelope are admitted and marked `missing`. |
| authorization / access-control | A principal without grants is refused with 403. The granted admin is served. The nine 000052 templates are published. |
| kafka | `jurisdiction.created` is committed with its write. The relay publishes it to `zoiko.jurisdiction.events`, and the envelope carries `payload_hash` and `aggregate_version` (J-11, J-12, J-15). |
| workflow-svc (J-02) | An activation is refused with 403 `approval_not_valid`, giving the reason, when it cites any of these: <ul><li>an unknown id;</li><li>a PENDING workflow of the rule;</li><li>a REJECTED workflow of the rule;</li><li>an APPROVED workflow of another rule.</li></ul>After each refusal the rule stays DRAFT. An activation citing an APPROVED workflow of the rule, decided by the approver, answers 200 ACTIVE. The open status-history row holds the workflow id and the activator, and `jurisdiction.rule.activated` is recorded. With workflow-svc down, activation fails closed with 503. |
| regulatory feed (JR-6) | A `regulatory.source.changed` event published to the feed topic moves the ACTIVE rule that cites the reference to `UNDER_REVIEW`. The rule's status and payload do not change. A drift event recorded by `jurisdiction-rules-svc:regulatory-feed` quotes the authority and the event id, and `legal.drift.detected` is recorded. A person then sets the drift back to CURRENT; the same event redelivered changes nothing. The e2e publishes the event itself, because its producer does not exist yet. |
| tenant-entity-registry | An entity is created whose primary jurisdiction is checked live against this service. The consumer projects it, and another tenant cannot see it. After its mapping is deleted, the J-10 backfill restores it, and a rerun changes nothing. |
| this service | The following were exercised: <ul><li>source review by a second principal;</li><li>a stale If-Match answering 412, then the ETag moving to 2;</li><li>an interpretation approved by its approver;</li><li>a rule with provenance;</li><li>a pack version, submitted for review;</li><li>an async compile that succeeded, which another tenant reads as 404;</li><li>signing and verifying the pack;</li><li>cursor paging, where another tenant's cursor answers 400;</li><li>a parked event requeued and then published, with its requeue evidence append-only;</li><li>`regulatory-rule.effective` announced when the start date passed, for a rule activated through workflow-svc.</li></ul> |

`scripts/audit.sh` was run live with `SKIP_SUITES=1`, because the suites had already run above. It
passed 24 of 24 checks: database, live routes, negative paths, telemetry, and an outbox that drains.

### Mutation check

Two guards were broken on purpose. A test failed for each, and both guards were then restored.

| Guard broken | Test that failed |
|---|---|
| The J-01 gate accepts any identity basis | `TestJ01_AdminWritesNeedAVerifiedIdentity`: `204, want 401` |
| The If-Match comparison is disabled | `TestAPI3_SourceCommandsCheckIfMatch`: `expected 412, actual 200` |

## Not verified live (read this)

- **The real producer of the regulatory feed.** The consumer is proven live against Kafka with an
  event the e2e publishes. external-data-feed-svc does not publish `regulatory.source.changed`, so
  compose leaves the consumer off by default (`JURISDICTION_REGULATORY_FEED_TOPIC` empty).
- **Entity creation for the e2e.** The demo admin needs `ENTITY_CREATE` in tenant-entity-registry. The
  e2e adds that as a development-only grant, the same way the other e2e scripts do.

## Spec requirements that are not met

| Item | Why | Owner |
|---|---|---|
| Producer of `regulatory.source.changed` (F05) | external-data-feed-svc does not publish it | teammate ⛔ |
| Production pack-signing key; `RESOLVER_RING` / `RESOLVER_REGION` | SEC-1 key custody | platform ops ⛔ |
| JUR-NEG-17: exemption without registration evidence | There is no exemption rule family. The wave docs leave exemptions to Tax Engineering. | 🧭 |
| JUR-NEG-29: block a regulated action on pack skew | Skew is reported and each resolver serves only its ring and region. Blocking needs a consistency policy. | 🧭 |
| J-05, J-08, J-09 | An ownership decision, a single precedence model, and a public read tier | architecture 🧭 |

## Contract surface

- `openapi.yaml` 1.2.0 covers every route, and it matches the router (a test checks this).
- `asyncapi.yaml` covers every event this service emits and consumes.
- RFC 9457 problems on every error path.
- API-3: an ETag on every aggregate read, and If-Match checked under a row lock on its commands.
- API-4: signed cursors, bound to the tenant, the route and the filters.
- API-5: operations for compile and test runs.
- J-11: one transaction per request. A write whose event cannot be recorded answers 503
  `event_not_recorded`, and nothing is written.

## Artifacts

- Migrations 000032 to 000037.
- access-control 000052, which publishes the role templates.
- `RUNBOOK.md`, `SPEC_DEVIATIONS.md`, `scripts/audit.sh`.
- `progress.md`, which holds the inventory, the ordered steps, the negative-path matrix and the
  evidence log.
- The Postman collection `postman/postman/ZoikoSuite_JurisdictionRules.postman_collection.json`
  (folders 04–07).
- The e2e script `deployments/scripts/e2e-jurisdiction-rules-deps.py`.

## Re-proving

```
python deployments/migrate.py up --db jurisdiction_rules
go run ./cmd/seed-demo-principals          # in services/identity-context-svc
pwsh deployments/scripts/seed-demo-rbac.ps1
python deployments/scripts/e2e-jurisdiction-rules-deps.py --prepare-signing-key   # once; prints the restart command
python deployments/scripts/e2e-jurisdiction-rules-deps.py
B=http://127.0.0.1:8082 bash services/jurisdiction-rules-svc/scripts/audit.sh
```
