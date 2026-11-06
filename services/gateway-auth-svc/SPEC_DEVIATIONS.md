# Specification Deviations — gateway-auth-svc

**Service:** gateway-auth-svc  
**Date:** 2026-11-06  
**Tracker:** DOMAIN1-DOMAIN2-COMPLETION-TRACKER-2026-11-06.md (D1-5)

This document records deliberate deviations from the original spec (ZS-SVC-M-001 INT-01 §4.1, Security §8, GOV-01 ingress, AU-01/04/12/23/27) and the AUTH-PLANE plan, as required by GW-6b (G-3).

---

## 1. S5-D1 placement (Production internet-edge controls)

**Spec requirement:** DDoS, WAF, bot/abuse, TLS policy, request-size limits at the internet edge.

**Deviation:** These controls are not implemented in gateway-auth-svc. They belong at the GCP Load Balancer / Cloud Armor layer (platform infrastructure). gateway-auth-svc enforces request-size limits (admission Policy) and rate limiting (INT-01), but the full S5-D1 stack (WAF, bot detection, DDoS) is a platform decision.

**Status:** 🧭 Decision — tracked as D1-5.F16 in the tracker. Platform team to provision Cloud Armor in front of the GCP LB.

**Reference:** AUTH-PLANE plan S5-D1; tracker D1-5.F16.

---

## 2. AU-27 (Sender-constrained tokens / DPoP / mTLS binding)

**Spec requirement:** Sender-constrained envelopes per client class (AU-27).

