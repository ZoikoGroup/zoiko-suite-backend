# BIZ-06 CRM / Relationship — Certification Status

**Service host:** counterparty-management-svc (BIZ-06 is bolted onto this service; no separate service)
**Assessed against:** `ZoikoSuite_Business_Operations_Content_Services_Detailed_Service_Specifications_v1_0` (ZS-SVC-P-001) section 8
**Status: NOT CERTIFIED.** Core contract implemented. The read-authorization defect found in the first audit is fixed and tested; event/API contracts, classification/retention and live-stack checks remain open.

A certificate listing only successes certifies nothing, so the last two sections say what was **not** verified.

## Contract coverage (spec section 8)

| Spec row | State | Where |
|---|---|---|
| Commands: CreateRelationship, LogInteraction, CreateOpportunity, UpdateStage, MergeCRMProfile, CloseOpportunity, LinkCommercialObject | Implemented (plus ConfirmPartyLink, Activate, MarkDormant, Close, RecordConsent as gap-fills) | `internal/handler/crm_handler.go` |
| Queries: GetRelationship, GetTimeline, GetOpportunity, ListPipeline, GetConsentContext, GetLinkedFinancialObjects | Implemented, authorization added to the 5 that lacked it (see below) | same |
| Lifecycle Prospect -> Active -> Dormant/Closed (+ MERGED) | Implemented, CHECK-constrained | migration 000002 |
| Opportunity stages "versioned per approved pipeline" | Fixed stage set (LEAD..CLOSED_WON/LOST) with append-only `opportunity_stage_transitions`. No governed pipeline-definition entity; the spec lists no command for one | migration 000002 |
| Authorization / SoD | `CRM_CREATE`, `CRM_MANAGE`, `CRM_CLOSE` on writes; `CRM_READ` on every read; separate `CRM_CONSENT_READ` for consent context (purpose-controlled data) | handler |
| Events (5) | Written to the eventing outbox in the same transaction as the change, as `com.zoikosuite.business.relationship.created`, `relationship.closed`, `interaction.logged`, `opportunity.created`, `opportunity.stage-changed`. Catalogue entries (extension), generated AsyncAPI (`eventing/asyncapi/business.asyncapi.json`) and payload schemas in `contracts/events`, enforced on every outbox row by the store tests | `internal/store/crm_store.go`, `contracts/events` |
| Failure: duplicate party match stays a candidate until ORG linkage confirmed | Enforced: separate `candidate_counterparty_id`, `party_link_status`, only ConfirmPartyLink promotes | migration 000002, store |
| Failure: opportunity value never feeds certified revenue | By inspection `value` is stored on the opportunity only. No test asserts it | — |
| Evidence: party link, source, owner, interactions, stage history, consent basis, linked objects | Columns and append-only tables exist for all | migration 000002 |
| Tenant isolation | RLS enabled and forced on the CRM tables; store sets tenant via bound `set_config` | migrations 000002, force_rls; `crmSetRLS` |

## Defect found in the first audit (FIXED)

**Five read handlers perform no authorization check:** `GetRelationship`, `GetTimeline`, `GetOpportunity`, `GetLinkedFinancialObjects`, `GetConsentContext`. Any caller with a tenant context can read relationship, interaction notes and consent data for any legal entity in the tenant. The spec requires "Sales/service teams within tenant/entity scope; restricted contact/consent data purpose controlled". Only tenant RLS protects these today. `ListPipeline` does check. Fix: each of the five now requires a principal (401), loads the resource (404 if unknown), then checks `CRM_READ` (`CRM_CONSENT_READ` for consent) on the resource's legal entity (403 denied, 503 authz unavailable, fail closed). `ListPipeline` already checked. The action names `CRM_READ` and `CRM_CONSENT_READ` were chosen in this change and are **not registered or granted in authorization-svc** — until they are, every read returns 403/503 in a real deployment.

## Implementation gate (spec: "must pass ... before production certification")

