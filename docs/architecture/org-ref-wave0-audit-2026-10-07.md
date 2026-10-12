# ORG / REF baseline — Wave 0 audit (2026-10-07)

Spec: `ZoikoSuite_Organization_Legal_Entity_Global_Reference_Data_Detailed_Service_Specifications.docx` (ZS-SVC-I-001).

**Method and limits.** Five per-service audits and one ownership investigation, all read-only
(greps and targeted reads; no builds, no tests run). "MISSING" means no hit in the service
tree. Every finding below is an agent's reading of the code and has **not** been independently
re-verified; items marked **verify** should be confirmed before anyone acts on them.

## 1. Status of all 20 services

| ID | Existing service | Score | Verdict |
|---|---|---|---|
| ORG-01 Identity | `identity-context-svc` | ~8% | Built for GOV-01, not ORG-01. No SubjectIdentity/IdP link/step-up/assurance. |
| ORG-02 Tenant | `tenant-entity-registry-svc` | 98% (repo audit, 28 Sep) | Built. Open: ONBOARDING treated as operable (known-gaps.md). |
| ORG-03 Legal Entity | `tenant-entity-registry-svc` | 98% (same) | Built. |
| ORG-04 Group & Ownership | none | 0% | Missing. `consolidation-svc` takes the child list from the request body. |
| ORG-05 Org Unit | `org-structure-svc` | ~3–17% | Older flat dept/position model. Rebuild the unit/hierarchy model. |
| ORG-06 Role & Delegation | `delegated-authority-svc` | ~62–69% (delegation half) | Role-assignment half is split across other services. |
| ORG-07 Party | `counterparty-management-svc` | ~10% | Flat vendor/customer registry + CRM. Not a golden record. |
| ORG-08 Address & Establishment | none | 0% | Missing. |
| ORG-09 Registration & License | none | 0% | Missing. |
| ORG-10 Payee Master | `payee-banking-identity-svc` | ~54% | Core state machine good; security gaps below. |
| REF-01 Jurisdiction | `jurisdiction-rules-svc` | ~12% | Basic table inside a rule-pack service. |
| REF-02 … REF-10 | none | 0% | Missing (9 services). |

Net: 2 built (ORG-02/03 are one service), 1 half-built (ORG-10), 17 to build or substantially rework.

## 2. Cross-cutting gaps (seen in nearly every audited service)

- **No transactional outbox** in `org-structure-svc`, `counterparty-management-svc`, `payee-banking-identity-svc`, `jurisdiction-rules-svc`. Events are published after commit and failures are swallowed. `identity-context-svc` has a real outbox that can be reused.
- **No `expected_version` / VERSION_CONFLICT** on writes.
- **Free-text errors** instead of the spec's typed codes (CONTEXT_INVALID, VERSION_CONFLICT, INVALID_TRANSITION, SOD_DENIED, REFERENCE_RETIRED …).
- **In-place mutation / no bitemporal history** (ORG-05, ORG-07, REF-01).
- **Event names/payloads** lack object version and `effective_at`.

→ Proposal: a shared `_contract` helper package (outbox writer, expected-version guard, typed error set, event envelope with version/effective_at) so new services don't each re-implement it and the four weak services can adopt it.

## 3. Priority fixes on existing code (before or alongside new builds)

All **verify** first.

1. **ORG-10 account number exposure.** Audit reports the full account is stored in plaintext (no tokenization/KMS) and the unmasked object is sent in Kafka payloads. Treat as a security finding.
2. **ORG-10 fail-open.** `payment-authorization-svc` proceeds unpinned if the destination lookup fails at request time; spec requires block.
3. **ORG-10 events.** Verified event never reaches Kafka; implicit supersession inside Activate emits nothing; no `TenantID` on several publishes.
4. **ORG-10 verification** accepts any non-empty string as an "independent" method; no `VerifyAuthorizationFingerprintInput`; no idempotency or `expected_version`; routes are `/org10/...` not `/v1/payee-destinations/{id}:verb` (the AP-10 client hard-codes the old path — rename both together).
5. **Period gate fail-open.** `financial-close-svc` returns OPEN for an unregistered period and `general-ledger-svc` treats 404 as open, so a missing period row bypasses the posting gate.

