# Specification Deviations — authorization-svc

**Service:** authorization-svc (IAM / AuthZ)
**Date:** 2026-11-10 (replaces the 7 Nov version)
**Specs:** ZS-IAM-001 (Global Authorization, RBAC/ABAC & SoD Standard), Governance
Control Plane GOV-03 / GOV-04 / GOV-12, Doc 03 §8.3, ORG-06 (role half)
**Tracker:** DOMAIN1-DOMAIN2-COMPLETION-TRACKER-2026-11-06.md, D2-5

The 7 Nov version described a different service in places: 21 paths and 27
schemas (there are 86 paths, 101 operations), a `/v1/bindings` surface, migration 000048 and
`alias_for` (those are access-control-svc's), and federation routes (those are
identity-context-svc's and access-control-svc's). This version lists only what
differs from the specs in this service's code today.

---

## Deliberate deviations

| # | Spec | What the spec says | What this service does | Why / status |
|---|---|---|---|---|
| 1 | ZS-IAM-001 §21 | `POST /v1/iam/access-assignments`, `…:revoke`, `GET /v1/iam/access-reviews` | Served by **access-control-svc** (governed request workflow + access reviews), which provisions into this service's `/v1/admin/role-assignments`. | Ownership decided 13 Oct (N-03/N-10). Not a gap. |
| 2 | ZS-IAM-001 §21 | `POST /v1/iam/delegations`, `…/{id}:revoke` | Served here since 10 Nov as aliases of `/v1/admin/delegated-authorities` (same handler; colon and slash forms). | Closed (D2-5.F19 delegation half). |
| 3 | ZS-IAM-001 §17 / §29 | Federation, SSO, SCIM | Not in this service: identity-context-svc (SCIM, federation) and access-control-svc (role eligibility). | Ownership. |
| 4 | ZS-IAM-001 §12, §27 A09 | Deterministic FX basis for limit comparisons | Reference-rate table, operator-set with `AUTHZ_FX_REFERENCE_RATES` (default GBP-based table); each converted decision records `FX_BASIS:REFERENCE_RATES:<from>/<to>` and `FX_RATE:<r>`. No live FX source is wired. | §31 lists the authoritative FX basis as a pre-production decision. |
| 5 | ZS-IAM-001 §10.2, §12 | Quorum, cooling window, related party, fact-hash revalidation, resource state | Judged from attributes the PEP supplies (`approved_by`, `required_approvals`, `supplier_bank_changed_by`, `approved_fact_hash`, `status`, …), not from approval or resource records held here. Fails closed where a fact is half-supplied (approved hash without current hash; amount without currency against a limit). | This service holds no workflow or business-object state; the owning services do. Callers must send the facts. |
| 6 | API-4 | Cursor paging on every list | Opt-in (`page_size` / `cursor`) on all 15 admin lists; role assignments page in SQL (keyset), the other registers in memory over the store's tenant read (capped at 500 rows, flagged `truncated`). `/v1/access-decisions` pages in SQL. | Registers are tenant-bounded; unpaged callers keep their array. |
| 7 | ZS-IAM-001 §19 | Short TTL, version-aware cache | 5 s TTL plus a database watermark in every key (1 s refresh, bypass after 3 s). | Met; noted because the TTL alone is not the staleness bound. |

## Enforcement switches held lax in local compose

Each was proven in strict mode on the local stack on 10 Nov 2026 and is the
code default outside `ENV=local`; compose keeps the lax value so callers that
have not migrated keep working.

| Switch | Compose | Default outside local | Waiting on |
|---|---|---|---|
| `AUTHZ_IDENTITY_ASSERTION` | `warn` | `require` | every console/admin caller forwarding the envelope (F-1) |
| `AUTHZ_EVALUATION_MTLS` | `permit` | `require` | every PEP holding a client certificate (M-02) |
| `AUTHZ_STEP_UP_UNKNOWN_AGE` | `permit` | `step_up` | PEPs forwarding `X-Authn-Age-Seconds` (X-3) |
| `AUTHZ_ENFORCE_TENANT_ON_AUTHORIZE` | `false` | `false` | tenantless callers sending `X-Tenant-Id` (X-1) |

## Not verified (verification, not implementation)

| Item | State |
|---|---|
| Cross-version contract compatibility | Needs GOV-08's schema registry; the contracts are checked against this build only (`TestOpenAPI_MatchesTheRouter`, `internal/events/contract_test.go`). |
| Alert rules exercised in an incident / game day | Rules are valid (`promtool`) and every metric they read is emitted; none has fired against production data. |
| Production SLO | Load and revocation were measured on the local single-replica stack (RUNBOOK §11), not a production class. |
| Break-glass / JIT approval with real TOTP step-up | Covered by unit tests and the 14 Oct live run; the 10 Nov persona run proves only the refusals (no step-up, over-long). |
| siem-integration-svc and Jaeger edges | Not started in the 10 Nov run (Docker VM memory); `e2e-authorization-deps.py` 28/32 with those four checks unrun. |

## Cross-team items this service cannot close

- Callers' own gates (GL, financial close, VAT, corporate tax) and full envelopes
  from callers (D2-5.F34/F35).
- access-control-svc's permission aliases (its 000048): the 7 Nov file could not
  apply on any database; rewritten 10 Nov to apply idempotently, and its
  `PermissionCatalogue.Lookup` alias read has its own defect (the canonical
  lookup's query has no table alias, so it never resolves) — recorded for
  access-control-svc.
