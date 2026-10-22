# document-vault-svc (:8094)

Governed document repository and records service. It implements BIZ-01/BIZ-02
(documents, versions, classification), DRC-02 records, DRC-03 retention and legal
holds, DRC-04 preservation (renditions, fixity, redaction, export packages) and
audit evidence, on PostgreSQL with forced row-level security, per-action
authorization (fail-closed) and a transactional outbox.

Authoritative contract: `ZS-SVC-S-001` (DRC-01 to DRC-05), §3.6 and §9–§11;
`Microservices Specification` §14.2; Business Operations spec BIZ-01/BIZ-02.

## Routes that follow `ZS-SVC-S-001`

| Route | Spec | Implementation |
|---|---|---|
| `POST /v1/legal-holds`, `POST /v1/renditions` | §9.2 | existing |
| `GET /v1/document-versions/{version_id}` | §3.3/§3.6 | exact lookup, tenant-scoped, `DOCUMENT_READ`, access-logged; §3.3 field names (`version_id`, `content_hash`, `detected_mime_type`, `malware_status`, `commit_status`, ...). |
| `POST /v1/documents/{id}/upload-reservations`, `POST /v1/upload-reservations/{id}/complete` | §3.4 | reservation state machine (RESERVED/COMMITTED/FAILED/QUARANTINED), `Idempotency-Key`, size/content-type/malware gates (fail closed), single transactional commit, idempotent replay. Content is carried inline (`content_base64`). |
| `POST /v1/records:declare`, `POST /v1/records/{id}:supersede` | §9.2 | one declaration path; supersede records a `SUPERSEDES` relationship (`record_superseded` event). |
| `POST /v1/retention:evaluate` | §9.2 | resolves ACTIVE rules by class + jurisdiction at `as_of` (default now) honouring each rule's `effective_from`/`effective_to`; reports conflicts and hold status, never picks between rules; publishes `retention_conflict_detected` once per (record, rule set). Binds nothing. Max 100 records, per-entity authorization. |
| `POST /v1/dispositions`, `POST /v1/dispositions/{id}:approve` | §9.2 | adapters over the existing state machine; legal hold blocks (409). Stops at `APPROVED_FOR_DISPOSITION`; nothing is deleted. |
| `POST /v1/legal-holds/{id}:release`, `:resolve-targets` | §9.2/§5.6 | resolve-targets covers the explicit-record scope: per-record outcome, immutable snapshot, partial failures reported (`legal_hold_partial_application`). |
| `POST /v1/exports` | §9.2/§6.5 | requires `recipient_scope` and `purpose`; `/v1/export-packages` keeps them optional. |

`GET /v1/documents/{id}` also returns `current_version_id` (the version whose number is the document's `current_version`) and `classification_id` (the current CONFIRMED classification; left out while the classification is only a CANDIDATE, since §3.2 says "approved"). `POST /v1/records:declare` refuses a missing `business_context` or `declaration_reason` with `DRC-005: RECORD_DECLARATION_INCOMPLETE` (§4.2 business-context gate, §4.3 mandatory `declaration_reason`).

Documents carry `row_version` (§3.2: database-maintained, 1 at creation, +1 on every change to the row, not settable by a client) and `document_state` (only where the service status is itself a §3.2 state: ACTIVE, SUPERSEDED). A cross-tenant `tenant_id` in a body is refused with `DRC-004: TENANT_SCOPE_MISMATCH` and a security log line; cross-tenant ids in other positions are indistinguishable from "not found" by design (row-level security).

Older paths keep working and share the same handlers.

## Not implemented, and why

| Spec item | Status | Reason / decision needed |
|---|---|---|
| `PATCH /v1/documents/{id}` (`If-Match`), `POST .../lock`, `POST .../withdraw` | EXTERNAL IAM DEPENDENCY + NEEDS CLARIFICATION | No `DOCUMENT_UPDATE`/`LOCK`/`WITHDRAW` permission exists in the taxonomy (IAM owner must add it). Mutable-field set, workflow-decision reference and missing-`If-Match` status are undefined; no `row_version` or `DRAFT`/`LOCKED`/`WITHDRAWN` state. |
| `signature-envelopes`, `:send`, `signature-callbacks/{provider}` (DRC-05) | EXTERNAL / WRONG SERVICE OWNERSHIP | Owned by `esignature-integration-svc`; OD-S09 to OD-S11 open. |
| Disposition execution / deletion | NEEDS CLARIFICATION | OD-S03 and backup-deletion model OD-S05. |
| Hold scope selectors other than explicit records | NEEDS CLARIFICATION | Scope query language OD-S04; unsupported selectors are refused. |
| Object-store scoped credentials / `encryption_key_ref` | EXTERNAL | Storage is a local AES-GCM file backend; no credentials, region or key reference. |
| Metadata-only `POST /v1/documents` | PARTIAL | Needs `DRAFT` state; create still ingests the first version. |
| Retention applicability by legal entity / data class | NEEDS CLARIFICATION | §5.2 gives a rule version no such field. Effective dates are implemented. Precedence between conflicting rules stays undecided (OD-S03); conflicts are reported, and the legal-review case belongs to the workflow service (EXTERNAL). |
| `content_object_id`, `encryption_key_ref` (§3.3) | EXTERNAL | No content-object entity or KMS reference exists; storage is a local file backend. `created_from_version_id`, `domain_owner`: population rules undefined. |
| Metadata-only create / `DRAFT` | NEEDS CLARIFICATION | §3.2 lists DRAFT as a value but defines no initial state for metadata-only create, no DRAFT-to-ACTIVE transition, and no relation to the reservation flow; the model assumes every document has a version. |
| Rule fields `priority`, `rule_class`, `trigger_field`, structured duration (§5.2) | NEEDS CLARIFICATION | §5.2 names them but gives no identifier set for `rule_class`, no type for `priority`, and no way the engine should use them (OD-S03). |
| `document_state` for RETAINED / PURGE_PENDING / ARCHIVED | NEEDS CLARIFICATION | No §3.2 equivalent; not guessed. |
| `disposition_started/completed/failed`, `document_version_quarantined`, `record_declared` | NEEDS CLARIFICATION | No execution exists to start or complete; a quarantined upload creates no version to anchor `version_id`; `record_declared` needs `record_class_id` (UUID file-plan id) and `schedule_state`, neither modelled. Existing `record.declared` and `document.version_upload_quarantined` are unchanged. |
| Event names | PARTIAL | `record.declared` and quarantine events keep their existing names. |
| Error response structure | NEEDS CLARIFICATION | §11.1 defines codes only; codes appear in `detail` and the `{error, detail}` body is unchanged. |

## Tests

```bash
cd services/document-vault-svc
go vet ./... && go build ./...
# DB-backed tests need a disposable database (the store suite DROPs tables):
REQUIRE_DB_TESTS=1 TEST_DATABASE_URL='postgres://USER:PASS@HOST:PORT/document_vault_test?sslmode=disable' go test ./...
```

`TestPublishOutbox_AgainstRealBroker` is skipped unless `KAFKA_TEST_BROKER` is set.
