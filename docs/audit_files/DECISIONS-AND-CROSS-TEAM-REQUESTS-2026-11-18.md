# Decisions and Cross-Team Requests — 18 Nov 2026

**Date:** 2026-11-18  
**Session:** D2-5 (authorization-svc) gap closure  
**Status:** Drafted, ready to send (Gmail/Slack not connected; user sends)

---

## DS-1…9 — Decision Requests (One message per owner group)

### Architecture (DS-1, DS-2, DS-5, DS-9)

**To:** Architecture team  
**Subject:** DS-1, DS-2, DS-5, DS-9 — Platform governance decisions needed

| Item | Question | Options | Recommendation | Already Built | Evidence | Need Back | By When |
|------|----------|---------|----------------|---------------|----------|-----------|---------|
| **DS-1** | Should the platform enforce `AUTHZ_IDENTITY_ASSERTION=require` as the default outside ENV=local? | 1. Keep `warn` as default<br>2. Flip to `require` | **Option 2** — All internal callers now present envelopes or mTLS delegate certs | access-control-svc on mTLS (CT-3), seed-demo-principals with tooling accounts, e2e scripts with identity envelopes, TOTP factors for step-up | e2e-authorization-scenarios.py 78/78 pass under require; e2e-identity-context-deps.py 61/61 pass; audit.sh 49/0 | Confirmation to flip compose default to `${AUTHZ_IDENTITY_ASSERTION:-require}` | 2026-11-25 |
| **DS-2** | Should FX reference rates remain operator-set via `AUTHZ_FX_REFERENCE_RATES`, or wire REF-03 live rate sets? | 1. Keep operator-set table<br>2. Wire REF-03 when ORG-8 lands | **Option 1 for now** — Table is complete, tested, recorded in SPEC_DEVIATIONS #4; REF-03 is EST-8 (IAM-5) | Reference-rate table in config, FX_BASIS/FX_RATE recorded on every converted decision | `services/authorization-svc/internal/abac/evaluator.go` FX conversion; `e2e-authorization-scenarios.py` limit tests with USD→GBP | Decision on REF-03 timeline | 2026-12-15 |
| **DS-5** | Should `AUTHZ_EVALUATION_MTLS` default to `require` outside local? | 1. Stay `permit`<br>2. Flip to `require` | **Option 2** — Every PEP will hold a client cert (M-02) | mTLS infrastructure complete; mtls-management-svc operational; PEPs: access-control, evidence-manifest, decision-support on mTLS | docker-compose.yml: all services have MTLS_BOOTSTRAP_TOKEN; e2e-authorization-deps.py mtls check passes | Confirm all PEPs migrated | 2026-11-25 |
| **DS-9** | Should governance-decision-log-svc reads use purpose header only (remove P-07 header on ledger appends)? | 1. Keep both<br>2. Purpose only on reads | **Option 2** — D-06 decision | policy-svc SPEC_DEVIATIONS P-07 recorded; governance-decision-log reads over mTLS with purpose | decision-support-svc reads with purpose=evidence_manifest_assembly; policy-svc ledger appends still send purpose | Cross-team CT-6 with policy-svc | 2026-12-01 |

---

### Product / Architecture (DS-3)

**To:** Product + Architecture  
**Subject:** DS-3 — Quorum and approval semantics

| Item | Question | Options | Recommendation | Already Built | Evidence | Need Back | By When |
|------|----------|---------|----------------|---------------|----------|-----------|---------|
| **DS-3** | Should quorum require independent approvals from distinct principals, and should the preparer be excluded? | 1. Current: distinct principals, preparer excluded<br>2. Relax preparer exclusion | **Option 1** — Current behaviour is spec-compliant (ZS-IAM-001 §10.2) | Quorum check in authorization-svc evaluator; e2e tests verify preparer excluded | `e2e-authorization-scenarios.py` quorum tests: "one independent approval of two" DENY, "two independent approvals" PERMIT | Confirmation that preparer exclusion is the intended policy | 2026-11-22 |

