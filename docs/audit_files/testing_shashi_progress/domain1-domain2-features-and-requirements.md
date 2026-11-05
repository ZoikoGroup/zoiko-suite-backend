# Domain 1 and Domain 2: features and what each still needs

**Date:** 2 November 2026
**Basis:** branch `shashi-changes` at commit `2a87f299` (23 commits behind `origin/main`).
**Scope:** the 16 core services: Domain 1, Identity, Scope & Foundation (9 services), and Domain 2, Governance Platform (7 services).

**Sources:**
- `docs/audit_files/Identity, Scope & Foundation-audit-2026-09-23.md` (latest sections: 19 Oct re-audit and fixes, 21 Oct cross-domain pass)
- `docs/audit_files/Governance Platform-audit-2026-09-28.md` (latest sections: 19 Oct re-audit, 21 Oct workflow completion pass, per-service gap registers)
- `docs/audit_files/IMPLEMENTATION-PROGRESS.md` (status board, API-3 per-service status)
- `docs/audit_files/IMPLEMENTATION-PLAN-2026-10-23.md` §4 (where missing sections are built), §5 (decisions taken), §6 (tasks)
- Route registration in each service's `internal/handler` (to confirm what exists in code)

**Status legend:**
- **DONE**: built and verified by the latest audit pass.
- **PARTIAL**: built, but a dependency, decision or part of the behaviour is missing.
- **MISSING**: not built.
- **BUILT (not audited)**: routes exist in code, but no audit pass has scored them yet.

Task ids in brackets (API-3, IAM-12, WF-3, ...) refer to the implementation plan. Gap ids (S1-B2, J-01, D-03, H-1, ...) refer to the gap registers in the two audit files.

---

## Summary

| # | Service | Port | Spec | Latest audit score | API-3 (ETag / If-Match) | Tests on 30 Oct |
|---|---|---|---|---|---|---|
| D1-1 | identity-context-svc | 8080 | GOV-01 §4, ZS-IAM-001 | 42/44 (95.5%, 96.6% weighted) | DONE | **4 failing** |
| D1-2 | tenant-entity-registry-svc | 8081 | ORG-02, ORG-03 | 47/48 (97.9%, 99.0%) | DONE | not re-run |
| D1-3 | configuration-feature-flag-svc | 8086 | ZS-SVC-AA-001 | 47/49 (96%, 98%) | PARTIAL | **does not compile** |
| D1-4 | secret-vault-integration-svc | 8087 | Security Std §3.1, §9, §13 | 18/21 (86%, 93%) | DONE | pass |
| D1-5 | gateway-auth-svc | 8092 | Security Std §8, GOV-01 ingress | 22/22 (100%) | n/a (no mutable aggregate) | not re-run |
| D1-6 | search-indexer-svc | 8096 | ZS-SVC-AB-001 | 47/49 (96%, 98%) | DONE | pass |
| D1-7 | notification-svc | 8133 | ZS-SVC-Y-001 | 35/37 (95%, 97%); 110-row matrix 94% weighted | marked DONE | **25 failing** |
| D1-8 | delegated-authority-svc | 8136 | ORG-06 (delegation half) | 23/23 (100%) | DONE | pass |
| D1-9 | access-control-svc | 8137 | Authz Std §5, §9, §10.1, §19, §24 | 10/10 (100%) | DONE | pass |
| D2-1 | jurisdiction-rules-svc | 8082 | Doc 03 §8.2, ZS-JUR-001, V-001 PDC-02 | 73/4/2 of 79 (92.4%, 94.9%) | DONE | pass |
| D2-2 | governance-decision-log-svc | 8083 | Doc 03 §8.7, GOV-07 | 41/6/0 of 47 (87.2%, 93.6%) | DONE (ETag on reads) | not re-run |
| D2-3 | policy-svc | 8085 | Doc 03 §8.1, GOV-05, V-001 | 54/2/0 of 56 (96.4%, 98.2%) | DONE | pass |
| D2-4 | obligations-svc | 8088 | Doc 03 §8.5 (WFC-05 unscored) | 41/22/2 of 65 (63.1%, 80.0%) | DONE | not re-run |
| D2-5 | authorization-svc | 8089 | Doc 03 §8.3, GOV-03/04/12, ZS-IAM-001 | 85/0/0 of 85 (100%) | PARTIAL (sessions only) | pass |
| D2-6 | workflow-svc | 8090 | Doc 03 §8.4, GOV-06, R-001 WFC-02/03 | 36/25/0 of 61 (59.0%, 79.5%) | DONE | pass |
| D2-7 | evidence-requirements-svc | 8130 | Doc 03 §8.6 | 39/1/3 of 43 (90.7%, 91.9%) | DONE (5 Nov) | pass (5 Nov: audit 42/42, E2E 84/84) |

Domain 1 group score: 291/303 (96.0% full, 97.9% weighted). Domain 2 group score: 350 met / 77 partial / 9 failed of 436 (80.3% full, 89.1% weighted).

"Not re-run" means the service was not in commit `2a87f299`, so its suite was not run on 30 Oct. Its last green run is recorded in the audit files (19–21 Oct).

---

## Do first

These block everything else:

1. **configuration-feature-flag-svc does not compile.** `internal/handler/handler.go:798:2: syntax error: non-declaration statement outside function body`. `writeJSONWithETag` and `requireIfMatch` are pasted three times around lines 785–829, and one copy sits inside `decodeJSON`. Keep one definition of each.
2. **identity-context-svc: 4 failing handler tests.** `TestUpdatePrincipalStatus_NoSelfExemption` gets 428 (the test sends no `If-Match`). `TestAuthorizeForwardsTheEnvelopeLegalEntityNotTheTenant`, `TestAuthorizePrefersTheParsedEnvelopeOnContext` and `TestSupportApprovalRoute` also fail.
3. **notification-svc: 25 failing tests**, all `412 stale_version`. The tests send the wrong `If-Match` versions (the fakes report version 0 or 2). `IMPLEMENTATION-PROGRESS.md` marks this service's API-3 as DONE, which is not true until these pass.
4. **Run CI on the branch.** No CI run exists for the recent commits.
5. **Bring `shashi-changes` up to date with `main`** (23 commits behind).

