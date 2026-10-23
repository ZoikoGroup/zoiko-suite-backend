# Implementation progress

Plan: `docs/audit_files/IMPLEMENTATION-PLAN-2026-10-23.md`. Update this file after every task.
Statuses: TODO · IN PROGRESS · DONE · BLOCKED. DONE needs a filled evidence block (plan §7).

## Status board

| Task | Title | Status | Commit(s) | Note |
|---|---|---|---|---|
| P0-1 | Commit pending authorization work | TODO | | |
| P0-2 | Register JURISDICTION_RULE_VALIDATE | TODO | | |
| P0-3 | Fix stale resolve.go comment | TODO | | |
| API-1 | Shared apicontract package | TODO | | |
| API-2 | Problem details + request id + cache, all our services | TODO | | |
| API-3 | ETag / If-Match on aggregate mutations | TODO | | |
| API-4 | Cursor pagination on lists | TODO | | |
| API-5 | Async operations | TODO | | |
| API-6 | Deprecation / Sunset on renamed routes | TODO | | |
| G-10 | Legal hold | TODO | | |
| G-09 | Retention and disposition | TODO | | |
| G-07 | Evidence ledger and verification | TODO | | |
| G-08 | Schema registry lifecycle | TODO | | |
| G-11 | Feature policy and capability exposure | TODO | | |
| G-02 | GTRM reads + compiler commands | TODO | | |
| G-05/03/04/12 | Named-operation parity | TODO | | |
| IAM-1 | AU-12 per-service audience | TODO | | |
| IAM-2 | AU-14 entitlement stage | TODO | | |
| IAM-3 | AU-15 legal-hold negative control | TODO | | after G-10 |
| IAM-4 | AU-16 relationship / qualification predicates | TODO | | after ORG-4, ORG-5 |
| IAM-5 | AU-18 authoritative FX | TODO | | after ORG-8 |
| IAM-6 | AU-19 observability | TODO | | |
| IAM-7 | AU-20 DoD artefacts | TODO | | |
| IAM-8 | AU-21 load + revocation SLOs | TODO | | |
| IAM-9 | AU-22 role-profile drift | TODO | | |
| IAM-10 | AU-25 canonical event names | TODO | | after G-08 |
| IAM-11 | AU-26 /v1/iam aliases | TODO | | |
| IAM-12 | Federation / SCIM / passkey acceptance | TODO | | |
| IAM-13 | ORG-06 remaining operations | TODO | | |
| WF-1 | WFC-01..03 open rows | TODO | | |
| WF-2 | WFC-04 cases | TODO | | |
| WF-3 | WFC-05 deadline clocks | TODO | | |
| WF-4 | Group 2 open rows inside our services | TODO | | after Phase 1 |
| ST-1 | State-machine definition registry | TODO | | |
| ST-2 | Available actions + transition history | TODO | | |
| ST-3 | No generic status writes | TODO | | |
| ORG-1 | ORG-01 identity | TODO | | |
| ORG-2 | ORG-03 name parity | TODO | | |
| ORG-3 | ORG-04 ownership and control | TODO | | |
| ORG-4 | ORG-05 org units + org graphs | TODO | | |
| ORG-5 | Parties + golden record | TODO | | |
| ORG-6 | Addresses and establishments | TODO | | |
| ORG-7 | Registrations, licences, identifiers | TODO | | |
| ORG-8 | Reference sets (FX, books, dimensions, units) | TODO | | |
| ORG-9 | Master imports | TODO | | |
| REF-1 | REF-01 jurisdiction registry commands | TODO | | |
| REF-2 | REF-07 accounting policy | TODO | | |
| REF-3 | REF-10 calendars + payment terms | TODO | | |
| JUR-1 | jurisdiction-decisions:resolve | TODO | | after ORG-5..7 |
| G1-1 | Group 1 register items now unblocked | TODO | | |
| EML-1 | 128 templates + 32 variants | TODO | | |
| EML-2 | Register parity test | TODO | | |
| EML-3 | Trigger routing | TODO | | |
| EML-4 | Event catalog registration | TODO | | after G-08 |
| EML-5 | Email platform controls | TODO | | |
| EML-6 | Email §13 backlog | TODO | | |
| NCD-1 | Notification matrix residuals | TODO | | |
| SEC-1 | Key custody | TODO | | |
| SEC-2 | Crypto agility | TODO | | |
| SEC-3 | SIEM forwarding + security operations | TODO | | |
| SEC-4 | Tokenization | TODO | | after SEC-1 |
| SEC-5 | carta-svc | TODO | | |
| SEC-6 | mtls-management-svc | TODO | | |
| SEC-7 | Effective security policy | TODO | | |
| SEC-8 | CI supply chain | TODO | | |
| SEC-9 | Backup / DR | TODO | | |
| FE-0 | Commit layout restyle | TODO | | |
| FE-1 | Console screens for new capabilities | TODO | | |
| FE-2 | Security & Trust panels | TODO | | |
| CLOSE-1 | Audit files re-scored | TODO | | |
| CLOSE-2 | Postman collections | TODO | | |
| CLOSE-3 | CI-equivalent run | TODO | | |

## Decisions taken during implementation

| Date | Task | Decision | Why |
|---|---|---|---|

## Cross-team follow-ups (changes needed in services we do not own)

| Service | Change needed | Needed by task |
|---|---|---|
| document-vault-svc | Consume `legal_hold.*` from retention-registry instead of owning holds | G-10 |
| obligation-tracking-svc | Overlaps obligations-svc (single owner C8) | WF-3 |
| external-data-feed-svc | Regulatory source feed for jurisdiction packs | JUR-1 |
| corporate-actions, filing-preparation, general-ledger, financial-close, vat-gst, corporate-tax | Forward the envelope and call evidence-requirements before governed actions | WF-4 |

## Dormant email triggers (no producer yet)

| Trigger | Template | Service that should produce it |
|---|---|---|

## Evidence blocks

<!-- One block per task, in the plan §7 format. Newest at the bottom. -->