---

### Tax Engineering (DS-2, DS-4)

**To:** Tax Engineering  
**Subject:** DS-2 (FX basis), DS-4 — Tax authority interface requirements

| Item | Question | Options | Recommendation | Already Built | Evidence | Need Back | By When |
|------|----------|---------|----------------|---------------|----------|-----------|---------|
| **DS-2** | (Same as Architecture DS-2) Should FX reference rates remain operator-set? | See Architecture DS-2 | | | | | |
| **DS-4** | What FX basis should tax-authority-interface-svc use for filed amounts? | 1. Use authorization-svc's reference table<br>2. Own rate source | **Option 1** — Single source of truth | authorization-svc FX table exists; tax-authority-interface-svc can read via config | `AUTHZ_FX_REFERENCE_RATES` env var; evaluator records FX_BASIS on every conversion | Confirm shared FX basis acceptable | 2026-12-01 |

---

### Security (DS-6)

**To:** Security team  
**Subject:** DS-6 — TOTP as interim high-assurance factor

| Item | Question | Options | Recommendation | Already Built | Evidence | Need Back | By When |
|------|----------|---------|----------------|---------------|----------|-----------|---------|
| **DS-6** | Approve TOTP (RFC 6238 SHA1/6/30) as the interim step-up factor pending WebAuthn; approve passkey attestation `none` with no authenticator allow-list | 1. Approve TOTP + passkey `none`<br>2. Require WebAuthn first | **Option 1** — TOTP is built, seeded, tested; WebAuthn is AU-11 (future) | TOTP in identity-context-svc; fixed dev secrets for admin@ (333333) and legal.counsel@ (666666); break-glass/JIT tests pass with real TOTP | `e2e-authorization-scenarios.py` break_glass() and JIT with totp_code(); authorization.spec.ts break-glass with TOTP; policies.spec.ts emergency suspension with break-glass session | Formal approval recorded in SPEC_DEVIATIONS (S1-D2 / SO-1) | 2026-11-22 |

---

### Finance (DS-7)

**To:** Finance  
**Subject:** DS-7 — Authoritative FX basis for limit comparisons (A09)

| Item | Question | Options | Recommendation | Already Built | Evidence | Need Back | By When |
|------|----------|---------|----------------|---------------|----------|-----------|---------|
| **DS-7** | Keep the operator-set reference-rate table locally in authorization-svc; wire REF-03 live rate sets when ORG-8 lands (EST-8 IAM-5) | 1. Keep local table<br>2. Wire REF-03 now | **Option 1** — Local table is complete, deterministic, auditable | `AUTHZ_FX_REFERENCE_RATES` parsed at startup; every converted decision records FX_BASIS and FX_RATE | `e2e-authorization-scenarios.py` limit tests with USD→GBP conversion; SPEC_DEVIATIONS #4 | Confirm local table sufficient for pre-production; REF-03 timeline | 2026-12-15 |

---

### Business Owner (DS-8)

**To:** Business Owner  
**Subject:** DS-8 — Break-glass / emergency suspension governance

| Item | Question | Options | Recommendation | Already Built | Evidence | Need Back | By When |
|------|----------|---------|----------------|---------------|----------|-----------|---------|
| **DS-8** | Confirm break-glass: 1 approver (default), max 2 h session, TOTP step-up for both requestor and approver; emergency suspension of accounting policy requires requestor's own live break-glass session | 1. Confirm current design<br>2. Change quorum/duration/step-up | **Option 1** — Current: `AUTHZ_BREAK_GLASS_APPROVALS=1`, `AUTHZ_MAX_BREAK_GLASS_SECONDS=7200`, TOTP enforced | Break-glass flow complete: request → TOTP → approve (TOTP) → Active; emergency suspension gated by break-glass session ID | `e2e-authorization-scenarios.py` break_glass(); `authorization.spec.ts` break-glass with TOTP; `policies.spec.ts` emergency suspension with break-glass session | Business sign-off on ACTIVE tie-break (decision recorded in tracker D2-3) | 2026-11-22 |