---

# Domain 1: Identity, Scope & Foundation

These services answer "who are you, which organisation are you in, and what is switched on?"

## D1-1. identity-context-svc (:8080)

**Features**
- **DONE:** resolve context (`POST /v1/context/resolve`). Returns principal, tenant, active legal entity, role profile, delegations and session trust as a signed envelope. Correlation is required.
- **DONE:** get the effective context (`GET /v1/context/session/{id}`).
- **DONE:** explain a resolution (`GET /v1/context/session/{id}/explain?as_of=`), including source-input provenance (channel, workload, causation, ingress version, cache state, entitlement status).
- **DONE:** refresh the cache, invalidate a tenant, invalidate a session.
- **DONE:** break-glass support context.
  - The request answers `202 PENDING_APPROVAL` and grants nothing.
  - The named approver makes it live with their own `POST /v1/context/support/{id}/approve`.
  - Review, revoke, a 4 h maximum TTL and a reconciler sweep.
- **DONE:** session revocation driven by events: `authority.revoked`/`.expired`, `role.updated`, `entity.updated` and `iam.assignment.*` (Kafka, at-least-once, commit after apply).
- **DONE:** principal admin (`/v1/principals/*`). A status change emits `principal.status.changed` and `identity.subject.enabled/disabled`, and ends sessions on disable.
- **PARTIAL:** login (`POST /v1/authenticate`) with a confidential client (`client_id`/`client_secret`), so `azp` maps to a verified channel. The console does not send client credentials yet.
- **PARTIAL:** TOTP MFA: enrol, activate, revoke, list factors, step-up (`POST /v1/authenticate/step-up`), admin reset (`POST /v1/principals/{id}/mfa/reset`). Backend only; no console UI.
- **DONE (wired 19 Oct; AU rows not re-scored):** OIDC provider.
  - Discovery, authorize with code + PKCE, token, refresh rotation, revoke, introspect, logout.
  - Login throttle, DPoP verification, multi-key/KMS keyring, subject lifecycle service.
- **MISSING:** SCIM, federation and passkey endpoints. The stores exist, but the routes are deliberately not registered.
- **PARTIAL:** session concurrency cap (default 5). Evicting the oldest session emits no `identity.session.revoked` and no SIEM event. The cap also applies to service accounts.
- **MISSING:** entitlement context reference. Every decision records `UPSTREAM_NOT_CONFIGURED`. A COM-03 client exists, but `COMMERCIAL_ACCOUNT_URL` is not set.
- **DONE:** Idempotency-Key replay (bound to the principal), workload-identity provenance, source-channel provenance, transactional outbox, RFC 9457 errors (API-2).
- **DONE, tests failing:** ETag/If-Match on Principal and SupportContext (API-3).

**Required to make all features work**
- Fix the 4 failing handler tests (see "Do first").
- Set `COMMERCIAL_ACCOUNT_URL` and verify the entitlement path (S1-C1, IAM-2).
- Console:
  - send `client_id`/`client_secret` at login, then set `IDP_CLIENT_AUTH=required` (R-1);
  - add MFA screens: enrol with QR, step-up prompt on `401 step_up_required`, admin reset page (FE-1).
- Grant the six `IDENTITY_*` actions to operator roles by migration. Today only the local seed script grants them (S1-B2).
- Refresh ingress bindings from tenant-entity-registry-svc (event or scheduled poll), so bindings do not read `STALE` after the TTL (S1-B3).
- Security sign-off on TOTP as the interim high-assurance factor. WebAuthn later.
- Plan tasks:
  - IAM-1 per-service audience (AU-12);
  - IAM-6 observability, IAM-7 DoD artefacts, IAM-8 load and revocation SLOs;
  - IAM-9 role-profile drift, IAM-10 canonical event names, IAM-11 `/v1/iam` aliases;
  - IAM-12 federation, SCIM and passkey;
  - ORG-1 (ORG-01 identity gaps).
- Re-score AU-01 … AU-11 in `Auth-Service-audit-2026-10-14.md` now that the auth plane is wired.
- Refresh `RELEASE_CERTIFICATE.md` and `scripts/audit.sh`. Both are stale since 18 Sep.
- Emit `identity.session.revoked` and a SIEM event when the session cap evicts a session.
- **Blocked:** choice of external IdP vendor.

## D1-2. tenant-entity-registry-svc (:8081)

**Features**
- **DONE:** tenant commands:
  - Create, with maker-checker and an onboarding-key idempotency key;
  - Activate, Suspend, Resume;
  - InitiateTermination and CompleteTermination (maker-checker);
  - ChangeDefaultLocale; ChangeHomeRegion (maker-checker, evidence enforced by a constraint);
  - FailedProvisioning state with RetryProvisioning and AbandonProvisioning.
- **DONE:** tenant reads: GetTenant, ResolveTenantByHost, ListTenantLifecycleHistory, GetTenantDefaults.
- **DONE:** legal-entity lifecycle: DRAFT → VERIFIED (evidence required) → ACTIVE → Inactive/Dissolved. Also amend, approved rename, non-destructive merge/unmerge, and quarantine of conflicting registry identities.
- **DONE:** entity reads: GetLegalEntity, FindByRegistryNumber, ListEntityVersions, GetLegalEntityAsOf.
- **DONE:** LEI with check digits, source and status; ISO 20275 ELF codes keeping the local text.
- **DONE:** purpose-scoped read of sensitive identifiers (the RESTRICTED tax-identity bundle), authorized at the target entity.
- **DONE:** entity hierarchies, jurisdiction assignments, residency policies and regions, workspaces.
- **DONE:** RLS tenant isolation, `If-Match`/`expected_version` on every protected command, typed `error_code`, outbox events carrying `object_id`/`object_version`.
- **DONE:** `GET /v1/entities/{id}/status`, used by workflow-svc, obligations-svc and evidence-requirements-svc.
- **PARTIAL:** fiscal-calendar reference. It is format-checked only; the audit says no REF-04 service exists to resolve it against.
- **MISSING (planned modules, plan §4):**
  - ownership and control (ORG-04); org units and org graphs (ORG-05);
  - parties and golden record (MDM-01…05); addresses and establishments;
  - registrations, licences and identifiers;
  - reference sets: FX, books, dimensions, units, `/v1/reference/{set}/{code}`;
  - master imports; GOV-02 runtime reads.

