# evidence-requirements-svc — Progress

**Status (21 Oct 2026):** built and in service; Group 2 completion pass done.
The previous file said "NOT STARTED — zero code written", which had not been
true since July. Design rationale stays in `context.md`; the contract is
`openapi.yaml` (kin-openapi valid, route parity tested).

## Built

| Concern (Doc 03 §8.6) | Where |
|---|---|
| Evidence preconditions, sufficiency logic | `POST /v1/evidence/evaluate` → SATISFIED / MISSING / NO_REQUIREMENTS_DEFINED; data-driven `RequirementSpec`, no jurisdiction branches; a malformed rule fails closed |
| Document requirements | SUPPORTING_DOCUMENT references verified against document-vault-svc (existence, tenant, entity), envelope forwarded |
| Signature / supporting-artifact rules | `evidence_type` + `artifact_subtype` + `minimum_count`; the same artifact (canonical reference) counts once |
| Catalogue | effective-dated requirements, tenant-wide or per entity; retirement by end-dating only |
| Ledger | `evidence_evaluations`, append-only (UPDATE / DELETE / TRUNCATE refused, 000007 + 000009), records the requirement set each determination was tested against |
| Provided-by-client requests | `/v1/pbc/*` (000004) |
| Events | `evidence.requirement.satisfied` / `.missing` through the transactional outbox (000008), envelope with causation, jurisdiction and governing refs, key `<tenant>/<evaluation>` |

## Controls

- Identity-envelope gate (signed envelope + session status; or a verified
  mTLS peer workload); compose `warn` until every caller forwards it.
- Authorization at the legal entity: EVIDENCE_EVALUATE (compose `warn` until
  the out-of-domain callers' principals are granted), EVIDENCE_REQUIREMENT_READ,
  EVIDENCE_EVALUATION_READ, EVIDENCE_REQUIREMENT_CREATE / _RETIRE. Tenant-wide
  catalogue changes authorize at PLATFORM (they used to send the tenant id as
  the entity, which authorization-svc cannot resolve).
- Legal entity server-validated against tenant-entity-registry-svc.
- FORCE RLS with NULLIF; explicit tenant predicate on every query.
- Idempotency-Key on every write; a changed body is 409.
- RFC 9457 errors with the §16 class; missing tenant is 401 everywhere.

## Open, outside this service

- Callers (board-resolutions, corporate-actions, filing-preparation) send an
  incomplete envelope; corporate-actions and filing-preparation fail open on
  an unknown outcome; general-ledger, financial-close, vat-gst and
  corporate-tax have no gate. All outside the Identity and Governance domains.
- document-vault-svc must grant DOCUMENT_READ to the evaluating principal; the
  document-status question (which statuses count) is unspecified.
- Non-document references (manifests, signatures) are not verified: no
  owning service exposes them yet.

## Deploy

1. Migration **000009** (ledger TRUNCATE guard).
2. `TENANT_REGISTRY_URL` (required outside local/dev), `IDENTITY_*`,
   `EVIDENCE_EVALUATE_AUTHZ` / `EVIDENCE_READ_AUTHZ` (default enforce).
3. access-control-svc 000021 registers EVIDENCE_EVALUATE / _REQUIREMENT_READ /
   _EVALUATION_READ; grant them.
4. Build from the repository root: `docker build -f services/evidence-requirements-svc/Dockerfile .`
