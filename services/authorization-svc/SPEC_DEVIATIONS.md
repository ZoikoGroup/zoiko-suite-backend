# Specification Deviations — authorization-svc

**Service:** authorization-svc  
**Date:** 2026-11-07  
**Tracker:** DOMAIN1-DOMAIN2-COMPLETION-TRACKER-2026-11-06.md (D2-5)

This document records deliberate deviations from the original spec (ZS-SVC-M-001 INT-01 §6–§15, IAM / AuthZ §18–§21) and the AUTH-PLANE plan, as required by AZ-5 (IAM-7).

---

## 1. Gate 1 dependency (Cross-version compatibility)

**Spec requirement (DoD gate 1):** Machine contract published and compatibility certified.

**Deviation:** OpenAPI (21 paths, 27 schemas) and AsyncAPI (6 messages) are published, and contract tests assert them against the router and the event constants **in both directions**. However, **cross-version compatibility is not certified**. That requires GOV-08's schema registry, which nothing here publishes to. The contract is checked against *this* build, not against what consumers were compiled from.

**Status:** 🧭 Decision — tracked as D2-5.F23 in the tracker. Platform team to provision schema registry.

**Reference:** DoD gate 1; tracker D2-5.F23.

---

## 2. Gate 9 (End-to-end load profile)

**Spec requirement (DoD gate 9):** NFR/load/failure tests meet approved class.

**Deviation:** The evaluator's own per-call cost is measured and gated in CI at P99 ≤ target ms sequential and ≤ target ms under concurrency. **Not measured:** end-to-end latency against a running stack, which is the number the P99 target actually refers to. The CI test stubs out the registries, Postgres, Redis and Kafka. The load profile to run against a live stack is in `RUNBOOK.md` §13 and has not been executed.

**Status:** 🧭 Decision — tracked as D2-5.F24 in the tracker. Requires full compose stack.

**Reference:** DoD gate 9; tracker D2-5.F24; RUNBOOK.md §13.

---

## 3. Gate 11 (Alert rules exercised)

**Spec requirement (DoD gate 11):** Operational dashboards/runbooks exercised.

**Deviation:** Dashboard, 8 alert rules and `RUNBOOK.md` exist, and every metric they reference is emitted. **"Exercised" means run against a live system during an incident or a game day.** That has not happened. The alert expressions are unproven against real data.

**Status:** 🧭 Decision — tracked as D2-5.F25 in the tracker. Requires full compose stack and a game day.

**Reference:** DoD gate 11; tracker D2-5.F25; RUNBOOK.md.

---

## 4. SoD jurisdiction check live (jurisdiction-rules-svc)

**Spec requirement (spec §9):** Separation of Duties rules scoped to a jurisdiction require the jurisdiction-rules-svc to resolve the jurisdiction's active flag.

**Deviation:** The SoD jurisdiction check returns 503 on local stack when jurisdiction-rules-svc is not running. Gate 4 is met in code, not exercised. `JURISDICTION_SERVICE_URL` is required in staging/production; the service refuses to start without it.

**Status:** 🧭 Decision — tracked as D2-5.F26 in the tracker. Requires jurisdiction-rules-svc running.

**Reference:** Spec §9; tracker D2-5.F26; RUNBOOK.md §5.1.

---

## 5. Production mTLS enforcement (AUTHZ_EVALUATION_MTLS=require)

**Spec requirement (IAM / AuthZ §13):** mTLS enforced on evaluation endpoints when `AUTHZ_EVALUATION_MTLS=require`.

**Deviation:** The flag is implemented and tested, but `AUTHZ_EVALUATION_MTLS=require` not exercised in local stack (requires mtls-management-svc enrolment). The service works without mTLS in development/staging (flag defaults to `off`).

**Status:** ⏳ Pending — tracked as D2-5.F27 in the tracker. Requires mtls-management-svc enrolment.

**Reference:** IAM / AuthZ §13; tracker D2-5.F27; RUNBOOK.md §11.

---

## 6. Identity assertion enforcement (AUTHZ_IDENTITY_ASSERTION=require)

**Spec requirement (IAM / AuthZ §12):** Admin routes require a verified identity envelope with `mfa_verified` and recent `auth_time`.

**Deviation:** The middleware is implemented and tested, but `AUTHZ_IDENTITY_ASSERTION=require` not exercised in local stack (requires identity-context-svc with `IDP_CLIENT_AUTH=required`). The service works without it in development (flag defaults to `off`).

**Status:** ⏳ Pending — tracked as D2-5.F28 in the tracker. Requires identity-context-svc with client auth.

**Reference:** IAM / AuthZ §12; tracker D2-5.F28; RUNBOOK.md §2.

---

## 7. IAM-8 SLOs (Load and revocation)

**Spec requirement (IAM-8 / SLO-1, SLO-2):** Load SLO (P99 ≤ target ms) and revocation SLO (≤ 30s).

**Deviation:** The SLO definitions exist but have not been measured end-to-end. The load profile in `RUNBOOK.md` §13 measures them but has not been run. Revocation SLO requires the full chain: identity-context-svc revocation → authorization-svc cache invalidation → next evaluate = DENY.

**Status:** 🧭 Decision — tracked as D2-5.F29 in the tracker. Shared with identity-context-svc; requires full stack.

