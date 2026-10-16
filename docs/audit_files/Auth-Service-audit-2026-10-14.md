# Auth service — full documentation and behaviour audit (14 October 2026)

**Scope.** The auth service as one plane:
- **identity-context-svc:** login, MFA, session envelope (GOV-01, Doc 03 §9.1);
- **gateway-auth-svc:** edge authentication (INT-01, Doc 05 §10.1);
- **authorization-svc:** decisions, IAM administration and sessions (GOV-03/04/12, Doc 03 §8.3);
- their wiring with access-control-svc and the callers.

**Baseline.** The code includes the uncommitted 14 Oct auth-plane pass (TOTP step-up, `auth_time`, envelope gate, session caps).

**Sources read for this audit:**
- ZS-IAM-001 (all 32 sections + appendices);
- Governance Control Plane GOV-01/03/04/12, §16–24;
- Doc 05 Security Architecture §3, §5, §6, §10, §11;
- ZS-SEC-001 §8, §9, §10, §11, §13, §16, §17;
- ZS-API-001 §5, §6, §12, §24–27, §30;
- ZS-EVENT-001 (IAM / security families, channels, producer authorisation);
- Doc 03 §8.3 and §9.1;
- ZoikoSuite Domain Ownership (7 Oct), risks R10, R22, R25, R26;
- `backend-completion-tracker.md` rows 79, 81, 82i, 84c.

**Method.**
1. Every requirement was checked against the code by reading it, not against earlier scores.
2. The main gaps were then proven on running binaries (identity-context, gateway-auth, authorization in `require` mode; scratch Postgres with NOBYPASSRLS roles).

Live evidence is marked **L**; code evidence **C**.

## Verdict

The 85 scored authorization rows pass, and the 14 Oct live E2E passed 41/41. **But the auth
service is not done end to end.**

A fresh reading of every source document finds:
- **12 High gaps.** Revocation, break-glass approval, authority-limit administration, login
  throttling, key management, the login protocol, federation, provisioning, the subject
  lifecycle, the PEP workload gate, the unattributed callers, and every other service still
  trusting `X-Principal-Id`.
- **15 Medium gaps** and **6 Low gaps.**
- **8 sign-off / decision items** and a **front-end track**.

The live behaviour probes matched the documents in **0 of 12** checks. The one apparent
match was a false positive (the input contract answered before routing); re-probed, it is a
gap.

Earlier scores measured what the 85 rows ask. They do not cover authentication protocol,
identity lifecycle, key management or the edge, which the documents also require.

## A. Gaps inside the three auth services