---

### DS-9 — Governed authority-source records (V-001 PDC-01)

**To:** Architecture  
**Subject:** DS-9 — Governed authority-source records

| Item | Question | Options | Recommendation | Already Built | Evidence | Need Back | By When |
|------|----------|---------|----------------|---------------|----------|-----------|---------|
| **DS-9** | Keep the GOV-05 model (version = package) or build PDC-01 aggregates? | 1. Keep GOV-05<br>2. Build PDC-01 | **Option 1** — GOV-05 model is working; PDC-01 is roadmap | policy-svc GOV-05 complete; accounting policies REF-07 complete | `policies.spec.ts` accounting policy lifecycle; SPEC_DEVIATIONS | Decision when a service owns authority sources | 2026-12-15 |

---

## CT-1, CT-2, CT-4, CT-5 — Cross-Team Requests (One message per team)

### CT-1 — policy-svc ↔ governance-decision-log-svc (Purpose header D-06)

**To:** policy-svc team  
**Subject:** CT-1 — D-06: Purpose header on reads only

| Field | Value |
|-------|-------|
| **What** | Remove P-07's `X-Purpose-Context` header on governance-decision-log-svc ledger appends; keep purpose on reads only |
| **Why** | Ledger appends are the service's own write (GOVERNANCE_DECISION_RECORD); the purpose is implied by the action. Reads need purpose for audit (D-04, D-06). |
| **Evidence** | decision-support-svc reads with `purpose=evidence_manifest_assembly`; policy-svc ledger appends still send purpose. SPEC_DEVIATIONS P-07 recorded. |
| **How to Verify** | `e2e-governance-decision-log-deps.py` reads work with purpose; ledger appends without purpose return 200. |
| **Status** | **drafted 18 Nov, ready to send** |

---

### CT-2 — identity-context-svc ↔ gateway-auth-svc (mTLS JWKS)

**To:** gateway-auth-svc team  
**Subject:** CT-2 — mTLS for JWKS and tenant registry

| Field | Value |
|-------|-------|
| **What** | gateway-auth-svc calls identity-context-svc JWKS and tenant-entity-registry-svc over mTLS (already in compose: `IDENTITY_JWKS_MTLS_ENABLED=true`, `TENANT_REGISTRY_MTLS_ENABLED=true`) |
| **Why** | SEC-INV-05: mTLS on material paths. Both peers serve :8449 with RequireAndVerifyClientCert. |
| **Evidence** | docker-compose.yml: gateway-auth-svc has MTLS_MANAGEMENT_SERVICE_URL, MTLS_BOOTSTRAP_TOKEN_PATH; e2e-gateway-auth-deps.py passes. |
| **How to Verify** | `e2e-gateway-auth-deps.py` check 10 (Authorization-svc age header reaches step-up) exercises the mTLS path. |
| **Status** | **drafted 18 Nov, ready to send** |

---

### CT-4 — access-control-svc ↔ authorization-svc (mTLS delegate CT-3)

**To:** access-control-svc team (internal)  
**Subject:** CT-4 — access-control-svc as registered delegate (CT-3 complete)

| Field | Value |
|-------|-------|
| **What** | access-control-svc now calls authorization-svc admin API over mTLS as registered delegate (AUTHZ_ADMIN_DELEGATES includes access-control-svc). Uses shared `identity-envelope/mtlsclient`. |
| **Why** | Required for `AUTHZ_IDENTITY_ASSERTION=require` (AZ-2). Without mTLS, access-control-svc's plaintext admin calls were refused 503. |
| **Evidence** | access-control-svc main.go uses mtlsclient.Start(); compose has AUTHZ_MTLS_ENABLED=true, MTLS_BOOTSTRAP_TOKEN_PATH, volume subpath, depends_on mtls-management-svc + keygen. |
| **How to Verify** | `e2e-authorization-scenarios.py` role create → provisioned in authz (log shows delegate/envelope basis). |
| **Status** | **drafted 18 Nov, ready to send** |

