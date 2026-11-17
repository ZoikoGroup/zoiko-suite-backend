# BIZ-03 Template — Certification Status

**Service host:** notification-svc (`/v1/document-templates`; tables `template_definitions`, `template_versions`)
**Assessed against:** `ZoikoSuite_Business_Operations_Content_Services_Detailed_Service_Specifications_v1_0` (ZS-SVC-P-001) section 5
**Status: NOT CERTIFIED.** The command and query contract is implemented and tested. One gap found and fixed in this audit (undeclared variables could be published). Accessibility checks, canonical events and live-stack checks remain open.

A certificate listing only successes certifies nothing, so the last two sections say what was **not** verified.

## Contract coverage (spec section 5)

| Spec row | State | Where |
|---|---|---|
| Commands: CreateTemplate, CreateVersion, ValidateTemplate, ApproveTemplate, PublishTemplate, RetireTemplate | All implemented | `POST /v1/document-templates/`, `/{id}/versions`, `/versions/{id}/validate`, `/approve`, `/publish`, `/{id}/retire` |
| Queries: GetTemplate, GetPublishedVersion, RenderPreview, CompareVersions, ListLocales | All implemented | `GET /{id}`, `/{id}/published?locale=`, `POST /versions/{id}/preview`, `GET /{id}/compare`, `/{id}/locales` |
| Lifecycle Draft -> Review -> Approved -> Published -> Retired/Superseded | Implemented on the version, per locale. Illegal edges are refused by a database trigger (migration 000017), not just code. Publishing supersedes the prior published version of the same locale in the same transaction | `internal/store/template_store.go`, migrations 000005/000017 |
| Authorization / SoD | `TEMPLATE_MANAGE` for author actions and reads, `TEMPLATE_APPROVE` for approve, publish and retire. Maker-checker is enforced for **every** version (approver may not be the creator), in code and by a DB CHECK. Fetch, then authorize, then mutate; fail-closed | handler |
| Events (4) | `template.created`, `template.version_approved`, `template.published`, `template.retired` are written to the service's own `event_outbox` in the same transaction as the change. **Not on the shared eventing outbox**, and only `business.template.published` has a catalogue entry (producer "Template Service", no owner) | `internal/events`, `internal/store/outbox.go` |
| Failure: missing variable schema blocks publication | **Gap, fixed in this audit** (below) | `ValidateTemplate` |
| Failure: protected text change after approval blocks publication | Content and terminal states are frozen by trigger; content hash is computed at creation | migrations 000005/000017, NP-05 tests |
| Failure: inaccessible critical template blocks publication | **Not implemented.** `accessibility_metadata` is stored as free text and never evaluated | open item 2 |
| Evidence: content hash, variables/schema, locale, approval, publication dates | All present on the version row. **Accessibility result and generated-artifact references are not recorded** | `template_versions` |

## Defect found and fixed in this audit

The spec says a missing variable schema blocks publication. `ValidateTemplate` only checked that the content parses; its own comment claimed a schema was checked, but nothing compared the variables the wording uses with the declared `variable_schema`. A version using `{{.amount}}` with an empty schema validated, was approved and published, and at send time the variable rendered as blank text (the render path refuses undeclared variables passed in, but not undeclared ones used).

Fix: `ValidateTemplate` now parses the content, collects every `{{.name}}` reference (including inside conditions, ranges and pipelines) and refuses the version when one is not declared, returning 400 `template_variable_invalid` (already mapped to stable code NCD-004). The version stays DRAFT. Declared but unused variables are still allowed.

## Implementation gate (spec: "must pass ... before production certification")

| Gate | Evidence | State |
|---|---|---|
| Authority boundary | The service stores wording and metadata only; no business-fact fields, by inspection. No test asserts it | Partial |
| Tenant isolation | RLS on both template tables. `TestPgStore_Templates_TenantIsolation` (added) runs as the suite's `NOSUPERUSER NOBYPASSRLS` role: tenant B cannot read, list locales, compare, preview, get the published version of, create a version for, validate, approve, publish or retire tenant A's template; A's data is unchanged; a raw count under B's context with no tenant predicate sees 0 rows in both tables. **Not mutation-checked with RLS disabled** (the application queries also carry a tenant predicate) | Covered |
| Classification / retention | Template content is not classified or retention-managed in this service | **Not covered** |
| Negative paths | Self-approval, retired template, illegal status edges (DB), malformed content, partial and undeclared-variable render, cross-template compare, stale `If-Match` (412), authz denial, undeclared variables at validation (new) | Covered |
| Evidence lineage | Hash, approver, dates and supersession links stored; accessibility result and generated-artifact references are not | Partial |

## What was verified, and how

```
go build ./... && go vet ./...   clean
go test ./... -count=1           all packages pass with TEST_DATABASE_URL set to a throwaway
                                 postgres:16-alpine container (store suite 382 s)
new tests (template_biz03_test.go)
  TestPgStore_ValidateTemplate_RefusesUndeclaredVariables  passes; fails with the check disabled
  TestPgStore_Templates_TenantIsolation                    passes
```

Date: 2026-11-17. The Postgres container was disposable and has been removed.

## NOT verified

- No Docker image build or docker-compose run of notification-svc.
- Real authorization-svc decisions for `TEMPLATE_MANAGE` and `TEMPLATE_APPROVE` (tests use a stub).
- The undeclared-variable check treats every field reference as top-level (`{{range}}` or `{{with}}` change the dot in Go templates); templates that rely on that would be refused. None exist in the test suite and variables are a flat string map.
- Whether documents and forms (BIZ-04) actually consume published templates: only the notification send path was seen.
- The isolation test was not run with RLS disabled.
- No event was delivered through Kafka.

## Open items to reach certification

1. Move the four template events to the shared eventing outbox with canonical names and schemas, catalogue all four (only `business.template.published` exists, with an unowned producer), and decide how this relates to the service's own `event_outbox` CHECK on known event types.
2. Define accessibility rules and which templates are critical; evaluate them at validation and record the result as evidence (spec: inaccessible critical template blocks publication).
3. Decide "externally binding or regulatory" templates, if they need a stricter approval policy than the maker-checker applied to all versions.
4. Record generated-artifact references (BIZ-04/BIZ-10 renders) against the published version.
5. Add an authority-boundary test, and a mutation check of the isolation test with RLS off.
6. Register and verify the `TEMPLATE_*` actions in authorization-svc; run the service in the compose stack.