**Required to make all features work**
- Build plan tasks ORG-1 … ORG-9 and G-02 (the planned modules above).
- A REF-04 fiscal-calendar source to resolve `fiscal_calendar_id` against (S2-C1).
- Each consuming service persists `object_id`/`object_version` from events (§9.2 gate 6, S2-C2).
- Run a restore drill on the production database and measure RPO/RTO (§9.2 gate 7, S2-C3).
- The console adopts the 16 contract changes listed in the service's `SPEC_DEVIATIONS.md`.
- The ingress must preserve the client's `Host` header. A proxy that rewrites it silently disables NP3.
- **Blocked:** choice of address/LEI verification provider.

## D1-3. configuration-feature-flag-svc (:8086)

**Features**
- **DONE:** config definitions registry: type, owner, scopes, fallback, safety class, allowed regions.
- **DONE:** immutable publish. Publishing an S2/S3 key requires an `approval_reference`.
- **DONE:** overrides on five layers (environment, service, tenant, org unit, user preference), with an allowlist and fixed precedence.
- **DONE:** resolve (`POST /v1/config/resolve`) with snapshot lineage, `UNKNOWN_KEY` / `NO_VALUE` / `SAFE_DEFAULT` / `RESIDENCY` / `STALE_SNAPSHOT` answers, and residency checks.
- **DONE:** immutable, fetchable snapshots (`GET /v1/config/snapshots/{id}`).
- **MISSING:** snapshot signing.
- **DONE:** feature flags:
  - create; release plans with eligibility first;
  - deterministic, monotonic buckets salted by key; evaluate;
  - kill switch; retirement tombstones.
- **DONE:** change sets (C1/C2/C3):
  - propose, then approve (proposer cannot approve; server sets `approved_at`; only PROPOSED/VALIDATED can be decided);
  - activate (effective time, `expected_before_hash`), then VERIFIED after the fleet converges;
  - rollback from the before-snapshot.
- **PARTIAL:** emergency (break-glass) changes. S2/S3 only, TTL, exact revert at expiry, mandatory retrospective. Who may break glass is not restricted.
- **PARTIAL:** runtime attestation and drift detection. `X-Workload-Id` is not taken from a verified identity.
- **PARTIAL:** plan eligibility (INV-02). It trusts `X-Commercial-Plan`.
- **PARTIAL:** `secret://<env>/<path>` references. Nothing resolves them.
- **DONE:** 8 events through the transactional outbox.
- **MISSING:** GOV-11 feature policy and capability exposure (task G-11).
- **PARTIAL:** ETag/If-Match (API-3). Present on config definitions only, and the handler currently does not compile.

**Required to make all features work**
- Fix the compile error (see "Do first").
- Finish API-3 on ConfigChange, EmergencyChange, ReleasePlan and KillSwitch. `GetChange` and `GetEmergencyChange` were added to the store in `2a87f299`.
- A dedicated emergency grant in authorization-svc, plus a required live IAM break-glass context on `/emergency-changes` (S3-B1).
- Take the workload identity for attestation from the mTLS client certificate (S3-B2).
- Resolve `secret://` references through the secret-vault-integration-svc broker (S3-B3).
- The edge sets `X-Commercial-Plan` (from commercial-account-svc) and `X-Org-Unit-Id`, and strips client copies. Decided as S3-D1; closes INV-02 and NP-08.
- Sign snapshots with a key-management-svc key (local key until SEC-1). Decided as S3-D2.
- Migrate the dev database to the latest migration and re-run `scripts/audit.sh` live (S3-B4).
- Build G-11.

## D1-4. secret-vault-integration-svc (:8087)

**Features**
- **DONE:** secret policies and versions: create, list, activate, list versions.
- **DONE:** material seeding, manual rotation, and an automated rotation sweeper (one claim per due slot).
- **DONE:** broker (`POST /v1/secrets/broker`). Issues a signed lease token (`ltk:v3`) bound to path, lease and expiry.
- **DONE:** lease verify, get, list and revoke; audit read. Every read route is authorized.
- **DONE:** break-glass emergency retrieval. Needs an active exception for this exact path and approver ≠ retriever; the reason is required and evidence is written before release.
- **DONE:** shared-secret exception register (create, list, revoke).
- **DONE in code:** inbound mTLS with an identity check, and KMS envelope encryption (Vault Transit or Cloud KMS). Production refuses to boot without the infrastructure. A local encrypted-file backend is used until then.
- **PARTIAL:** workload identity bound to environment, region and audience (O-1). The checks are built but off.
- **PARTIAL:** non-exportable workload credentials (O-2). The mTLS private key is generated by the issuer and sent over the wire.
- **MISSING:** dynamic credentials (O-3). Material is static today.
- **MISSING:** tokenization, masking and pseudonymization (SEC-4).
- **DONE:** ETag/If-Match on SecretPolicyVersion, SecretLease and SharedSecretException.

**Required to make all features work**
- Infrastructure:
  - create a Vault Transit or Cloud KMS key, then migrate existing material once with `VAULT_MIGRATE_LEGACY_KEY_FILE`;
  - provision server and client certificates;
  - register an exception for every secret that more than one workload brokers.
- mtls-management-svc (SEC-6) adds a URI SAN `spiffe://zoiko/<env>/<service>` and a CSR-signing endpoint. Then set `MTLS_REQUIRED_URI_PREFIX` and `MTLS_MAX_CLIENT_CERT_LIFETIME=24h`, mandatory in production (O-1, O-2).
- A gateway client certificate for the edge → service hop (S4-B1).
- Per-lease dynamic credentials from a Vault secrets engine (decided S4-D1). **Blocked** until Vault infrastructure exists.
- SEC-4 tokenization, after SEC-1 key custody.
- Rotate or allowlist the 6 repo-wide gitleaks findings.

## D1-5. gateway-auth-svc (:8092)