---

### CT-5 — evidence-manifest-svc ↔ authorization-svc (access-decision reads)

**To:** evidence-manifest-svc team (internal)  
**Subject:** CT-5 — AccessDecisionClient forwards Authorization bearer

| Field | Value |
|-------|-------|
| **What** | evidence-manifest-svc's AccessDecisionClient now forwards the caller's Authorization bearer when it names the forwarded principal (same rule as access-control-svc's ForwardBearer). For relay/scheduled paths, uses mTLS identity as delegate. |
| **Why** | authorization-svc's GET /v1/access-decisions/{id} requires verified envelope. Forwarding the signed-in user's envelope is the stronger proof. |
| **Evidence** | aggregator/forward.go + clients.go: NewAccessDecisionClientWithHTTPClient with forwarding transport. Unit test for header. |
| **How to Verify** | `e2e-evidence-requirements-deps.py` manifest with an access decision; `e2e-authorization-scenarios.py` decision_log() reads work. |
| **Status** | **drafted 18 Nov, ready to send** |

---

### CT-3 — access-control-svc mTLS delegate (Done)

**Status:** **done (ours)** — Commit: access-control-svc on mTLS as registered delegate (CT-3)

### CT-7 — Tooling principals + TOTP (Done)

**Status:** **done (ours)** — Commit: identity accounts for tooling principals + demo TOTP factors

### CT-6 — governance-decision-log-svc purpose header (Open)

**Status:** **open decision (ours)** — D-06: purpose header on reads only. Cross-team with policy-svc. See CT-1.

---

## Tracker Updates

### Part F2 — Decision Requests (DS-1…9)
| Item | Status |
|------|--------|
| DS-1 | drafted 18 Nov, ready to send |
| DS-2 | drafted 18 Nov, ready to send |
| DS-3 | drafted 18 Nov, ready to send |
| DS-4 | drafted 18 Nov, ready to send |
| DS-5 | drafted 18 Nov, ready to send |
| DS-6 | drafted 18 Nov, ready to send |
| DS-7 | drafted 18 Nov, ready to send |
| DS-8 | drafted 18 Nov, ready to send |
| DS-9 | drafted 18 Nov, ready to send |

### Part F3 — Cross-Team Requests (CT-1…7)
| Item | Status |
|------|--------|
| CT-1 | drafted 18 Nov, ready to send |
| CT-2 | drafted 18 Nov, ready to send |
| CT-3 | **done (ours)** — with commit |
| CT-4 | drafted 18 Nov, ready to send |
| CT-5 | drafted 18 Nov, ready to send |
| CT-6 | **open decision (ours)** |
| CT-7 | **done (ours)** — with commit |

---

## Verification Summary (All under require)

- **Go suites:** access-control-svc ✅, evidence-manifest-svc ✅, identity-context-svc (seeder build) ✅, authorization-svc identity pkg ✅
- **Live E2E:** e2e-identity-context-deps.py 61+ ✅, e2e-authorization-deps.py 33 ✅, e2e-authorization-scenarios.py 78 + break-glass/JIT ✅, seed-demo-rbac.ps1 run twice (idempotent) ✅, access-control role create from console → provisioned in authz ✅, evidence-manifest manifest with access decision ✅, audit.sh 49/0 ✅, Playwright authorization.spec.ts + policies.spec.ts ✅
- **Containers:** postgres kafka redis → mtls → identity tenant → access-control authorization (+ policy, workflow, evidence-manifest, GDL only for their checks)

---

*Generated 2026-11-18. Nothing is sent (Gmail/Slack not connected); the user sends them.*