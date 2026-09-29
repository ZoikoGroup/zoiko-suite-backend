# privacy-purpose-registry-svc (PRV-01)

**Default Port:** `8151`  
**Classification:** Interlinked Service  
**Specification:** `ZS-SVC-W-001` (§8, §9, §18, §18.1, §19, §31, §32)  
**Role:** Registry of lawful processing purposes and processing activities (GDPR Article 30 RoPA) providing the legal and operational baseline for all privacy decisions, consent tracking, and data transfers.

---

## Architecture & Responsibilities

`privacy-purpose-registry-svc` governs the creation, review, activation, and retirement of:
1. **Lawful Purposes:** Defined purpose statements with compatibility classes and lawful basis references (immutable once published).
2. **Processing Activities (RoPA):** Governed data processing definitions specifying controller/processor roles, owners, purpose linkages, data categories, subject classes, jurisdictions, retention rules, notice/consent dependencies, and DPIA/TIA assessments.

### Downstream Dependencies
- `privacy-consent-svc` (8152): Validates that a purpose is registered and `PUBLISHED` prior to recording consent.
- `privacy-decision-svc` (8153): Validates that an activity is `ACTIVE` and that linked purposes are `PUBLISHED` during runtime access evaluation.
- `privacy-transfer-svc` (8155): Validates `purpose_activity_refs` against registered processing activities.

---

## Minimum Activation Gates (§8.2)

An activity version cannot transition from `DRAFT` to `VALIDATED` unless all 8 minimum activation gates pass structural validation. If any gate fails, the version remains `DRAFT` (PRV-I13: findings are fail-closed, never partial-permit):

| Gate | Requirement | Rule / Field | Error Code (§32) |
|---|---|---|---|
| **Gate 1** | Named accountable business owner | `owner` non-empty | `PRV-003` |
| **Gate 2** | Controller/processor role resolved | `privacy_role` in `CONTROLLER`, `PROCESSOR`, `JOINT_CONTROLLER` | `PRV-003` |
| **Gate 3 & 4** | Specific purpose statement & published references | `purpose_ids` non-empty and each purpose is `PUBLISHED` | `PRV-001` |
| **Gate 5** | Data subjects, categories, jurisdictions enumerated | `jurisdictions` (`PRV-004`), `subject_classes` (`PRV-010`), `data_categories` (`PRV-010`) | `PRV-004`, `PRV-010` |
| **Gate 6** | Retention/DRC rule references identified | `retention_rule_refs` non-empty | `PRV-014` |
| **Gate 7** | Notice/consent dependency explicitly declared | `notice_consent_dependency` in `REQUIRED`, `NOT_REQUIRED`, `CONDITIONAL` (never implicit) | `PRV-006` |
| **Gate 8** | DPIA/TIA requirement resolved or marked review-required | `dpia_tia_status` in `RESOLVED`, `REVIEW_REQUIRED`, `NOT_REQUIRED` | `PRV-016` |

---

## Activity Lifecycle State Machine (§9, Figure 4)

```
[ DRAFT ] ─────────── (Validate - 8 Gates) ───────────> [ VALIDATED ]
    ▲                                                          │
    │ (Supersede)                                          (Submit)
    │                                                          ▼
[ REJECTED ] <─────── (Reject - SoD) ───────────────── [ SUBMITTED ]
                                                               │
                                                          (Approve - SoD)
                                                               ▼
                                                         [ APPROVED ]
                                                               │
                                                           (Activate)
                                                               ▼
                                                          [ ACTIVE ]
                                                            │    ▲
                                                   (Suspend)│    │(Resume)
                                                            ▼    │
                                                      [ SUSPENDED ]
                                                            │
                                                        (Retire)
                                                            ▼
                                                        [ RETIRED ]
```

### State Transitions
1. `DRAFT -> VALIDATED`: Via `/validate` once all 8 gates are satisfied without findings.
2. `VALIDATED -> SUBMITTED`: Submits the version for review.
3. `SUBMITTED -> APPROVED`: Approved by a reviewer satisfying Segregation of Duties.
4. `SUBMITTED -> REJECTED`: Rejected with a mandatory `reason`. REJECTED is an immutable terminal state (PRV-I20); remediation occurs by creating a successor draft via `POST /{id}/versions` with `parent_version_id`.
5. `APPROVED -> ACTIVE`: Sets `effective_from` timestamp.
6. `ACTIVE <-> SUSPENDED`: Temporary halt and resumption of processing operations.
7. `ACTIVE / SUSPENDED -> RETIRED`: Permanent decommission of the processing activity version.

---

## Canonical & Version-Explicit API Routes (§9.1, §18)

Both canonical latest-version aliases and explicit version endpoints are supported:

