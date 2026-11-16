# BIZ-01 Document Management — Certification Status

**Service host:** document-vault-svc (BIZ-01 and BIZ-02 are implemented in this one service; the DRC-02/03/04 records, retention and preservation work shares its tables)
**Assessed against:** `ZoikoSuite_Business_Operations_Content_Services_Detailed_Service_Specifications_v1_0` (ZS-SVC-P-001) section 3
**Status: NOT CERTIFIED.** The functional contract is the most complete of the BIZ services audited so far. Remaining gaps are the event contract (not on the shared eventing outbox, no catalogue entries or schemas), the spec's Draft and Disposed states, and live-stack checks.

A certificate listing only successes certifies nothing, so the last two sections say what was **not** verified.

## Contract coverage (spec section 3)

| Spec row | State | Where |
|---|---|---|
| Commands: UploadDocument, CreateVersion, DeclareRecord, SupersedeDocument, MoveToArchive, RequestDisposition | All implemented | `POST /v1/documents`, `/{id}/versions`, `/declare-record`, `/supersede`, `/archive`, `/request-disposition`; also `POST /v1/records:declare` and upload reservations |
| Queries: GetDocument, GetVersion, ListVersions, VerifyDigest, GetLinkedObjects, GetAsOfDocument | All implemented | `GET /v1/documents/{id}`, `/v1/document-versions/{id}`, `/versions`, `/verify-digest`, `/links`, `/as-of` |
| Lifecycle Draft/Active -> Superseded/Declared Record -> Archived/Disposed | Active, Superseded, Declared Record (tracked separately), Archived implemented. **No Draft** (create ingests the first version) and **no Disposed**: disposition stops at `APPROVED_FOR_DISPOSITION` and nothing is deleted (README open decisions OD-S03, OD-S05) | `internal/store/pg_store.go` |
| Authorization | Per-action, fail-closed: `DOCUMENT_CREATE`, `_READ`, `_DOWNLOAD`, `_VERSION_CREATE`, `_DECLARE_RECORD`, `_SUPERSEDE`, `_ARCHIVE`, `_REQUEST_DISPOSITION`, `_LINK`, `_ACCESS_LOG_READ`, plus `RECORD_*`, `RETENTION_*`, `LEGAL_HOLD_*` | handler |
| Events (5) | Emitted, but under the service's own names: `document_created`, `document_version_committed`, `document.record_declared` (and `record.declared`), `document.superseded`, `document.archived`; also `document.disposition_requested`, `document.version_upload_quarantined` and `classification.*`. Mixed underscore/dot naming. **Not on the eventing outbox, not canonical, no schemas** | `internal/store/pg_store.go`, `internal/outbox` |
| Failure: malware/type/hash quarantines the upload | Enforced before anything is persisted; a quarantined new version records `document.version_upload_quarantined` and creates no version; content type is detected, not trusted from the caller | handler, `internal/scan`, `internal/contenttype` |
| Failure: no phantom successful version | Reservation state machine (RESERVED/COMMITTED/FAILED/QUARANTINED) with a single transactional commit; a blob whose metadata row did not commit is removed. Versions and the access log are append-only by trigger (migration 000013) | `upload_reservations.go`, migration 000013 |
| No destructive delete under hold/retention | No route deletes a document or version. The only `storage.Delete` removes an orphaned blob whose row never committed. `RequestDisposition` is refused while any record of the document is under an unreleased legal hold | `cleanupBlob`, `RequestDisposition` |
| Evidence: hash, uploader/source, metadata, links, classification, version chain, access history, retention/hold | Version rows carry checksum and creator, access log append-only, links, classification (BIZ-02) and retention/hold state (DRC-03) all present | migrations 000001-000016 |

## Implementation gate (spec: "must pass ... before production certification")

| Gate | Evidence | State |
|---|---|---|
| Authority boundary | The service stores documents and metadata only, with no business-fact fields, by inspection. No test asserts it | Partial |
| Tenant isolation | RLS enabled and forced on the document tables (migration 000002). `TestPgStore_Documents_TenantIsolation` (added in this audit) passes on Postgres 16 as a `NOSUPERUSER NOBYPASSRLS` role: tenant B cannot read, list, list versions/links, read as-of, add a version, link, declare, supersede, archive or request disposition of tenant A's document; A's data is unchanged; a raw count under B's tenant context with no `tenant_id` predicate sees 0 rows in `documents`, `document_versions`, `document_links` and `document_access_log`. The test was confirmed to fail with RLS disabled on `documents`. Retention, records, preservation, reservations and version lookup already had their own tenant tests | Covered, including RLS |
| Classification / retention | BIZ-02 classification and DRC-03 retention rules, legal holds and disposition gating are implemented and tested (retention, hold-blocks-disposition, tenant-scoped evaluation) | Covered |
| Negative paths | Quarantine on create and on new version (handler tests, 422, nothing persisted), idempotency replay/mismatch/concurrency, append-only triggers, hold blocks disposition, cross-tenant declare refused, unknown document 404, invalid as-of | Covered |
| Evidence lineage | Append-only versions and access log, version-chain as-of reconstruction, link and classification history all have store tests | Mostly covered |

## What was verified, and how

```
go build ./... && go vet ./...   clean
go test ./... -count=1           all packages pass with TEST_DATABASE_URL set to a throwaway
                                 postgres:16-alpine container; store suite 262 s. The suite runs as
                                 the postgres superuser except the integrity tests and the new
                                 isolation test, which use a NOSUPERUSER NOBYPASSRLS role
new test                         TestPgStore_Documents_TenantIsolation: passes; fails with RLS off
```

Date: 2026-11-16. The Postgres container was disposable and has been removed. The only code change in this audit is the new test file.

## NOT verified

- No Docker image build or container smoke run of this service, and the docker-compose stack was not run.
- Real authorization-svc decisions for the `DOCUMENT_*` actions were not checked, nor that those actions are registered there (tests use a stub).
- A real ClamAV scanner and a real object store were not used. The storage backend is a local AES-GCM file backend.
- Whether a download of a sensitive (classified) document requires a purpose or classification check (spec: "sensitive records require purpose/classification checks") was not verified, and no test for it was found.
- No event was delivered through Kafka to a consumer.
- The rest of the store suite ran as a superuser, so its RLS behaviour under an application role is only covered by the tests named above.

## Open items to reach certification

1. Move the document and classification events to the shared eventing outbox with canonical names (`com.zoikosuite.business.document.*`), payload schemas in `contracts/events` enforced by the store tests, and catalogue entries. The catalogue already lists `business.document.stored`, `versioned` and `classification-changed`, but its producer is the ambiguous "Document Service", so no deployed service owns them. The spec's names (DocumentUploaded, DocumentVersionCreated, RecordDeclared, DocumentSuperseded, DocumentArchived) need mapping to these.
2. Decide the Draft state and the Disposed state (needs the clarifications OD-S03, OD-S05 and a `DOCUMENT_UPDATE`/lock permission in the IAM taxonomy).
3. Verify or add the purpose/classification check on retrieval of sensitive documents.
4. Add an OpenAPI contract for the BIZ-01 routes (none exists in the service directory) and a route-drift test, as BIZ-04 and BIZ-07 have.
5. Check the `DOCUMENT_*` actions against authorization-svc, and run the service in the docker-compose stack.
6. Add a test that asserts no business-fact fields exist in the document model (authority boundary).
