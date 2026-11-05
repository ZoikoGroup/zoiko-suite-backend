# evidence-requirements-svc — Release Certificate

## Release Metadata
- **Service:** evidence-requirements-svc
- **Version:** 1.0.0
- **Date:** 2026-11-05
- **Git Commit:** (to be filled at release)
- **Built By:** (to be filled at release)
- **Platform:** zoiko-suite-backend-platform

## Scope
Governance Platform 7/7 — Evidence Requirements Service. Determines what supporting evidence must exist before an action may be completed. Owns evidence preconditions, document requirements, signature requirements, supporting artifact rules, and evidence sufficiency logic.

## Specification Compliance
| Document | Section | Status |
|---|---|---|
| 03-microservices.md | §8.6 | ✅ Implemented |
| 04-data-model.md | §7.1 (EvidenceRequirement) | ✅ Implemented |
| 03-microservices.md | §17.6 (Material Service Must Be Evidential) | ✅ Implemented |
| 03-microservices.md | §23 (Tier classification) | ⚠️ Open (D-ER-1) |

## Feature Completeness
| Feature | Spec Ref | Status |
|---|---|---|
| Evidence evaluation (SATISFIED/MISSING/NO_REQUIREMENTS_DEFINED) | §8.6 | ✅ |
| Requirement catalogue (create, list, get, end-date) | §7.1 | ✅ |
| Document verification (document-vault-svc) | §8.6 | ✅ |
| Manifest verification (evidence-manifest-svc) | §8.6 | ✅ |
| Signature verification (esignature-integration-svc) | §8.6 | ⚠️ Blocked (no scoped read) |
| Supporting artifact rules (minimum_count, artifact_subtype) | §7.1 | ✅ |
| Append-only evaluation ledger | §17.6 | ✅ |
| Outbox events (satisfied/missing) | §8.6 | ✅ |
| PBC requests (14 routes) | AUD-05 | ✅ |

## API Contract
- **OpenAPI:** openapi.yaml (kin-openapi valid, route parity tested)
- **AsyncAPI:** asyncapi.yaml (events: evidence.requirement.satisfied, evidence.requirement.missing)
- **Authentication:** Identity envelope (Bearer JWT), workload mTLS
- **Authorization:** EVIDENCE_EVALUATE, EVIDENCE_REQUIREMENT_READ, EVIDENCE_EVALUATION_READ, EVIDENCE_REQUIREMENT_CREATE, EVIDENCE_REQUIREMENT_RETIRE, EVIDENCE_REQUEST_CREATE, EVIDENCE_REQUEST_MANAGE, EVIDENCE_REQUEST_SUBMIT_RESPONSE, EVIDENCE_REQUEST_EVALUATE
- **Idempotency:** Idempotency-Key required on all mutations
- **Optimistic Concurrency:** If-Match / ETag on all mutations (API-3)
- **Pagination:** API-4 cursor pagination on all list endpoints
- **Errors:** RFC 9457 problem+json with error_class

## Security
| Control | Status |
|---|---|
| Row-level security (RLS) + explicit tenant_id | ✅ |
| Identity envelope verification (AU-01) | ✅ |
| Session revocation checking (AU-01) | ✅ |
| DPoP sender constraint (AU-27) | ✅ |
| Workload mTLS peer verification | ✅ |
| Fail-closed on all upstream unavailability | ✅ |
| No soft-delete (end-dating only) | ✅ |
| Append-only evaluation ledger | ✅ |
| Request body limit (256 KiB) | ✅ |

## Observability
| Signal | Endpoint | Status |
|---|---|---|
| Health (liveness) | GET /healthz | ✅ |
| Readiness (DB connectivity) | GET /readyz | ✅ |
| Prometheus metrics | GET /metrics | ✅ |
| OpenTelemetry traces | OTLP/HTTP to otel-collector:4318 | ✅ |
| Structured logging | zap JSON to stdout | ✅ |

## Database
- **Schema:** deployments/migrations/000001–000010
- **Tables:** evidence_requirements, evidence_evaluations, evidence_requests, evidence_request_responses, evidence_request_receipts, evidence_request_notes, outbox
- **RLS:** Enabled with tenant_isolation_policy on all tenant tables
- **Immutability:** Trigger guard on evidence_evaluations (000009)
- **Outbox:** Transactional with 000008

## Dependencies
| Service | Purpose | Required |
|---|---|---|
| postgres | Primary storage | Yes |
| kafka | Event broker (outbox) | Yes |
| authorization-svc | Admin gating, authz decisions | Yes |
| document-vault-svc | SUPPORTING_DOCUMENT verification | Yes |
| tenant-entity-registry-svc | Legal entity validation, jurisdiction resolution | Yes |
| identity-context-svc | Identity envelope verification, revocation | Yes |
| evidence-manifest-svc | EVIDENCE_MANIFEST verification | No (local/dev optional) |
| esignature-integration-svc | SIGNATURE verification | No (local/dev optional) |

## Test Results
| Suite | Tests | Pass | Fail | Skip |
|---|---|---|---|---|
| internal/handler | 52 | 52 | 0 | 0 |
| internal/events | 5 | 5 | 0 | 0 |
| internal/registry | 1 | 1 | 0 | 0 |
| internal/store | 19 | 1 | 0 | 18 (no TEST_DATABASE_URL) |

## Known Deviations (SPEC_DEVIATIONS.md)
| ID | Deviation | Impact |
|---|---|---|
| D-ER-1 | Tier classification unresolved | Label only; built to Tier 0 discipline |
| D-ER-2 | Scan-result ownership interim | EVIDENCE_REQUEST_MANAGE used; no scanner identity |
| D-ER-3 | Document/signature statuses not specified | Verify existence/scope only |
| D-ER-4 | Nullable legal_entity_id | NULL = tenant-wide; COALESCE to zero UUID |
| D-ER-5 | NO_REQUIREMENTS_DEFINED caller-decided | Distinct outcome, never SATISFIED |
| D-ER-6 | Compose gate modes warn | EVIDENCE_EVALUATE_AUTHZ=warn, IDENTITY_VERIFY=warn |

## Blockers
| Item | Blocker | Owner |
|---|---|---|
| Signature verification | esignature-integration-svc exposes no scoped read | esignature team |
| Caller-side gate wiring | 4 of 6 callers not yet wired | Respective service teams |
| Caller envelope completeness | 3 callers send incomplete envelope | Respective service teams |

## Rollback Plan
1. Stop new container
2. Start previous image
3. If schema changed: `migrate -path deployments/migrations down 1`
4. Verify `/readyz` returns 200

## Sign-off
- [ ] Platform Governance Lead
- [ ] Security Review
- [ ] Architecture Review
- [ ] On-call Handoff Complete

---
*This certificate is generated as part of the S2 Contract & Docs deliverable (ER-6).*