**Deviation:** Not implemented. The gateway accepts standard Bearer tokens (and DPoP proofs via identity-envelope's DPoPVerifier when presented), but does not enforce sender-constrained tokens as a mandatory requirement. The interface is documented but enforcement is deferred to a Security decision (D-7).

**Status:** 🧭 Decision — tracked as D1-5.F20 in the tracker.

**Reference:** Security Standard AU-27; tracker D1-5.F20.

---

## 3. S5-B2 (Console → service calls bypass ForwardAuth)

**Spec requirement:** Console calls should go through the edge (ForwardAuth) or every service verifies the envelope.

**Deviation:** The console (`app/admin/*`) calls services directly with the `zoiko-internal` audience envelope, bypassing the gateway's ForwardAuth. This is a known migration state controlled by `IDENTITY_ACCEPT_LEGACY_AUDIENCE=true` in compose. Per-service audience exchange (GW-5 / AU-12) is implemented at the gateway for non-console traffic. The console routing remains a frontend follow-up (X5 cross-cutting).

**Status:** 🟡 Partial — GW-5 implements the exchange for the gateway's outbound calls and for `/verify` when `GATEWAY_AUDIENCE_ROUTES` is configured. Console bypass is documented and tracked as X5.

**Reference:** AUTH-PLANE plan S5-B2; tracker D1-5.F17, GW-5.

---

## 4. GW-FE deviation (Console diagnostic page does not read admin surface)

**Spec requirement (original GW-FE-1):** Console reads the per-replica admin API (decisions, usage, rate-limit state, route versions).

**Deviation:** The admin surface (GW-4) is deliberately NOT read by the console. The admin surface exposes in-memory per-replica state (decisions, usage, limiter state) which varies by replica behind a load balancer. A console page load would show a different replica's state each time, and a decision-by-id lookup would miss when landing on another replica. Fleet traffic belongs in Grafana (`gateway_*` metrics) and denials in SIEM. The console page (`/admin/gateway`) is a diagnostic page showing only fleet-true or per-viewer data: probes, the identity JWKS, the viewer's session, and the denial-reason reference.

**Status:** Documented deviation — GW-FE-1 removed, GW-FE-2 revised (2026-11-04). See tracker "Decisions taken" section.

**Reference:** AUTH-PLANE plan GW-FE-1/GW-FE-2; tracker D1-5 Decisions taken.

---

## 5. M-01 / S4-B1 (Per-service mTLS enrolment)

**Spec requirement:** Per-service mTLS enrolment and Traefik `serversTransport` client certificates for edge → service hop.

**Deviation:** Not implemented. Requires mtls-management-svc enrolment (SEC-6) and Traefik infrastructure. The gateway provisions its own client identity at boot (via mtls-management-svc) for upstream mTLS calls (JWKS, tenant registry), but the edge → service hop mTLS is blocked on infrastructure.

**Status:** ⛔ Blocked — tracked as D1-5.F18 in the tracker. Platform decision.

**Reference:** AUTH-PLANE plan M-01, S4-B1; tracker D1-5.F18.

---

## 6. Commercial-account-svc live dependency (GW-3 / S3-D1)

**Spec requirement:** `X-Commercial-Plan` / `X-Org-Unit-Id` edge-set headers require commercial-account-svc to be running.

**Deviation:** The gateway's commercial client is implemented and tested (unit tests pass), but the live half is blocked because commercial-account-svc does not run locally. Compose does not include it in the default stack.

**Status:** 🟡 Partial — unit tests `TestClient_*` pass; live E2E (GW-8) requires commercial-account-svc.

**Reference:** AUTH-PLANE plan GW-3, S3-D1; tracker GW-8, D1-5.F10.

---

## 7. Residency gate fail-closed on unknown policy (GW-1)

**Spec requirement (INT-01 M7):** "fail closed on unknown (write)".

**Deviation:** The residency gate fails closed on unknown policy OR registry outage for material writes. Reads are never blocked. This matches the INT-01 M7 spec text but is stricter than "unknown policy" alone — it also fails closed on registry unavailability for writes. This is deliberate: a write must not proceed on an unconfirmed context.

**Status:** Implemented as designed — see tracker "Decisions taken: Residency gate".

**Reference:** INT-01 M7; tracker GW-1.

---

## 8. CARTA fail-closed on writes, open on reads (S5-D2 / AU-23)

**Spec requirement:** CARTA STEP_UP_MFA → 401 step_up_required; unreachable → closed writes / open reads.

**Deviation:** Implemented as specified. When CARTA is configured but unreachable, material writes get 503 (fail closed), reads proceed unscored (fail open). This is the AU-23 decision (21 Oct 2026).

**Status:** Implemented — tracker D1-5c.

**Reference:** AU-23; tracker D1-5c.

---

## 9. Trusted proxy CIDRs default to RFC 1918 (GW-2 / S5-D3)

**Spec requirement:** `GATEWAY_TRUSTED_PROXY_CIDRS`, `_DEPTH` configuration for client IP extraction behind GCP LB.

**Deviation:** Defaults to RFC 1918 private ranges (10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16). Production CIDRs are a platform decision (GW-7). Depth defaults to 1 (immediate peer).

**Status:** Implemented — tracker GW-2, D1-5.F08.

**Reference:** AUTH-PLANE plan GW-2, S5-D3; tracker Decisions taken.

---

## 10. Login rate limit separate bucket (GW-2 / AU-04)

**Spec requirement:** Source-level login throttling; 25 failed logins → 429 before the 25th.

**Deviation:** Implemented as a separate `LoginLimiter` bucket keyed by trusted client IP, distinct from the general IP limiter. Default 5 req/s, burst 10. Applies to `/v1/authenticate`, `/v1/authenticate/step-up`, `/v1/account/*`, `/oauth2/login*`, `/oauth2/token`.

**Status:** Implemented — tracker GW-2, D1-5.F09.

**Reference:** AU-04; tracker GW-2.

---

## 11. Admin token sourced from Docker secret file (GW-4)

**Spec requirement:** Admin read surface on `:8192` with token from file.

**Deviation:** The token is sourced from a Docker secret (volume `gateway_admin_token`), never from environment variables. The entrypoint script generates a ≥24-char token file in the volume if missing, `chown 65532`, `chmod 0400`. The console does not read this surface (see GW-FE deviation above).

**Status:** Implemented — tracker GW-4, Decisions taken.

**Reference:** AUTH-PLANE plan GW-4; tracker Decisions taken.

---

## 12. Audience exchange rollout mode (GW-5 / AU-12 / IAM-1)

**Spec requirement:** Per-service audience exchange at `/verify` with route→audience map.

**Deviation:** `IDENTITY_ACCEPT_LEGACY_AUDIENCE=true` kept in compose/k8s for all services. The exchange is proven in tests and one E2E probe; strict mode (audience validation enforced) is BLOCKED on S5-B2 (console routing). The gateway exchanges for its own outbound calls (carta-svc, commercial-account-svc) AND for `/verify` when `GATEWAY_AUDIENCE_ROUTES` is configured. Unknown protected route fails closed (503). Exchange failure → 503, never pass-through. Failures counted in `gateway_auth_exchange_failures_total{audience}`.

**Status:** Implemented — tracker GW-5, D1-5.F12.

**Reference:** AUTH-PLANE plan GW-5, IAM-1, AU-12; tracker G-1.

---

## 13. E2E dependency check script (GW-6a)

**Spec requirement:** Dependency E2E `deployments/scripts/e2e-gateway-auth-deps.py` on pattern of `e2e-authorization-deps.py`.

**Deviation:** The original script (pre-GW-6a) had `skip()` calls that counted as passes, wrong OAuth client, wrong commercial URL, and exit code issues. Rewritten as part of GW-6a (this session).

**Status:** Rewritten — tracker GW-6a.

**Reference:** AUTH-PLANE plan G-2; tracker GW-6a.

---

## 14. Contract and docs (GW-6b)

**Spec requirement:** SPEC_DEVIATIONS.md, re-certified RELEASE_CERTIFICATE.md, live `scripts/audit.sh`.

**Deviation:** This document (SPEC_DEVIATIONS.md) is created as part of GW-6b. RELEASE_CERTIFICATE.md will be re-certified after live audit. `scripts/audit.sh` will be run against the live stack.

**Status:** In progress — tracker GW-6b.

**Reference:** AUTH-PLANE plan G-3, G-4; tracker GW-6b.

---

## Summary

| Deviation | Spec Ref | Tracker | Status |
|-----------|----------|---------|--------|
| S5-D1 placement | S5-D1 | D1-5.F16 | 🧭 Decision |
| AU-27 sender-constrained | AU-27 | D1-5.F20 | 🧭 Decision |
| S5-B2 console bypass | S5-B2 | D1-5.F17, GW-5 | 🟡 Partial |
| GW-FE admin surface | GW-FE-1 | Decisions taken | Documented |
| M-01/S4-B1 mTLS edge | M-01, S4-B1 | D1-5.F18 | ⛔ Blocked |
| Commercial-account-svc live | GW-3, S3-D1 | GW-8 | 🟡 Partial |
| Residency gate fail-closed | INT-01 M7 | GW-1 | ✅ Implemented |
| CARTA fail-closed writes | S5-D2, AU-23 | D1-5c | ✅ Implemented |
| Trusted proxy defaults | GW-2, S5-D3 | GW-2 | ✅ Implemented |
| Login rate limit bucket | GW-2, AU-04 | GW-2 | ✅ Implemented |
| Admin token from file | GW-4 | GW-4 | ✅ Implemented |
| Audience exchange rollout | GW-5, IAM-1, AU-12 | GW-5, G-1 | ✅ Implemented |
| E2E script rewrite | G-2 | GW-6a | ✅ Rewritten |
| SPEC_DEVIATIONS + audit | G-3, G-4 | GW-6b | In progress |

---

**Next review:** After GW-6b completes (live audit, RELEASE_CERTIFICATE re-certification).