| ID | Sev | Gap | Doc | Evidence | To make it done |
|---|---|---|---|---|---|
| AU-01 | **High** | **Logout / revocation is not enforced.** After `POST /v1/context/session/{id}/invalidate` (204), the same envelope still opens authorization-svc admin routes (200) and passes gateway `/verify` (200) until it expires (≤ 300 s). Only identity-context's own MFA routes refuse it. | IAM §3 "Revocation … must not wait for cache/token expiry", A11; Doc 05 §5.3 | **L** | Revocation check on the envelope's `session_context_id` at the gateway and the authz gate: a short-TTL revocation set fed by `session.invalidated` / `principal.status.changed`, or an introspection call with a cache. Measure revocation lag (§26). |
| AU-02 | **High** | **Break-glass goes ACTIVE with no independent approval.** It is issued 201 ACTIVE with `approved_by` empty, and there is no Requested → Approved state. | GOV-12 (state model, ApproveBreakGlass, "requester cannot approve own"); Doc 05 §5.5 "require elevated approval"; GOV §1 "independently approved". IAM §14 asks only for notification, so the documents conflict. | **L** | Add the approval state and route (independent approver, own step-up), as JIT already has. Quorum by risk class is GOV §23's open decision. |
| AU-03 | **High** | **Approval and signing authority limits cannot be administered.** The limits are evaluated on every approval decision, but nothing creates or changes them: `CreateAuthorityLimit` has no caller and `POST /v1/admin/authority-limits` is 404. A limit can only be set by direct DB insert, and `iam.authority_limit.changed` can never fire. | IAM §12, §21–22, §23; §8 "no direct runtime mutation"; GOV anti-pattern 12 | **L C** | Governed command API (create / change / retire with maker-checker, reason and version) emitting `iam.authority_limit.changed`; access-control registry action for it. |
| AU-04 | **High** | **No source-level login throttling.** 25 failed logins against 25 accounts from one client: all 401, never 429. Per-account lockout bounds guessing on one account, not password spraying. The edge's global limit (1000 req/s) keys on the immediate peer (`depth: 0`), which behind a load balancer is the balancer. Identity routes are not behind the edge at all (S5-B2). | Doc 05 §10.1; ZS-SEC-001 §8; ZS-API-001 §27; IAM §26 | **L C** | Per-IP and per-tenant rate limits on `/v1/authenticate` and `/v1/authenticate/step-up`, keyed on the real client IP (trusted-proxy depth), plus 429 + `Retry-After`. Route identity through the edge. |
| AU-05 | **High** | **Signing and encryption keys are not managed.** The envelope signing key is a PEM file with a single `kid`, and the JWKS publishes one key, so there is no rotation without breaking live envelopes. The HS256 login-token secret, the MFA key and the client-secret file come from env / files, not KMS or vault. The code's own TODOs say so. | ZS-SEC-001 §10–13 (KMS / HSM, rotation, JIT secrets); Doc 05 §9 | **C** | KMS-backed signing (or an HSM key) with two published keys during rotation; secrets from secret-vault-integration-svc; a documented rotation runbook. |
| AU-06 | **High** | **The login is a password-for-token exchange, not an OIDC flow.** There is no authorization code + PKCE and no authorization-server metadata (`/.well-known/openid-configuration` 404). The confidential-client login added on 14 Oct (client_id + secret + username/password → token) is the resource-owner password grant shape, which §18 prohibits. | IAM §17–18 (code + PKCE; ROPC prohibited; AS metadata); ZS-API-001 §24 | **L C** | A real OIDC provider (first-party or external IdP) with hosted login, code + PKCE, AS metadata and refresh-token rotation. The console then logs in by redirect, and IC's `/v1/authenticate` becomes internal or is retired. **Decision:** build or buy the IdP. |
| AU-07 | **High** | **No enterprise federation.** There is no OIDC or SAML SSO (`SAMLAssertion` returns `ErrSAMLUnsupported`). | IAM §3, §17 (Wave 4); Doc 05 §5.2 | **C** | Federation through the IdP chosen in AU-06, with an issuer / audience / nonce / PKCE profile and controlled claim-to-subject mapping. |
| AU-08 | **High** | **No provisioning.** There is no SCIM 2.0, no just-in-time (JIT) user creation and no group-to-role mapping. Deprovisioning relies on manual status changes; scenarios A20 / A21 are not testable. | IAM §17 (Wave 4), A20, A21, §23 `identity.scim_sync.completed` | **C** | SCIM endpoint (users / groups), `externalId` binding, governed group mappings, termination-priority deprovisioning. |
| AU-09 | **High** | **No subject lifecycle.** Nothing creates a subject or binds an authentication identity, invites a user, or sets, changes or resets a password; there is no password policy. Principals exist only through seed tools. `identity.subject.created` / `identity.subject.disabled` are never produced. | IAM §2, §22, §23; Doc 05 §5.3; ZS-SEC-001 §10.1 | **L C** | Subject + authentication-identity admin API; invite / activation; self-service change and governed reset (separately controlled recovery, IAM §3); password policy including a breached-password check. |
| AU-10 | Med | **No session continuation.** The login token lives 300 s and the envelope 300 s, with no refresh token. A console must re-prompt for the password or hold it. | IAM §18 (refresh rotation); Doc 05 §5.3 bounded TTL | **C** | Comes with AU-06: refresh tokens with rotation and reuse detection, bound to the session record (so AU-01 revocation covers them). |
| AU-11 | Med | **Only TOTP.** There are no passkeys / WebAuthn, the preferred phishing-resistant factor. | IAM §3, §13 | **C** | WebAuthn factor in the same `principal_credentials` model; assurance level `PHISHING_RESISTANT`. |
| AU-12 | Med | **One audience for everything.** Every envelope has `aud=zoiko-internal`, so a token captured at one service is valid at all of them. | IAM §18 "narrow resource audience"; ZS-API-001 §24 | **L** | Per-service or per-resource-family audiences, or token exchange at the gateway. |
| AU-13 | Med | **Privileged activity is not evidence-linked.** Decision evidence has no structured privileged / break-glass / support session ID (break-glass appears only inside the basis text), no `BREAK_GLASS` tag or obligation, and support sessions produce no summary of resources accessed. "Commands executed under a session" cannot be listed for review. | IAM §13–15, §20; GOV-12 evidence; ZS-SEC-001 §17 | **C** | Session-ID columns on `access_decision_log`, a `BREAK_GLASS` reason code / obligation, and a per-session activity read used by the review routes. |
| AU-14 | Med | **No product-entitlement stage.** Pipeline stage 3 (scenario A04) is absent: authz never asks commercial-account-svc / capability-registry. | IAM §7 stage 3, A04; GOV-03 dependencies | **C** | Entitlement check (cached, COM-03) before candidate permissions, with a `FEATURE_DISABLED` / deny reason. |
| AU-15 | Med | **No legal-hold negative control.** A delete is not refused under hold (A23). | IAM §7 stage 6, A23 | **C** | Hold projection (GOV-10 events) as a negative control on destructive actions. |
| AU-16 | Med | **No relationship or qualification predicates.** There is no engagement / matter membership (A24) and no jurisdiction qualification (A25). Subject attributes are whatever the calling service sends: there is no attribute source (PIP) owned by an authoritative service. | IAM §6.2, §8 PIP, A24, A25 | **C** | A PIP for subject attributes (HR / qualifications) and relationship providers (audit engagements, legal matters). |
| AU-17 | Med | **Policy is not published under governance.** SoD rules, ABAC rules, roles and bundles take effect the moment they are created; there is no draft → approve → publish lifecycle. They are versioned and evented, but not independently approved. | IAM §8 PAP "no direct runtime mutation without controlled publication"; GOV §1 | **C** | Approval step (maker-checker) before activation for policy artefacts, as policy-svc's GOV-05 lifecycle does. |
| AU-18 | Med | **Approval limits use hard-coded FX rates** (e.g. 1 USD = 0.78 GBP in source), not an authoritative FX basis. | IAM §12, A09; §31 "authoritative FX basis" | **C** | Rates from currency-registry-svc with as-of evidence; §31 decision. |
| AU-19 | Med | **authorization-svc observability.** It has HTTP metrics only: no permit / deny / step-up rates, cache health, revocation lag or privileged-session gauges, and no alert rules of its own. identity-context has no MFA metrics or alerts (lockouts, replays, step-up failures). | IAM §26 | **C** | Domain metrics + alert rules (deny spikes, self-approval attempts, break-glass active, MFA lockout / replay bursts). |
| AU-20 | Med | **authorization-svc lacks its DoD artefacts.** It has no OpenAPI, AsyncAPI, runbook, release certificate or audit script. identity-context and gateway-auth certificates and `audit.sh` (17 Sep) predate the 14 Oct changes. | GOV DoR #3, DoD #1, #11, #12; IAM §29 | **C** | Publish contracts (OpenAPI 3.1, AsyncAPI), runbook, re-certified release certificates, updated audit scripts. |
| AU-21 | Med | **Performance and revocation SLOs are unmeasured.** There is no load test of `/v1/authorize` after the 14 Oct changes and no measured revocation propagation. The ownership plan's R22 (hot synchronous path) is unchanged: there is no PEP-side cache or sidecar. | IAM §29 Load / Revocation; Doc 05 §6.5; Doc 03 §3.9; R22 | **C** | Load test against the NFR C0 budget; revocation-lag measurement; decide on a PEP sidecar / local decision cache. |
| AU-22 | Med | **Role-profile projection drift (R10, tracker 79).** identity-context projects role assignments only from access-control-svc's events. Grants and revocations made directly in authz (admin API, expiry sweeper, HR-exit revocations) never reach it, so envelope `role_profile` can be stale. | Domain Ownership R10; tracker 79 | **C** | Consume `iam.assignment.*` from `zoiko.authorization.events` (or make authz the single writer and drop the duplicate). |
| AU-23 | Low | **CARTA.** A `STEP_UP_MFA` answer hard-blocks instead of sending the user to the step-up that now exists, and CARTA fails open when unreachable (an S5-D2 decision). | Doc 05 §3.11, §5.4 | **C** | Map `STEP_UP_MFA` → `401 step_up_required`; decide the fail mode. |
| AU-24 | Low | **Error and request-ID format.** Errors are not RFC 9457 `application/problem+json`, and there is no `Zoiko-Request-Id` (`X-Request-Id` is used instead). | ZS-API-001 §6, §12 (B-02) | **L** | Platform-wide X9 change. |
| AU-25 | Low | **Event names and channels drift.** <br>• ZS-IAM-001 §23 names `identity.subject.*` / `identity.session.revoked`, but identity emits `principal.status.changed` / `session.invalidated`. <br>• ZS-EVENT-001 wants `zoikosuite.<domain>.<stream>.v1` and `security.privileged-session.*`, but authz uses `zoiko.authorization.events` and `security.privileged_session.*`. <br>• There is no producer or consumer ACL on the broker. | IAM §23; EVENT §5.2, §14 | **C** | Resolve the IAM-vs-EVENT naming conflict, then alias or rename; broker ACLs. |
| AU-26 | Low | **API paths drift from §21.** The routes live at `/v1/admin/role-assignments` and `/v1/admin/delegated-authorities`, not `/v1/iam/access-assignments` and `/v1/iam/delegations`. authz also keeps a delegation write API that duplicates delegated-authority-svc (tracker 81). | IAM §21; tracker 81 | **C** | Aliases or the documented paths; retire the duplicate writer once the console has moved. |
| AU-27 | Low | **Bearer envelopes are not sender-constrained.** There is no DPoP or mTLS binding, so a stolen envelope replays for ≤ 300 s. | IAM §18; §31 decision | **C** | Decide per client class (§31). |