### Processing Activities
| Canonical Path (§9.1) | Explicit Version Path | Method | Description |
|---|---|---|---|
| `/privacy/processing-activities` | — | `POST` | Create activity and initial draft version |
| `/privacy/processing-activities/{id}` | — | `GET` | Resolve activity as of `?as_of=` |
| `/privacy/processing-activities/{id}/versions` | — | `POST` | Create successor draft version from parent |
| `/privacy/processing-activities/{id}/versions/{vid}` | — | `GET` | Get exact version |
| `/privacy/processing-activities/{id}/validate` | `.../versions/{vid}/validate` | `POST` | Evaluate 8 activation gates |
| `/privacy/processing-activities/{id}/submit` | `.../versions/{vid}/submit` | `POST` | Submit validated version for review |
| `/privacy/processing-activities/{id}/approve` | `.../versions/{vid}/approve` | `POST` | Approve submitted version (SoD enforced) |
| `/privacy/processing-activities/{id}/reject` | `.../versions/{vid}/reject` | `POST` | Reject submitted version with reason (SoD enforced) |
| `/privacy/processing-activities/{id}/activate` | `.../versions/{vid}/activate` | `POST` | Activate approved version |
| `/privacy/processing-activities/{id}/suspend` | `.../versions/{vid}/suspend` | `POST` | Suspend active version |
| `/privacy/processing-activities/{id}/resume` | `.../versions/{vid}/resume` | `POST` | Resume suspended version |
| `/privacy/processing-activities/{id}/retire` | `.../versions/{vid}/retire` | `POST` | Retire active or suspended version |

### Purpose Registry & RoPA
| Path | Method | Description |
|---|---|---|
| `/privacy/purposes` | `POST` | Create purpose and initial draft version |
| `/privacy/purposes` | `GET` | List all currently `PUBLISHED` purposes |
| `/privacy/purposes/{id}` | `GET` | Resolve purpose as of `?as_of=` |
| `/privacy/purposes/{id}/versions` | `POST` | Create successor draft version |
| `/privacy/purposes/{id}/versions/{vid}/publish` | `POST` | Publish purpose version (SoD enforced, immutable) |
| `/privacy/ropa` | `GET` | Article 30 RoPA filtered by `?role=` and `?jurisdiction=` |

---

## Security & Governance Controls

### 1. Segregation of Duties (Maker-Checker, §18)
- A principal cannot publish their own purpose version (`CreatedByPrincipalID != PrincipalID`). Violation returns `403 Forbidden`.
- A principal cannot approve their own processing activity version (`CreatedByPrincipalID != PrincipalID`). Violation returns `403 Forbidden`.
- A principal cannot reject their own processing activity version (`CreatedByPrincipalID != PrincipalID`). Violation returns `403 Forbidden`.

### 2. Idempotency & Concurrency (§18.1)
- Clients may supply an `Idempotency-Key` header on any mutating `POST` request.
- Requests are hashed (`SHA-256(method + path + body)`) and verified against `purpose_registry_idempotency_keys`.
- Identical replays receive the cached response with `Idempotency-Replay: true`.
- Modified payloads under an existing key return `409 Conflict`.
- Uninstrumented calls (no header) proceed normally without degradation.

### 3. Fail-Closed Authorization & Tenant Isolation
- Mutating routes require `X-Principal-Id` (401 if missing).
- Calls to `authorization-svc` are fail-closed: denial yields `403 Forbidden`, unreachable service yields `503 Service Unavailable`.
- Postgres Row-Level Security (RLS) is strictly enforced via session-variable transaction scoping: `SELECT set_config('app.tenant_id', $1, true)`.
- Immutability database triggers prevent any modification of `PUBLISHED` purpose versions or `ACTIVE` activity version contents.

---

## Database Migrations

- `000001_initial_schema.up.sql`: Initial purposes and processing activities schema.
- `000002_immutability_triggers.up.sql`: DB triggers preventing post-publication/activation content mutations.
- `000003_add_rls.up.sql`: Row-Level Security policies on purpose and activity tables.
- `000004_activation_gates_and_idempotency.up.sql`: Adds `notice_consent_dependency`, `dpia_tia_status`, updates immutability triggers, and creates `purpose_registry_idempotency_keys` table with RLS.

---

## Kafka Events Emitted (§19)

- `privacy.purpose.published`
- `privacy.processing_activity.submitted`
- `privacy.processing_activity.approved`
- `privacy.processing_activity.rejected`
- `privacy.processing_activity.activated`
- `privacy.processing_activity.suspended`
- `privacy.processing_activity.resumed`
- `privacy.processing_activity.retired`

---

## Local Verification & Health Checks

```bash
# Run unit & integration tests
go test -v ./...

# Run code analysis
go vet ./...

# Build binaries
go build -o server ./cmd/server
go build -o healthcheck ./cmd/healthcheck

# Health probes
curl http://localhost:8151/healthz
curl http://localhost:8151/readyz
```