## 4. Ownership conflicts and recommended resolutions

| # | Conflict | Recommendation |
|---|---|---|
| 1 | `financial-close-svc` owns period state (spec: REF-05) and close orchestration (ACC-14) | New `accounting-period-svc` (REF-05); `financial-close-svc` becomes ACC-14 only and calls REF-05 commands. Unregistered period fails closed. Backfill OPEN→Open, CLOSED→SoftClose, LOCKED→HardClosed; keep old status endpoint as a proxy during cutover. |
| 2 | `consolidation-svc` asserts group membership from the request | Build ORG-04; runs carry `group_membership_version_id` + `as_of_date`; request can only narrow the graph. Seed from existing runs (unverified), log mismatches before enforcing. |
| 3 | Roles/delegations in `authorization-svc` (`principal_role_assignments`, `delegated_authorities`), `delegated-authority-svc` (`delegation_grants`) and served from `identity-context-svc` | `delegated-authority-svc` becomes ORG-06 (add role assignments + lifecycle). `authorization-svc` keeps definitions/SoD and reads a projection. identity-context endpoints read through. |
| 4 | Payment terms inside `supplier-financial-profile-svc`; free-text UoM in `goods-service-receipt-svc` | Replace with `payment_term_id`+`term_version` and `uom_id`+rule version; nullable → backfill → validate → NOT NULL. |
| 5 | ORG-07 placement | Extend `counterparty-management-svc` (owns the `counterparty_id` that ORG-10 depends on). Four new tables: profile versions, roles, external ids, merge/split history. Keep CRM in its own package. |
| 6 | REF-01 placement | Split into a `ref-jurisdiction-svc`; keep old table as read-only mirror until `jurisdiction_rules` / `pack_version_jurisdictions` are repointed. |
| 7 | ORG-01 placement | Open: new identity-master service vs extend `identity-context-svc` (spec says ORG-01 must not absorb authorization authority). |

## 5. Migration surface (candidates; from column-name greps, verify)

- `fiscal_period` as VARCHAR: general-ledger, consolidation, asset-management, inventory-management, financial-control, financial-close (+ possibly project-accounting).
- `book` / `book_id`: ~8 services; nullable in GL because REF-06 doesn't exist.
- `currency`: ~43 services as free text — the largest migration; do it late and incrementally.
- Dimensions: AP, AR, GL (per line), commercial-account, expense-claim, project-accounting, others.
- Payment terms: supplier-financial-profile. UoM: goods-service-receipt, inventory-management.

## 6. Decisions required from the owner (Wave 0 exit)

1. Approve REF-05 as a new service and `financial-close-svc` reduced to ACC-14.
2. ORG-04 as a new service (recommended) vs module of `org-structure-svc`.
3. ORG-06 owner: `delegated-authority-svc` (recommended).
4. ORG-07: extend `counterparty-management-svc` (recommended).
5. REF-01: split out (recommended) vs keep inside `jurisdiction-rules-svc` as a stopgap.
6. ORG-01: where the identity master lives.
7. Spec §11 pre-production decisions still open: ISO/GLEIF source mechanism, FX source hierarchy and manual-rate authority, book taxonomy, policy approval board, fiscal-calendar transition governance, party match thresholds, payee verification tiers, dimension stewardship, payment-term override authority.

## 7. Revised wave order

0. Decisions above + verify §3 items + shared `_contract` helper.
1. Fix ORG-10 security items (§3) — independent of everything else.
2. REF-02 Currency, REF-09 UoM (leaf services).
3. REF-04 Calendar, REF-05 Period (+ GL/close cutover), REF-06 Book.
4. REF-07 Policy, REF-08 Dimensions.
5. REF-03 FX, REF-10 Calendar/Terms.
6. ORG-08, ORG-09, ORG-04; ORG-07 uplift; ORG-05 rebuild; ORG-06 role half; REF-01 split; ORG-01.
7. Cross-domain negative tests (spec §8, 48 paths), DR/restore, performance.