**Features**
- **DONE:** ForwardAuth `/verify`. Verifies the signed envelope against identity-context-svc's JWKS (issuer and audience pinned) and refuses incomplete claims.
- **DONE:** hostname ↔ tenant binding (NP-2) with no fallback; tenant-context resolution; fail closed.
- **DONE:** sets the 8 server-resolved headers and strips forged identity headers (split policy). The 8 caller-asserted envelope fields pass through.
- **DONE:** CARTA risk check on every request (anything but ALLOW blocks), SIEM denial stream, `X-Auth-Denial-Reason`.
- **DONE:** assurance headers: `X-Assurance-Level` from MFA, `X-Authn-Age-Seconds` from `auth_time`, verified source channel, verified support-context id.
- **DONE:** mTLS to identity-context-svc and tenant-entity-registry-svc (SEC-INV-05).
- **UNDECIDED:** production internet-edge controls: DDoS, WAF, rate limiting, bot/abuse, TLS, request-size limits (6 open rows).

**Required to make all features work**
- Decide where the production edge controls live (e.g. Cloud Armor in front of the GCP load balancer) and provision them (S5-D1).
- Confirm the code matches the decision "CARTA unreachable: fail closed on writes, open on reads" (S5-D2, decided 21 Oct).
- A trusted-hop design so CARTA scores the real client IP behind the GCP load balancer (S5-D3).
- Console → service calls bypass ForwardAuth (S5-B2, R-3). Route them through the edge, or have every service verify the envelope. About 100 services still trust `X-Principal-Id` on direct calls.
- Set `X-Commercial-Plan` and `X-Org-Unit-Id` at the edge (S3-D1).
- Per-service enrolment in mtls-management-svc (M-01), and Traefik `serversTransport` client certificates.

## D1-6. search-indexer-svc (:8096)

**Features**
- **DONE:** governed lexical search (`POST /v1/search`): mandatory filters, query safety and complexity bounds, field policy, bounded result window with a signed cursor, freshness on every response.
- **PARTIAL:** semantic/hybrid search (`POST /v1/search/semantic`) and the six vector-index controls. Built and verified with a stub; answers ESR-019 because no embedding provider is configured.
- **DONE:** retrieve with per-candidate re-authorization (`POST /v1/retrieve`).
- **DONE:** retrieval classes R0, R1 and R3 (export).
- **PARTIAL:** R2 source hydration. Wired for `obligation` only.
- **DONE:** index contracts (immutable after publication) and index generations: backfill, ledger validation, activate, rebuild, model-migration certification (recall@k).
- **DONE:** restrictions/erasure with restriction epochs; an erased record cannot return.
- **DONE:** checkpoints, broker-measured lag, STALE/UNKNOWN freshness gates (ESR-012), restriction-failure exclusion (ESR-018).
- **DONE:** search exports with purpose, separate permission and evidence; facet privacy (minimum cell); snippet field policy.
- **DONE:** 8 events, 20 reason codes, search evidence, search sources, retrieval evaluations, Idempotency-Key, ETag/If-Match on contracts and generations.
- **OPEN QUESTION:** RAG contract (§10.2), mostly owned by AI governance.

**Required to make all features work**
- Choose an embedding provider and model (OD-10), set `EMBEDDING_PROVIDER_URL`, certify recall. **Blocked** on that choice.
- Decide which scopes are R2 (OD-05) and set `SOURCE_SERVICE_URL_<TYPE>` for each.
- A replay API in each owning domain service, so rebuilds can go past Kafka retention (S6-C2).
- Re-run the grant-dependent `scripts/audit.sh` sections (7–12, 16) against a real, migrated authorization-svc.

## D1-7. notification-svc (:8133)

**Features**
- **NCD-01, intents and templates:**
  - **DONE:** communication intents: versions, activation by a second principal, retire.
  - **DONE:** templates: validate (15 checks), approve/reject (approver ≠ author), publish (effective-dated, immutable), retire.
  - **DONE:** side-effect-free render preview; bitemporal "effective" read.
- **NCD-02, recipients and channels:**
  - **DONE:** recipient resolution (IAM-verified endpoints, hashed and masked).
  - **PARTIAL:** channel decision. The privacy/marketing permission is supplied by the caller.
  - **DONE:** suppressions with governed lift; preferences; revalidate before submit.
- **NCD-03, delivery:**
  - **DONE:** durable delivery jobs; attempt state machine enforced by the database; certified provider routing with a circuit breaker.
  - **DONE:** multi-channel fallback; quotas that defer and never drop; priority; storm dedupe; governed bulk sends.
  - **DONE:** cancel, misdelivery handling, corrections.
- **NCD-04, evidence:**
  - **DONE:** append-only delivery evidence; authenticated bounce and complaint ingestion; channel reputation; canonical suppression.
- **NCD-05, regulated notices:**
  - **DONE:** notice packages, recipient acknowledgments bound to the content hash, deadline escalation.
  - **PARTIAL:** record declaration to DRC. It arrives by callback.
- **DONE:** delivery semantics. No generic "SENT"; stable `communication_id`; purpose-scoped idempotency; UNKNOWN after an ambiguous submit, reconciled first; resend requires a reason.
- **DONE:** operations. Sealed unsubscribe token (RFC 8058); DKIM and sender-auth monitor; metrics and alerts; runbook; 110-row certification matrix.
- **PARTIAL:** legacy free-text send path (`POST /v1/notifications`, `/events/ingest`). It is still open, so INV-01 is not met.
- **PARTIAL:** channels. EMAIL and IN_APP only; SMS and push are missing.
- **MISSING:** email templates: 128 templates, 32 state variants and trigger routing (EML-1 … EML-6).
- **Marked DONE, 25 tests failing:** ETag/If-Match on Notification, Template and TemplateVersion.

**Required to make all features work**
- Fix the 25 failing tests (see "Do first"), then correct the API-3 line in `IMPLEMENTATION-PROGRESS.md`.
- Call privacy-decision-svc (`POST /v1/privacy/decisions`) in the channel decision. First register the service's purposes in privacy-purpose-registry-svc (G-1).
- Declare regulated-notice evidence packages as records. Legal hold sits with retention-registry-svc (decided S7-D3) (G-2).
- Lease the provider, callback, webhook, unsubscribe and DKIM secrets from secret-vault-integration-svc instead of environment variables (G-3).
- Grant `TEMPLATE_MANAGE`, `TEMPLATE_APPROVE`, `NOTIFICATION_SUPPRESS` and `NOTIFICATION_RESOLVE_OUTCOME` by migration (G-9).
- Console (G-8, G-7):
  - screens for intents, template approval, suppressions, preferences, regulated notices, evidence, exceptions and reputation;
  - a recipient acknowledgment page;
  - move the send form to `POST /v1/communications`.