## B. Gaps that need other services

| ID | Sev | Gap | Evidence | Needs |
|---|---|---|---|---|
| X-R3 | **High** | **Every other service still takes `X-Principal-Id` / `X-Tenant-Id` on trust** when called directly. Only authorization-svc (and identity's MFA routes) verify the envelope. ZS-SEC-001 §9: forwarded context must be "signed … and revalidated by the receiving service". | **C** (grep of the estate) | Each service adopts authz's `internal/identity` gate (a shared module would be better). |
| X-82i | **High** | **About 86 `/v1/authorize` callers send no envelope.** Their decisions are logged without tenant or principal attribution (invisible in the audit read), and `ZS_ENVELOPE_ENFORCEMENT` cannot go `strict`. | tracker 82i | Each caller forwards the envelope (policy-svc's client is the reference). |
| X-M02 | **High** | **The decision endpoint answers anyone.** With no certificate and no headers: 200 (permit mode). Production defaults to `require`, but no PEP holds a client certificate yet, so require mode cannot go live. | **L** | Each PEP provisions its certificate and is added to `AUTHZ_PEP_ALLOWLIST`. |
| X-3 | Med | **PEPs do not forward `X-Authn-Age-Seconds`**, so `AUTHZ_STEP_UP_UNKNOWN_AGE=permit` and reopen-class step-up (A26) does not bite. | **C** | Callers forward the gateway's age header. |
| X-1 / X-2 | Med | **Tenantless callers, and no preparer on own-object actions** (REQUIRE_APPROVAL fallback). | register | Calling services. |
| X-84c | Low | **Always-grant `Authorize` stubs still exist in 7 services** (migration-integrity, tax-authority-interface, reporting-orchestration, hris-connector, external-data-feed, esignature-integration, banking-connector). They are dead code (0 callers) and a latent always-allow. | **C** | Delete them. |

## C. Front end (zoiko-suite-frontend-platform; not on this machine, unverified)

1. Send `Authorization: Bearer <envelope>` on every authorization-svc call; then compose →
   `AUTHZ_IDENTITY_ASSERTION=require`.
2. MFA UI:
   - enrolment (QR from `otpauth_uri`) and activation;
   - on `401 step_up_required`: prompt → step-up → re-resolve → retry;
   - admin reset page.
3. Send the client credentials at login (then `IDP_CLIENT_AUTH=required`). This is superseded
   by AU-06 if a real OIDC flow is adopted.
4. Route console → service calls through the edge (S5-B2).
5. Render server `available-actions` and denial / step-up states (IAM §29).

## D. Decisions and sign-offs (no code can close these)

| ID | Decision | Owner |
|---|---|---|
| D-1 | TOTP as the interim high-assurance factor; the assurance taxonomy (IAM §31) | Security |
| D-2 | Session maximums 8 h / 2 h / 4 h (IAM §31) | Security |
| D-3 | Break-glass approval and quorum by risk class (GOV §23; the IAM §14 vs GOV-12 / Doc 05 conflict) | Security + Finance / Control + Ops |
| D-4 | Build or buy the IdP / OIDC provider (AU-06 to AU-08) | Architecture + Security |
| D-5 | Revocation SLO by risk tier (IAM §31) | Security |
| D-6 | Authoritative FX basis for authority limits (IAM §31) | Finance / Control |
| D-7 | Token format and sender-constrained tokens per client class (IAM §31) | Security |
| D-8 | Platform items: RFC 9457 (X9); WAF / DDoS at the GCP edge; real client IP behind the load balancer; delete the stale `docker-compose.override.yml`; `init-db.sh` role ordering | Platform |

## E. Deployment state

- **Real databases are behind the code.**
  - identity_context: 000012, 000013;
  - authorization_svc: 000033 (+ earlier ones where missing);
  - access_control: 000020.
- **Staging / production:** `MFA_SECRET_KEY` (fatal without it); `AUTHZ_IDENTITY_JWKS_URL`
  (https :8449 — mTLS key fetch unexercised live while the override turns authz mTLS off).
- **Enrolment:** session issuers and approvers must enrol TOTP before they need to.
- **Commit state:** all 14 Oct work is uncommitted.

## Path to "done end to end" (recommended order)

1. **Close the live security holes in the three services.**
   - AU-01 revocation;
   - AU-02 break-glass approval (after D-3);
   - AU-03 authority-limit commands;
   - AU-04 login throttling;
   - AU-13 privileged-session evidence.

   These are fixable now without other teams.
2. **Production key and secret posture.**
   - AU-05 key management and rotation;
   - AU-12 audiences;
   - deploy E.
3. **Identity lifecycle and protocol** (needs D-4).
   - AU-06 OIDC code + PKCE with refresh (AU-10);
   - AU-09 subject lifecycle and password management;
   - AU-07 federation;
   - AU-08 SCIM;
   - AU-11 WebAuthn.
4. **Authorization completeness.**
   - AU-14 entitlement;
   - AU-15 legal hold;
   - AU-16 PIP / relationships;
   - AU-17 policy publication approval;
   - AU-18 FX;
   - AU-22 projection.
5. **Estate rollout** (other owners). X-R3, X-82i, X-M02, X-3, X-1 / X-2, X-84c, and the
   front-end track C.
6. **Certification.**
   - AU-19 observability;
   - AU-20 contracts, runbook, release certificates;
   - AU-21 load and revocation SLO;
   - AU-24 to AU-27;
   - a fresh end-to-end acceptance run of IAM §27 A01–A30 and GOV §19.

## Live behaviour probes (14 Oct 2026)

| Probe | Document requires | Observed |
|---|---|---|
| Logout, then reuse the envelope at authz | Denied after revocation | **200** |
| Logout, then gateway `/verify` | Denied | **200** |
| Break-glass issue | Independent approval before ACTIVE | **201 ACTIVE**, `approved_by` empty |
| 25 failed logins from one client | Throttled (429) | **401 every time** |
| `/v1/authorize` with no certificate, no headers | Verified workload only | **200** (permit mode) |
| `POST /v1/admin/authority-limits` | Governed command | **404** |
| `POST /v1/principals` | Subject creation | **404** |
| Password reset / change routes | Recovery flow | **404 / 404** |
| `/.well-known/openid-configuration` | AS metadata | **404** |
| authz error Content-Type | `application/problem+json` | `application/json` |
| Envelope `aud` | Narrow per resource | `zoiko-internal` for all |
| Request-ID response header | `Zoiko-Request-Id` | `X-Request-Id` |

The 14 Oct E2E (41 / 41) remains valid for what it covered:
- the step-up chain;
- the A-01 gate;
- the session caps;
- the admin MFA reset.
