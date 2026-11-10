# BIZ-04 Form Definition & Submission — Certification Status

**Service host:** workflow-svc (BIZ-04 is bolted onto this service; no separate service)
**Assessed against:** `ZoikoSuite_Business_Operations_Content_Services_Detailed_Service_Specifications_v1_0` (ZS-SVC-P-001) section 6
**Status: NOT CERTIFIED.** Core contract implemented and now evidenced; three spec items remain open.

A certificate listing only successes certifies nothing, so the last sections say what was **not** verified.

## Contract coverage (spec section 6)

| Spec row | State | Where |
|---|---|---|
| Commands: CreateForm, PublishForm, SaveDraft, SubmitForm, ValidateSubmission, SupersedeSubmission, RouteToDomain | Implemented (plus RetireForm, which the spec's lifecycle implies) | `internal/handler/form.go` |
| Queries: GetForm, GetSubmission, GetSubmissionVersion, GetValidationResult, ListPendingSubmissions | Implemented | same |
| Lifecycles (form and submission) | Enforced by database CHECK constraints; submitted values immutable by trigger | migration 000018 |
| Authorization / SoD | Publish is maker-checker (owner cannot publish; DB CHECK + store + 403). Separate actions `FORM_MANAGE`, `FORM_APPROVE`, `FORM_SUBMIT`, `FORM_READ`. | handler, migration 000018 |
| Events (5) | Published as `form.published` and `form.submission.{received,validated,rejected,routed}` | `internal/events/publisher.go` |
| Failure: target-domain rejection leaves submission accepted as evidence | Routes recorded separately (`form_submission_routes`); submission stays ACCEPTED | store |
| API contract | `openapi.biz04-forms.yaml`, tied to the router by a test | this directory |
| Dependency: BIZ-01 attachments | **Not implemented** — no attachment or digest fields on submissions | — |
| SoD: target-domain mapping approver / submitter cannot self-approve downstream | **Not implemented** — `RouteToDomain` checks only `FORM_SUBMIT` | `handler/form.go` |

## Changes made in this audit

- **Tenant filter added to every form store query.** Previously ~20 queries filtered only by `form_id` / `submission_id` and relied entirely on row-level security. With a superuser or `BYPASSRLS` database role, tenant B could read tenant A's form (found by the new test). Every query now also filters on `tenant_id`. Defense in depth: behavior is unchanged for correct callers.
- `openapi.biz04-forms.yaml` (new) and `TestFormRoutes_MatchOpenAPIContract` (new, fails if a form route and the contract disagree; verified by renaming a path).
- `TestPgStore_Form_TenantIsolation` (new): tenant B cannot read, list, draft against, publish, retire, submit, validate, supersede or route tenant A's records, and A's data is unchanged; plus a raw RLS probe that runs only for a non-superuser role.
- Three older tests that ran raw SQL with no tenant context only passed because the test role bypassed RLS; they now run through `execAsTenant`, so they test the constraint or trigger under any role.

## Implementation gate

| Gate | Evidence | State |
|---|---|---|
| Authority boundary | Routing records an attempt; the owning domain commits. Nothing in BIZ-04 posts a business change. By inspection; no test asserts it. | Partial |
| Tenant isolation | `TestPgStore_Form_TenantIsolation` passes as superuser and as a `NOSUPERUSER NOBYPASSRLS` role; RLS probe passes under the non-superuser role | Covered, including RLS |
| Classification / retention | Nothing form-specific found for submitted values | **Missing** |
| Negative paths | Self-publish, republish, double retire, validate non-submitted, double supersede, route non-accepted, immutability after submit (store tests) | Mostly covered |
| Evidence lineage | Form version snapshot, submitter, timestamps, validation result and route references recorded; attachments/digests absent | Partial |

## What was verified, and how

```
go build ./... && go vet ./...    clean
go test ./...                     all packages pass against a throwaway postgres:16-alpine
                                  (store suite 135 s) as the postgres superuser
form store tests                  15/15 pass as a NOSUPERUSER NOBYPASSRLS role
```

## NOT verified

- Handler behavior was not re-tested beyond the existing handler tests and the route/contract test; the HTTP status codes in `openapi.biz04-forms.yaml` were written from reading the handlers, not from calls against a running service.
- The OpenAPI file parses by regex in the drift test only for paths and methods; it was not run through an OpenAPI linter, and schemas are not checked against the Go types.
- Only the form tests were run as the non-superuser role; the rest of the store suite was not.
- No real authorization-svc, Kafka or full docker-compose stack.

## Open items

1. **Attachments and digests** (BIZ-01): needs a decision on how a submission references vault documents.
2. **Routing separation of duties:** who counts as the "target-domain mapping approver", and whether the submitter may route their own accepted submission, is a policy decision.
3. **Classification / retention** of submitted values: policy decision.
4. Event-name contract test and AsyncAPI file for the form events (not written).
5. Run the rest of the store suite under a non-superuser role.
