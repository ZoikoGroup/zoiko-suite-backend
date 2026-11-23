# BIZ-02 Record Classification — Certification Status

**Service host:** document-vault-svc (BIZ-02 is implemented in the same service as BIZ-01; classification lives in its own table set, `record_classifications`, migration 000008)
**Assessed against:** `ZoikoSuite_Business_Operations_Content_Services_Detailed_Service_Specifications_v1_0` (ZS-SVC-P-001) section 4
**Status: NOT CERTIFIED.** Core contract implemented and now evidenced. One SoD defect found and fixed in this audit (restricted classes). The classification events are now on the shared eventing outbox. The `RESTRICTED` status, downstream policy decision references and live-stack checks remain open.

A certificate listing only successes certifies nothing, so the last two sections say what was **not** verified.

## Contract coverage (spec section 4)

| Spec row | State | Where |
|---|---|---|
| Commands: ClassifyRecord, ConfirmClassification, Reclassify, SupersedeClassification, BulkClassify | All implemented. BulkClassify is atomic: every proposal and its event commit together or none does; all documents are authorized before anything is written | `internal/handler/handler.go`, `internal/store/classification_store.go` |
| Queries: GetClassification, GetAsOfClassification, ExplainPolicyMapping, ListUnclassified, ListClassificationHistory | All implemented | same |
| Lifecycle Unclassified -> Candidate -> Confirmed -> Superseded/Restricted | Candidate, Confirmed, Superseded implemented, with database triggers limiting what may change on a row. Unclassified is the absence of a row. **`RESTRICTED` is defined as a status but no code ever sets it** (its own comment says "wired in a later wave"); restricted handling is by classification *value* only | `internal/domain/classification.go` |
| Authorization / SoD | Steward actions `CLASSIFY_RECORD`, `CONFIRM_CLASSIFICATION`, `RECLASSIFY_RECORD`, `SUPERSEDE_CLASSIFICATION`. Human proposals need a different confirming principal (maker-checker, in the store). **Security/privacy owner for restricted classes was missing: fixed in this audit** (below) | handler, `internal/authz/client.go` |
| Automated classification only within confidence policy | Every AI proposal lands CANDIDATE and needs a human confirm. The auto-confirm threshold is an explicit placeholder (`-1`, never met) because the policy owners (AI-02, DATA-GOV) do not exist. Conservative, but the confidence policy itself is not implemented | `AIConfidenceAutoConfirmThreshold` |
| Events (4) | `classification.proposed`, `confirmed`, `reclassified` and `superseded` are written to the shared eventing outbox in the same transaction as the change, as `com.zoikosuite.business.classification.proposed`, `confirmed`, `reclassified` and `superseded`, with payload schemas in `contracts/events` enforced by the store tests and catalogue entries (extension, producer Document Vault Service). The catalogue's section 19 entry `business.document.classification-changed` is still unowned and overlaps these | `internal/store/eventing_events.go`, `contracts/events` |
| Failure: low-confidence/ambiguous stays candidate | Enforced: nothing auto-confirms | store |
| Failure: policy engine failure blocks protected downstream actions rather than defaulting to public/internal | No policy engine is integrated, so this cannot occur. A document cannot be created without an explicit classification (invalid value 400, no silent default). `ExplainPolicyMapping` states honestly that no policy mapping is configured. Whether downstream protected actions (for example content download) consult the classification was not verified | handler, `ExplainPolicyMapping` |
| Evidence: classifier/reviewer, confidence, rule/model version, source evidence, effective dates, downstream policy decision IDs | Proposer, confirmer, confidence, `rule_model_version`, `source_evidence`, `effective_at` and correlation id are recorded and append-only in history. **Downstream policy decision IDs are not recorded** (no policy engine) | `RecordClassification` |

## Defect found and fixed in this audit

The spec requires a "security/privacy owner for restricted classes". Confirming a classification whose value is `RESTRICTED` needed only the ordinary `CONFIRM_CLASSIFICATION` action from any other steward, and superseding a classification into or out of `RESTRICTED` needed only `SUPERSEDE_CLASSIFICATION`.

