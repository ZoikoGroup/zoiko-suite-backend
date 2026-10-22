# evidence-manifest-svc (:8095)

Builds structured, checksummed evidence sets (manifests) for audit, regulator,
legal-discovery and compliance-review scenarios, and also hosts the AUD-03 audit
population and AUD-04 sampling surface.

Documentation: `docs/architecture/03-microservices.md` §14.4 (purpose and the
`evidence.manifest.generated` event) and `docs/architecture/04-data-model.md` §15
(`EvidenceManifest`, `EvidenceManifestItem`). Neither defines endpoints, schemas,
status codes or permission names. `docs/audit_files/Evidence, Audit & Utility.md`
is an older internal audit report, not a specification; it cites
`03_service_inventory_and_specifications.md`, which is not in the repository.

## Behaviour that is enforced

- Tenant isolation: forced row-level security on every table; every store call is tenant-scoped.
- Authorization (`EVIDENCE_MANIFEST_GENERATE`, `EVIDENCE_MANIFEST_READ`), fail-closed (403 on denial, 503 when authorization-svc is unreachable), scoped to the manifest's own legal entity.
- `GET /v1/evidence-manifests` always authorizes: with `legal_entity_id` that entity is checked; without it every legal entity that has a manifest is checked and only readable ones are listed (403 if manifests exist but none is readable).
- Generation is all-or-nothing: manifest, records, checksum, GENERATED status and the outbox event commit in one transaction; any source failure records a FAILED manifest and returns 503.
- A manifest row is immutable once GENERATED or FAILED (database trigger; migration 000008). Only PENDING -> GENERATED/FAILED is permitted, and identity/scope columns never change. `manifest_records` are append-only.
- The download archive is assembled and every write checked before any response is sent; a packaging failure is a 500, never a truncated 200.
- Checksum: one flat SHA-256 over source type, source record id and snapshot in canonical read order. No Merkle tree, no per-item hash.

## Not implemented, and why

| Item | Status |
|---|---|
| Permission names `EVIDENCE_GENERATE` / `EVIDENCE_READ` / `EVIDENCE_DOWNLOAD` (older report) | DOC DRIFT / NEEDS CLARIFICATION: no specification names them; download and verify reuse `EVIDENCE_MANIFEST_READ`. |
| "Signed" download zip | DOC DRIFT / NEEDS CLARIFICATION: no signing contract, key source or signature format is defined. |
| Document-vault and audit-event-store as manifest sources | CROSS-SERVICE / NEEDS CLARIFICATION: no authoritative contract for either. |
| Data-model fields `scenario_reference`, `source_service`, `integrity_hash`, `generated_by`, `manifest_type` | NEEDS CLARIFICATION: no defined source or population rule; current names (`manifest_id`, `scenario_type`, `status`, `requested_by`, `source_type`, `source_record_id`) are kept. |
| AUD-03 `GetManifest`, `GetLineage`, `ComparePopulationVersions`, `ListLateItems`; AUD-04 `GetEvaluation`, `GetProjection` | NEEDS CLARIFICATION / SERVICE OWNERSHIP: the Audit & Assurance specification does not say this service owns them or define their responses. |
| Error body, rate limiting | NEEDS CLARIFICATION / NOT SPECIFIED. |

## Tests

```bash
cd services/evidence-manifest-svc
go vet ./... && go build ./...
TEST_DATABASE_URL='postgres://USER:PASS@HOST:PORT/evidence_manifest_test?sslmode=disable' go test ./...
```

The store suite DROPs tables: use a disposable database. Without `TEST_DATABASE_URL`
the database tests skip. `TestPublishOutbox_AgainstRealBroker` skips without a broker.