- A product decision per workflow on which service notifies whom, on which event, with which template. Then move callers to `POST /v1/communications` and close the legacy path (G-6, INV-01).
- An MDM party source for external contacts and cross-tenant relationships (G-4). The plan puts it in tenant-entity-registry-svc (ORG-5).
- Deadline clocks from obligations-svc (WFC-05, task WF-3) instead of caller-supplied deadlines (G-5).
- Signed, expiring secure links. S1+ content also needs a signed-in session (NP-33, decided S7-D2).
- An estate control so only notification-svc can reach provider credentials (NP-01, TC-01).
- Plan tasks EML-1 … EML-6 and NCD-1.
- **Blocked:** SMS and push provider choices (OD-03, OD-04); sender DNS.

## D1-8. delegated-authority-svc (:8136)

**Features**
- **DONE:** create a delegation.
  - The delegator is bound to the caller and must hold the action in their own right.
  - SoD checked through `/v1/sod/validate`; self-dealing refused.
  - An on-behalf grant is PROPOSED and needs an independent approver.
- **DONE:** revoke, extend, suspend/resume; cross-tenant expiry sweeper.
- **DONE:** AuthorityLimit (monetary or quantity ceiling).
- **DONE:** overlap prevention (EXCLUDE constraint); Idempotency-Key replay; `409 correlation_reused`.
- **DONE:** effective delegations as of a date; ExplainDelegationChain.
- **DONE:** durable evidence rows for refused escalations; `evidence_ref` on every event.
- **DONE:** `If-Match` / `expected_version` (428 without, 412 when stale).
- **DONE:** events `authority.delegated` / `.revoked` / `.expired`, projected into authorization-svc and identity-context-svc.

**Required to make all features work**
- Quantity (non-monetary) authority limits for the delegator, in authorization-svc `authority_limits` (decided S8-D1).
- IAM-13: the ORG-06 role half in authorization-svc. AssignBusinessRole, SuspendAssignment, ListPrincipalAssignments, GetAssignmentsAsOf.
- Console screens.

## D1-9. access-control-svc (:8137)

**Features**
- **DONE:** role definitions and permission bundles, provisioned into authorization-svc.
- **DONE:** permission registry. 65 §5 `resource.action` names plus 668 legacy names; unknown actions refused.
- **DONE:** system role templates (append-only) and the 21 §9.1 archetypes, with instantiate and upgrade.
- **DONE:** assignment requests.
  - Risk-tiered approval, with SoD re-checked against what the subject already holds.
  - CRITICAL roles also need a security approver.
  - Revoke, including effective-dated revocation; expiry sweep.
- **DONE:** groups and group assignments, fanned out to one governed request per member.
- **DONE:** access reviews.
  - Periodic, privileged, and event-triggered from HR events via administered subject links.
  - Flags: ORPHANED_ROLE, SOD_CONFLICT, DORMANT, SUBJECT_INACTIVE.
  - Decisions: KEEP, REVOKE, MODIFY, ESCALATE. REVOKE runs as `svc-access-control`.
- **DONE:** sessions end on role or assignment change; Idempotency-Key; outbox events; refused-escalation evidence; ETag/If-Match on roles and bundles.

**Required to make all features work**
- Add `resource.action` aliases for the 668 legacy action names (decided S9-D1, not built).
- Grant the `svc-access-control` service identity in real deployments.
- Console screens (FE-1).

## Domain 1: cross-cutting requirements