Fix: a new action, `CONFIRM_RESTRICTED_CLASSIFICATION`, is now required **in addition to** the ordinary one when confirming a `RESTRICTED` classification, and when superseding a classification that is `RESTRICTED` or whose replacement is `RESTRICTED` (moving a document into or out of `RESTRICTED`). It fails closed (403 denied, 503 authz unavailable) and a refused confirm leaves the proposal a CANDIDATE. Proposing (ClassifyRecord, Reclassify, BulkClassify) is unchanged; the gate is on confirmation.

**The new action is registered in access-control-svc (migration 000058, HIGH) but no role template or bundle grants it yet.** Until a role is given it, confirming or superseding any RESTRICTED classification returns 403 in a real deployment; who holds it is a role-design decision.

## Implementation gate (spec: "must pass ... before production certification")

| Gate | Evidence | State |
|---|---|---|
| Authority boundary | Classification "does not grant access itself": the classification table is separate from the document row and nothing in the access path was found to read it as a grant, by inspection. No test asserts it | Partial |
| Tenant isolation | RLS forced on `record_classifications` (migration 000008). `TestPgStore_Classification_TenantIsolation` (added in this audit) passes on Postgres 16 as a `NOSUPERUSER NOBYPASSRLS` role: tenant B cannot read, explain, list history or as-of, classify, reclassify or confirm tenant A's classification; A's current classification and history are unchanged; a raw count under B's tenant context sees 0 rows. The test was confirmed to fail with RLS disabled on the table | Covered, including RLS |
| Classification / retention | Classification feeds DRC-03 retention evaluation, which has its own tests; retention itself is DATA-GOV's decision and is only resolved here | Covered (retention tests) |
| Negative paths | Self-confirmation refused (store and handler, confirm and supersede), AI proposal needs confidence and human proposal must not carry one, invalid value 400, reclassify requires an existing confirmed classification, already-superseded 409, unknown classification 404, bulk rolls back on a part-way failure, restricted confirm and supersede without the security-owner action (new) | Covered |
| Evidence lineage | History, as-of and policy-mapping explanation tests exist; policy decision IDs are not recorded | Partial |

## What was verified, and how

```
go build ./... && go vet ./...   clean
go test ./... -count=1           all packages pass with TEST_DATABASE_URL set to a throwaway
                                 postgres:16-alpine container (store suite 210 s)
new handler tests                restricted_classification_test.go: confirm RESTRICTED needs the
                                 security-owner action (403, stays CANDIDATE; granted -> 200),
                                 non-restricted does not consult it, authz outage fails closed
                                 (503), supersede into RESTRICTED needs it. The confirm and
                                 supersede tests fail against the original handler
new store test                   TestPgStore_Classification_TenantIsolation: passes; fails with RLS off
```

Date: 2026-11-16. The Postgres container was disposable and has been removed.

## NOT verified

- No Docker image build or container smoke run, and the docker-compose stack was not run.
- Real authorization-svc decisions for the BIZ-02 actions (including the new one) were not checked; tests use a stub.
- Whether downstream protected actions consult the classification was not verified.
- `Reclassify` and `BulkClassify` proposals of `RESTRICTED` are not gated (only confirmation is). That is deliberate here, but the spec text does not say whether proposing should also need the security owner.
- The isolation test is the only classification test run as a non-superuser; the rest of the store suite ran as a superuser.
- No event was delivered through Kafka.

## Open items to reach certification

1. ~~Register `CONFIRM_RESTRICTED_CLASSIFICATION`.~~ Registered (access-control-svc migration 000058). Still to do: grant it to the security/privacy owner role.
2. Decide and implement the `RESTRICTED` status transition (the constant exists; no code sets it) and whether it should replace `CONFIRMED` for restricted values.
3. ~~Move the four classification events to the shared eventing outbox.~~ Done (see `BIZ01_CERTIFICATION.md`, "Event migration"). Still to do: reconcile with the catalogue's unowned `business.document.classification-changed`.
4. Implement the confidence policy and policy-decision references once AI-02 and DATA-GOV exist, and record downstream policy decision IDs.
5. Verify, and add a test, that protected downstream actions use the confirmed classification and never fall back to a default.
6. Confirm with the spec owner whether proposing a `RESTRICTED` classification needs the security owner.