| Gate | Evidence | State |
|---|---|---|
| Authority boundary | Opportunity value not wired to any revenue path by inspection; no test | Partial |
| Tenant isolation | RLS forced on tables. `TestPgStore_CRM_TenantIsolation` passes on Postgres 16 as a `NOSUPERUSER NOBYPASSRLS` role: tenant B cannot read, or transition/close/consent/link/merge-into, tenant A's data; A's data is unchanged; a raw count under B's tenant context with no `tenant_id` predicate sees 0 rows in all six CRM tables | Covered, including RLS |
| Classification / retention | Nothing CRM-specific (contact notes, consent records) | **Missing** |
| Negative paths | Store tests: unknown relationship, confirm-without-candidate, closed-opportunity stage change, double merge, self-merge, invalid consent status, append-only on interactions/transitions/links/consent. `crm_handler_test.go` covers the five reads (401, 403 with no data returned or leaked, 503 fail-closed, 404 before authz, correct action and legal entity, consent needs its own action) and CreateRelationship (validation, denied creates/publishes nothing, publishes `relationship.created`). Other write handlers still have no handler tests | Partial |
| Evidence lineage | Columns and append-only history exist; no test asserts the lineage fields end to end | Partial |

## What was verified, and how

```
go build ./... && go vet ./...   clean
go test ./... -count=1           all packages pass with TEST_DATABASE_URL set to a throwaway
                                 postgres:16-alpine container; the 22 CRM store tests all PASS
                                 (0 skipped). Run as the postgres superuser only
docker build                     image builds
container smoke run              starts, connects to Postgres, /healthz and /readyz 200;
                                 /v1/relationships/{id} and /v1/pipeline without identity -> 401
```

Re-run after the fix, same day: handler tests pass, and fail on the pre-fix handler (6 test groups red against the original `crm_handler.go`, green against the fix). Full suite passes with `TEST_DATABASE_URL` (superuser, for migrations) and `TEST_DATABASE_APP_URL` (`crm_app`, `NOSUPERUSER NOBYPASSRLS`, for the isolation test): store 23/23. The Postgres containers were disposable and have been removed.

After the outbox adoption: full suite passes against Postgres 16 (store now 26 tests: 22 original, isolation, events-written-to-outbox, no-region-rolls-back, outbox migration matches library; each test run also validates every outbox row against `contracts/events`, and a deliberately broken schema was confirmed to fail it). `eventing` catalogue, policy and contracttest suites pass with the five new entries. `docker build -f counterparty-management-svc/Dockerfile services/` builds; the container refuses to start without `EVENT_RESIDENCY_REGION` (exit 1) and with it connects, verifies the outbox schema, starts the relay and serves `/healthz` and `/readyz` 200.

## NOT verified

- Only the isolation test uses the non-superuser role; the other 22 store tests ran as the superuser. The isolation test was not mutation-checked (RLS disabled to confirm the probe fails).
- The docker image was built and smoke-run before the handler change, not rebuilt after it.
- The smoke run proves unauthenticated requests are rejected; it does not exercise the authenticated read-authorization defect above.
- Write handlers other than CreateRelationship have no handler tests. `CRM_READ` and `CRM_CONSENT_READ` are registered in access-control-svc (migration 000057); no role template or bundle grants them yet (a role-design decision), and real authorization-svc decisions were not checked.
- There is no `openapi.biz06.yaml` (HTTP API contract). `CloseOpportunity` still emits no event although it changes the stage. The new outbox path was exercised against Postgres and a container, but not end to end through Kafka to a real consumer; the relay started in the smoke run with no reachable broker, so delivery was not observed.
- Dependencies ORG-07, BIZ-09/10 and AR/LEG/COM links were not checked end to end. `ConfirmPartyLink` does not verify the counterparty exists in this audit.
- The docker-compose stack was not run.

## Open items to reach certification

1. ~~Add authorization to the five read handlers.~~ Done. ~~Register `CRM_READ` / `CRM_CONSENT_READ`.~~ Registered (access-control-svc migration 000057). Still to do: grant them to the roles that need them.
2. ~~Add CRM handler tests.~~ Done for reads and CreateRelationship; extend to the other writes.
3. ~~Add a cross-tenant negative test including a non-superuser RLS probe.~~ Done.
4. ~~Run the CRM store suite on Postgres and record the result.~~ Done, 23/23 (isolation test as non-superuser).
5. ~~Event contracts: catalogue entries, schemas, contract test.~~ Done (outbox adoption, 5 schemas, catalogue, AsyncAPI regenerated). Still to do: `openapi.biz06.yaml`, and a Kafka-delivery check through a real broker.
6. Decide classification/retention for interaction notes and consent records.
7. Add a test or guard asserting opportunity value never reaches a revenue path.