- Restore a green build and suite first (see "Do first").
- API contract: finish API-3 in configuration-feature-flag-svc. API-4 (cursor pagination), API-5 (async operations) and API-6 (Deprecation/Sunset) are all TODO.
- Live verification: no Docker image builds or live compose acceptance since 21 Oct, and no CI run on the recent commits.
- Envelope verification estate-wide (R-3). Services that trust `X-Principal-Id` on direct calls must verify the signed envelope (authorization-svc's `internal/identity` is the pattern).
- Production grants by migration, not by `seed-demo-rbac.ps1`.
- Plan tasks: SEC-1 … SEC-9 (key custody, crypto agility, SIEM, tokenization, CARTA, mTLS, security policy, CI supply chain, backup/DR), FE-0 … FE-2, CLOSE-1 … CLOSE-3.
- **Blocked on vendor choices:** embedding provider, SIEM vendor, cloud KMS, external IdP, address/LEI verification provider, sender DNS.

---

# Domain 2: Governance Platform

These services answer "is this allowed, under which rules, who must approve, and what proof is needed?"

## D2-1. jurisdiction-rules-svc (:8082)

**Features**
- **DONE:** jurisdiction registry: list, get, ancestors, create, deactivate. Nothing is deleted.
- **DONE:** effective-dated rules: rules by date, get rule, status history, create, transition DRAFT → ACTIVE (creator ≠ activator), end-date.
- **DONE:** rule pack (`GET /v1/jurisdictions/{id}/rule-pack`). Resolves the parent chain to one winning rule per code, names its source, returns `pack_digest`.
- **DONE:** fail-closed jurisdiction check (`GET /v1/jurisdictions/{id}`) for other services; 503 means "cannot answer", never "no".
- **PARTIAL:** working out which jurisdictions apply to an entity or transaction. Resolves the ancestor chain only, plus the `/v1/entities/{id}/jurisdictions` projection.
- **BUILT (not audited):** `POST /v1/validate-action`. Built after the 19 Oct audit; `JURISDICTION_RULE_VALIDATE` registered in P0-2.
- **PARTIAL:** `POST /v1/rule-resolutions:resolve` with `rule_decision_evidence`. `RESOLVER_ENABLED` is off by default.
- **BUILT:** `POST /v1/regulatory-calculations:execute`.
- **BUILT:** regulatory calendars (versions, sources, publish) and obligation rules (provenance, publish); `POST /v1/obligation-calculations:calculate`.
- **BUILT:** pack compile, sign, verify and artifact download; signing keys (register, retire, revoke).
- **Legal drift:** recording by hand (`POST …/drift`, `legal.drift.detected`) is DONE. Automatic detection from a regulatory feed is MISSING.
- **PARTIAL:** entity-jurisdiction consumer (from tenant-registry events). No backfill for entities whose events left Kafka retention.
- **DONE:** bitemporal replay, idempotency keys, ETag/If-Match, append-only triggers, outbox, RFC 9457 errors.

**Required to make all features work**
- external-data-feed-svc publishes a regulatory-change event carrying `external_feed_reference`. Add a consumer here that moves matching rules to `UNDER_REVIEW` and records drift (J-03, J-04; cross-team).
- Resolve `X-Approval-Reference` against workflow-svc on DRAFT → ACTIVE (J-02).
- Verify identity (signed envelope or mTLS) instead of trusting `X-Principal-Id`; remove the published `8082:8082` port (J-01, X5).
- Decisions:
  - owner of entity applicability (C8, J-05);
  - enable `RESOLVER_ENABLED` (J-07);
  - one precedence model (J-08);
  - public reads, `rule_payload` content, event naming (J-09).
- In-service fixes:
  - backfill job for old entities (J-10);
  - write registry events inside the transaction (J-11); `aggregate_version` on registry events (J-12);
  - `FOR UPDATE SKIP LOCKED` in the outbox worker (J-15); replay tool for parked events (J-16).
- Plan tasks: REF-1 (REF-01 registry commands), REF-3 (REF-10 business calendars and payment terms), JUR-1 (`jurisdiction-decisions:resolve`, after ORG-5 … ORG-7).
- Apply the missing local migrations with `deployments/migrate.py up` (E-1); extend the Postman collection (E-4).

## D2-2. governance-decision-log-svc (:8083)

**Features**
- **DONE:** append a decision (`POST /v1/decisions`). Tenant-owned, immutable once written, idempotent on `(tenant_id, decision_id)`.
- **DONE:** query decisions by actor and other fields (`GET /v1/decisions`); get by id.
- **DONE:** replay a decision (`POST /v1/decisions/{id}/replay`); list replay manifests (tenant-scoped).
- **DONE:** outbox and relay for `governance.decision.recorded` / `.replayed`. Not proven against a real broker.
- **DONE:** `jurisdiction_context` on decisions, column-width and size guards, RFC 9457 errors, ETag on reads.
- **PARTIAL:** read authorization. Compose runs `DECISION_LOG_READ_AUTHZ=warn`.
- **MISSING:** GOV-07 evidence ledger and verification (hash chain, verification), task G-07.

**Required to make all features work**
- contract-lifecycle-svc, decision-support-svc and evidence-manifest-svc forward the envelope and `X-Principal-Id` (D-04).
- Grant each reader `GOVERNANCE_DECISION_READ`, then set `DECISION_LOG_READ_AUTHZ=enforce` (D-03).
- Identity verification through gateway ForwardAuth and mTLS workload identity (D-05, X5).
- Decide whether `X-Purpose-Context` is required on reads or on appends (D-06), and event naming (D-07, C3).
- Answer the 5 open questions:
  - meaning of the "audit" query dimension;
  - whether the GOV-07 hash chain applies;
  - whether `decision_id` is the §16 `evidence_id`;
  - retention class for decisions;
  - which GOV section owns GovernanceDecision.
- Prove workflow-svc's ledger relay live. It was coded on 21 Oct with no run against a real decision log.
- Build G-07.
- Remove the tracked `server.exe`.

## D2-3. policy-svc (:8085)

**Features**
- **DONE:** policies and versions: create, list applicable, get, version history, compare versions, get version, transitions.
- **DONE:** GOV-05 lifecycle.
  - submit-review, approve (maker-checker plus own-object SoD through authorization-svc), activate, schedule;
  - suspend, review-suspension (the suspender cannot review), retire;
  - `If-Match` mandatory, 428 without it.
- **DONE:** evaluate (`POST /v1/policies/evaluate`, e.g. `APPROVAL_THRESHOLD`), resolve, explain, get evaluation evidence.
- **DONE:** evidence relayed into governance-decision-log-svc as `svc-policy` through a durable outbox.
- **DONE:** 8 event types; `jurisdiction_context` and `residency_region` on events.
- **DONE:** control tests (definitions, executions, effectiveness) and attestations (create, get, revoke).
- **DONE in code:** identity verification (JWKS envelope, or mTLS on `:8449`). Compose runs `POLICY_IDENTITY_VERIFY=warn`.
- **MISSING:** evaluation for SPEND_CONTROL, SIGNATORY_MATRIX and SOD_RULE. No business rules define them.
- **MISSING:** `openapi.yaml`.
- **MISSING:** REF-07 accounting policy (`ACCOUNTING_POLICY` type), task REF-2.
- **Roadmap:** V-001 PDC-05 certification suites, simulation and waivers.

**Required to make all features work**
- Business input for the three policy types (P-01); then an evaluator and payload validation per type.
- Write `openapi.yaml`.
- governance-decision-log-svc forwards the envelope when it replays; then set `POLICY_IDENTITY_VERIFY=enforce` (P-12).
- Grant `svc-policy` `GOVERNANCE_DECISION_RECORD` in production.
- Build REF-2.
- A Kafka broker to prove the event relay end to end.
- Callers (expense-claim-svc, payment-authorization-svc) report a policy-svc 401/403 as 401/403, not 503 (P-13).

## D2-4. obligations-svc (:8088)

**Features**
- **DONE:** obligations: create (dedup on tenant + code, jurisdiction validated fail-closed, entity-bound), list, get, PATCH.
- **DONE:** status transitions OPEN, IN_PROGRESS, OVERDUE, CLOSED.
- **DONE:** overdue-detection scheduler.
- **DONE:** link and unlink a workflow instance.
- **BUILT (not audited):** filing requirements: create with dedup, list, get, status lifecycle and transitions.
- **BUILT (not audited):** obligation definitions with versions, retire, entity mappings (create, list, end).
- **PARTIAL:** applicability decisions (append-only, versioned, UNASSESSED kept distinct). Duplicates are appended, the value is not validated, and "exactly one of human or system" is not enforced.
- **DONE:** transactional outbox and relay (fixed 19 Oct). Not proven against a real broker.
- **DONE:** Idempotency-Key semantics, `If-Match`, RLS, object-level read authorization, `X-Legal-Entity-Id` reconciliation, full envelope on outbound authz.
- **PARTIAL:** legal-entity existence check. The header and body are reconciled and authorized, but not checked against the registry.
- **MISSING:** single owner of obligation data. obligation-tracking-svc and filing-tracker-svc also hold obligations and emit the same event names (C8).
- **MISSING:** WFC-05 deadline clocks:
  - clock classes; deadlines computed with calendar provenance; recalculation by supersession;
  - states DUE_SOON, DUE, FULFILLED, WAIVED, NOT_APPLICABLE, SUPERSEDED;
  - escalation schedule; `evaluate`/`satisfy` APIs; clock events.

**Required to make all features work**
- WF-3: the full WFC-05 deadline-clock model (16 rows; in scope by the 23 Oct decision).
- Idempotent applicability decisions, a CHECK on the value, and exactly one decider.
- Check the legal entity against tenant-entity-registry-svc (`GET /v1/entities/{id}/status`).
- C8: obligations-svc is the decided owner. Cross-team work removes the obligation-tracking-svc overlap.
- The 22 partial rows:
  - definition/instance split; `obligation.updated` scope; event and channel naming; partition key;
  - publish-failure metric; caller-supplied id collision; dead envelope resolver; invalid status should be 400;
  - Location header; cursor pagination (API-4);
  - openapi and Postman; safe store tests; URL escaping; compose `depends_on`; stale `progress.md`.
- Re-audit the new definition, mapping and filing-status routes.

## D2-5. authorization-svc (:8089)

**Features**
- **DONE:** `POST /v1/authorize`. Six layers in order, each later layer can only remove what an earlier one allowed: principal status, RBAC, delegation (intersected with the delegator's live grants), static SoD, own-object SoD, ABAC. Fails closed.
- **DONE:** SoD:
  - `/v1/sod/validate`, `/v1/sod/evaluate`, `/v1/sod/conflicting-permissions`;
  - SoD rules with maker-checker (approve, reject, retire, reactivate);
  - SoD exceptions (request, approve, reject, revoke).
- **DONE:** admin APIs:
  - roles and permission bundles (retire, reactivate);
  - role assignments (approve, reject, revoke, `effective_to`);
  - ABAC rules with maker-checker; authority limits (monetary; create, change, approve, reject, retire);
  - workload bindings.
- **DONE:** delegated-access evaluate; delegated-authority projection.
- **DONE:** privileged (JIT), break-glass and support sessions.
  - Request, approve (step-up MFA required for requester and approver), review, revoke, activity.
  - Maximums 8 h / 2 h / 4 h; over-long requests refused.
- **DONE:** access decision log (`/v1/access-decisions`) with `matched_grants`.
- **DONE:** available actions per resource; `/v1/me/capabilities`; cache invalidate; effective-access recompute (GOV-03 commands).
- **DONE:** identity-envelope verification (JWKS) on every non-evaluation route; mTLS workload gate; admin delegates by verified mTLS CN.
- **DONE:** event consumers for principal status, assignments and delegations, with cache invalidation.
- **PARTIAL:** ETag/If-Match (API-3). Session aggregates only.
- **MISSING:** ORG-06 role half: AssignBusinessRole, SuspendAssignment, ListPrincipalAssignments, GetAssignmentsAsOf (IAM-13).
- **MISSING:** quantity (non-monetary) authority limits (decided S8-D1).

**Required to make all features work**
- Finish API-3 on roles, bundles, assignments, SoD rules, ABAC rules, authority limits and workload bindings.
- Build IAM-13 and quantity limits.
- Security sign-offs: TOTP as the interim factor (SO-1); the session maximums (SO-2).
- Console:
  - forward `Authorization: Bearer <envelope>` on every call, then set `AUTHZ_IDENTITY_ASSERTION=require` (F-1);
  - MFA UI (F-2);
  - log in with client credentials (F-3).
- Every PEP gets a client certificate; then set `AUTHZ_EVALUATION_MTLS=require` (M-02).
- Calling services:
  - stop making tenantless `/v1/authorize` calls and send the preparer on own-object actions (X-1, X-2);
  - forward `X-Authn-Age-Seconds`, then set `AUTHZ_STEP_UP_UNKNOWN_AGE=step_up` (X-3).
- Delete the stale `docker-compose.override.yml`, which turns this service's mTLS off (P-1).
- Production grants by migration: `IDENTITY_*`, notification actions, workflow actions, `svc-policy`, `svc-access-control`.

## D2-6. workflow-svc (:8090)

**Features**
- **DONE:** create a workflow instance. Expands the definition's stages, checks entity status in tenant-entity-registry-svc, refuses a foreign tenant.
- **DONE:** get instance, next approver, pending approvals (`GET /v1/workflows/pending`), approval evidence (`GET /v1/workflows/{id}/evidence`).
- **DONE:** submit approve/reject action, request changes.
- **PARTIAL:** quorum votes. Quorum races are still open (L-2).
- **DONE:** escalate, escalate-to a target, cancel, invalidate, verify-release.
- **DONE:** expiry; deadline, escalation and approver-recheck timers; durable recovery of stuck instances.
- **DONE:** delegation checked through authorization-svc (`delegation_id` recorded); Idempotency-Key replay and mismatch.
- **DONE in code:** decision-log writes through a dedicated outbox destination; `decision_id` returned. No live ledger proof.
- **DONE:** lineage on decisions and transitions: `access_decision_id`, `authority_basis`, `delegation_id`.
- **PARTIAL:** consumed events (policy, delegated-authority, authorization) trigger approver re-checks. Tenantless global `policy.*` events are skipped.
- **PARTIAL:** workflow definitions: create, list, get, supersede, activate, suspend, retire. Instances are not pinned to a definition version, and the definitions table has no RLS.
- **DONE:** identity-envelope gate (signed envelope overrides gateway headers; compose `IDENTITY_VERIFY=warn`).
- **DONE:** ETag/If-Match on 11 mutations (7 instance, 4 definition).
- **BUILT (not in the Group 2 audit):** audit engagement module: engagements, acceptance, scope, completion gates, plans, materiality, risks, assertions, workpapers, reviews, review notes, sign-offs, quality reviews, release gates.
- **BUILT (not in the Group 2 audit):** forms module: forms, publish/retire, drafts, submissions, validate, supersede, route to domain.
- **MISSING:** WFC-04 cases (WF-2); state-machine definition registry, available actions and transition history, no generic status writes (ST-1 … ST-3).

**Required to make all features work**
- access-control-svc (H-1, H-2):
  - registers `WORKFLOW_CANCEL`, `WORKFLOW_ESCALATE`, `WORKFLOW_INVALIDATE`, `WORKFLOW_ADMINISTER`;
  - a role template carries `WORKFLOW_APPROVE`.
- Set `WORKFLOW_LEDGER_PRINCIPAL_ID`, grant it `GOVERNANCE_DECISION_RECORD`, and run once against a real governance-decision-log-svc.
- Handle tenantless global `policy.*` events, so a global policy change re-checks every tenant.
- Handler tests for: ROLE stages, definition expansion, escalation-target approval, registry 404/503, Idempotency-Key replay/mismatch, identity-gate override.
- The 25 partial rows:
  - quorum stage lock and `row_version` (L-2);
  - RLS on stages, transitions, quorum tables, outbox and definitions (L-8);
  - per-workflow-type approve authority (Q-6);
  - split state model; common columns; event naming (Q-3).
- Business-object owners call `/invalidate` or emit change events (H-5). invoice-approval-svc calls a path that does not exist (N-3).
- Reporting-line and team data for approver independence (H-6). A client certificate for authorization-svc `require` mode (H-7).
- Decisions Q-1 … Q-6 (WFC scope, binding API surface, event catalogue, consumed-event behaviour, service-account vs human approver, approve action codes).
- Plan tasks WF-1, WF-2, ST-1 … ST-3.
- Root-context Docker image build and live E2E (not run).

## D2-7. evidence-requirements-svc (:8130)

Re-checked against the code on 5 Nov 2026 (completion plan ER-0 … ER-9; file:line as of that day).

**Features**
- **DONE:** evaluate (`POST /v1/evidence/evaluate`): SATISFIED / MISSING / NO_REQUIREMENTS_DEFINED; fails closed on a malformed rule.
- **DONE:** get an evaluation; evaluation register `GET /v1/evidence/evaluations` (API-4); summary `GET /v1/evidence/summary`.
- **DONE:** requirement catalogue: list (API-4), get (ETag), create (idempotent), end-date (If-Match).
- **DONE:** supporting-artifact rules (subtype, minimum count), data-driven.
- **DONE:** append-only decision ledger; outbox events `evidence.requirement.satisfied` / `.missing`, proven on Kafka (one record per decision, none for replays or NO_REQUIREMENTS_DEFINED). Fixed in this pass: the outbox refused every event under the real runtime role (migration 000011).
- **DONE:** PBC requests (14 routes); every manager command needs If-Match; register paged and authorized.
- **DONE (was MISSING):** document verification against document-vault-svc, envelope forwarded (`internal/documentvault/client.go`; live E2E §6).
- **DONE (was PARTIAL):** signature verification against esignature-integration-svc (`internal/esignature/client.go`). Live SATISFIED not shown: that service runs only in the phase7 stack.
- **DONE (was MISSING):** distinct artifacts by canonical reference (`handler.go:516`; `TestER02_SameManifestTwiceCountsOnce`).
- **DONE (was PARTIAL):** an empty `reference_id` never counts; EVIDENCE_MANIFEST refs checked against evidence-manifest-svc. A real manifest cannot count yet: `EVIDENCE_MANIFEST_READ` is not registered, so the owner refuses every read (fails closed).
- **DONE (was MISSING, decision):** authorization on evaluate (EVIDENCE_EVALUATE; compose warn, D-ER-6) and `openapi.yaml` 1.4.0 with parity test.
- **MISSING (callers):** gate coverage for general-ledger post, financial-close lock, vat-gst and corporate-tax.
- **MISSING (callers):** board-resolutions, corporate-actions and filing-preparation send an incomplete envelope; corporate-actions and filing-preparation pass any outcome but MISSING (fail open).
- **PARTIAL:** `GET /v1/pbc/requests/{id}` is still authorized by tenant only.

**Still required**
- Cross-team: the four missing caller gates; the full envelope from the three callers; an allow-list (only SATISFIED passes).
- evidence-manifest-svc: register `EVIDENCE_MANIFEST_READ` in access-control and a template, so manifests can count.
- Decisions recorded, not taken: tier (D-ER-1), scan-result ownership (D-ER-2), which document/signature statuses count (D-ER-3).
- Switch compose evaluate/identity gates to enforce once the callers send envelopes.

## Domain 2: cross-cutting requirements

- **X5, signed identity everywhere:** ForwardAuth on the local Traefik routes, remove published service ports, and every service verifies the envelope.
- **X3, real Kafka brokers:** every outbox is coded and retry-tested, but none is proven end to end against a broker.
- **Group decisions:**
  - Decided: C3 keep wire names and add canonical names alongside (R9); C8 obligations-svc is the owner; WFC-05 is in scope.
  - Open: C4, which API surface binds.
- **API contract:** finish API-3 in authorization-svc. API-4 done in evidence-requirements-svc (5 Nov); elsewhere API-4, API-5 and API-6 are TODO.
- **Plan tasks:**
  - G-07 evidence ledger; G-08 schema registry lifecycle; G-09/G-10 retention and legal hold (retention-registry-svc); G-05/03/04/12 named-operation parity;
  - WF-1 … WF-4, ST-1 … ST-3, REF-1 … REF-3, JUR-1.
- **Production grants:** every grant comes from a migration, not from `seed-demo-rbac.ps1`.
- **Live verification:** no Docker images or live compose acceptance since 19–21 Oct, and no CI run.
- **Console:** screens for the new capabilities (FE-1).