**Reference:** IAM-8; tracker D2-5.F29; RUNBOOK.md §13.

---

## 8. Federation and SCIM (INT-10, AU-07/AU-08)

**Spec requirement:** Federation and SCIM routes available when `FEDERATION_ENABLED=true`.

**Deviation:** Off unless the operator turns it on. With `FEDERATION_ENABLED=false` (the default) none of the `/v1/federation/*` or `/scim/v2/*` routes are mounted (404), and the console's Federation tab says so. This is a deliberate default, not a gap.

**Status:** Documented — tracked as D2-5.F30 in the tracker.

**Reference:** RUNBOOK.md §15; Spec INT-10.

---

## 9. IAM-11 aliases cross-service rollout

**Spec requirement (IAM-11):** Canonical grant satisfies legacy check, legacy grant satisfies canonical check.

**Deviation:** The `alias_for` column is added to `permission_definitions` and the Lookup resolves canonical↔legacy. However, the rollout mode is controlled by `IDENTITY_ACCEPT_LEGACY_AUDIENCE=true` kept in compose/k8s for all services. Strict mode (audience validation enforced) is blocked on S5-B2 (console routing). The access-control permission taxonomy is registered and the aliases are populated in migration 000048.

**Status:** 🟡 Partial — tracked as D2-5.F31 in the tracker. Cross-team coordination needed.

**Reference:** IAM-11; tracker D2-5.F31; migration 000048.

---

## 10. Commercial-account-svc live dependency

**Spec requirement:** `X-Commercial-Plan` / `X-Org-Unit-Id` edge-set headers require commercial-account-svc to be running.

**Deviation:** The gateway's commercial client is implemented and tested (unit tests pass), but the live half is blocked because commercial-account-svc does not run locally. Compose does not include it in the default stack.

**Status:** 🟡 Partial — tracked as D2-5.F32 in the tracker.

**Reference:** AUTH-PLANE plan GW-3, S3-D1; tracker D2-5.F32.

---

## 11. Document upload in E2E (document-vault-svc)

**Spec requirement:** Document uploads in E2E tests require document-vault-svc.

**Deviation:** Document upload in E2E is skipped because document-vault-svc needs clamd in compose. The store and handler suites cover the functionality.

**Status:** ⛔ Blocked — tracked as D2-5.F33 in the tracker. Requires clamd in compose.

**Reference:** ER-W4; tracker D2-5.F33.

---

## 12. Caller gates (cross-team)

**Spec requirement:** Caller services must enforce their own gates (GL, financial-close, vat-gst, corporate-tax).

**Deviation:** These are cross-team dependencies. Authorization-svc provides the evaluation engine; the callers must wire it up. Not a gap in this service.

**Status:** 🟡 Partial — tracked as D2-5.F34 in the tracker. Cross-team.

**Reference:** AUTH-PLANE plan caller gates; tracker D2-5.F34.

---

## 13. Full envelope from callers (cross-team)

**Spec requirement:** Callers must present the full identity envelope (not just principal_id).

**Deviation:** Cross-team dependency. Authorization-svc accepts the envelope; the callers must send it. Not a gap in this service.

**Status:** 🟡 Partial — tracked as D2-5.F35 in the tracker. Cross-team.

**Reference:** AUTH-PLANE plan envelope; tracker D2-5.F35.

---

## 14. Production mTLS to authorization-svc

**Spec requirement:** Production mTLS from callers to authorization-svc.

**Deviation:** `AUTHZ_MTLS_ENABLED=true` not exercised in local stack. Requires mtls-management-svc enrolment and Traefik serversTransport configuration.

**Status:** ⏳ Pending — tracked as D2-5.F36 in the tracker. Infrastructure blocked.

**Reference:** M-01 / S4-B1; tracker D2-5.F36.

---

## Summary

| Deviation | Spec Ref | Tracker | Status |
|---|---|---|---|
| Gate 1 cross-version compat | DoD gate 1 | D2-5.F23 | 🧭 Decision |
| Gate 9 end-to-end load | DoD gate 9 | D2-5.F24 | 🧭 Decision |
| Gate 11 alerts exercised | DoD gate 11 | D2-5.F25 | 🧭 Decision |
| SoD jurisdiction live | Spec §9 | D2-5.F26 | 🧭 Decision |
| Production mTLS enforcement | IAM §13 | D2-5.F27 | ⏳ Pending |
| Identity assertion enforcement | IAM §12 | D2-5.F28 | ⏳ Pending |
| IAM-8 SLOs | IAM-8 | D2-5.F29 | 🧭 Decision |
| Federation/SCIM default off | INT-10 | D2-5.F30 | Documented |
| IAM-11 aliases rollout | IAM-11 | D2-5.F31 | 🟡 Partial |
| Commercial-account-svc live | GW-3, S3-D1 | D2-5.F32 | 🟡 Partial |
| Document upload in E2E | ER-W4 | D2-5.F33 | ⛔ Blocked |
| Caller gates | caller gates | D2-5.F34 | 🟡 Partial |
| Full envelope from callers | envelope | D2-5.F35 | 🟡 Partial |
| Production mTLS to authz | M-01, S4-B1 | D2-5.F36 | ⏳ Pending |

---

**Next review:** After AZ-5 completes (live audit, RELEASE_CERTIFICATE re-certification).