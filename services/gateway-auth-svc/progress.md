# gateway-auth-svc — progress and feature matrix

Status: baseline green (2026-11-04, GW-0).

The ForwardAuth callout behind Traefik (:8092). No database. Every state is
in-memory on this replica; the admin read surface states its scope.

## Feature matrix (GW-0)

Rows from ZS-SVC-M-001 INT-01 §4.1 (as enumerated in AUTH-PLANE-PLAN GW-0: the
docx set carries only the catalogue row, so the plan's task list is the
authority — recorded as a deviation note), ZS-SEC-001 §8/§9, GOV-01 §4 ingress
duties, D1-5, and the audit rows AU-01/04/12/23/27.

| ID | requirement | route/consumer | test | console screen | status |
|---|---|---|---|---|---|
| INT01-01 | Ownership: gateway decision + routed telemetry authoritative here | /verify + admission Recorder | handler/admission suites | Overview KPIs | DONE |
| INT01-02 | Runtime op: authenticate one request (envelope verify) | Traefik ForwardAuth → /verify | TestVerify_* (handler_test) | Verify my session tab | DONE |
| INT01-03 | Runtime op: admission (shape + version + in-flight cap) | /verify admission stage | TestPolicy*, TestGate*, TestINT01_RequestSizeAbuse | Overview pipeline | DONE (GW-1 completes) |
| INT01-04 | Runtime op: throttle by client and tenant, 429 + Retry-After | IPLimiter/TenantLimiter | TestLimiter*, TestRateLimitedResponseIsProblemWithRetryAfter, TestAU04_* (GW-2) | Rate limits tab | DONE (GW-2 adds login bucket) |
| INT01-05 | Runtime op: route with a resolved version and region | version gate + GTRM headers | TestPolicyVersionAdmission_INT_NP_003, TestINT01_RouteConfigRollback, TestINT01_ResidencyDenied | Versions & policy tab | PARTIAL → GW-1 |
| INT01-06 | Server-resolved inputs: tenant, rate limit, routing, authorization, environment | resolver + admission + GTRM headers | TestVerify_* , TestTenantQuotaAppliesAfterAuthentication | Overview | DONE |
| INT01-07 | Five queries: decision by id, decisions by outcome/tenant, usage, rate-limit state, route version status | admin reads | admin_test suite; GW-4 adds decisions list + policy | Registers (tabs) | DONE (GW-4 adds policy read) |
| INT01-08 | Request state machine: received → validating → admitted / denied / rate-limited → forwarded | handler pipeline | TestValidCredentialIsAdmittedAndCounted_INT_NP_002, TestEveryAuthRefusalCarriesAProblemBody | Overview pipeline | DONE |
| INT01-09 | Event api_request_denied | refuse() default event | TestAbuseThresholdRaisesOneEventPerClientPerWindow, TestINT01_ResidencyDenied | Denied tab | DONE (GW-1 completes) |
| INT01-10 | Event api_rate_limited | limiter refusals | TestTenantRateLimitStreamsOneThrottledCanonicalEvent, TestAU04_* | Rate limits tab | DONE |
| INT01-11 | Event api_route_version_unavailable | version gate refusal | TestUnsupportedAPIVersionRefusedWithoutFallback_INT_NP_003 | Versions tab | DONE |
| INT01-12 | Event api_abuse_threshold_exceeded | Recorder abuse detection | TestAbuseThresholdRaisesOneEventPerClientPerWindow | Overview callout | DONE |
| INT01-13 | Idempotency: decision reads replay identically; decisions retrievable by id | admin GET decisions/{id} | TestDecisionLookupAndList | Denied tab drawer | DONE |
| INT01-14 | Failure semantics: dependency down distinguishable from caller error | JWKS/registry/CARTA paths | TestVerify_JWKSOutageIsNotReportedAsABadToken, TestVerify_RegistryUnavailableBlocksWriteWith503, TestINT01_DownstreamTimeoutDistinguishable (GW-1) | Overview | PARTIAL → GW-1 |
| INT01-M1 | MAT forged tenant header refused | /verify stages 1/4 | TestINT01_ForgedTenantHeader (GW-1) | Denied tab | PARTIAL → GW-1 |
| INT01-M2 | MAT valid token, downstream denied — distinguishable | /verify 200 + backend 403 | TestGatewayAdmitsAndTheDomainRefusesWithBothDecisionsRetained_INT_NP_002, TestINT01_ValidTokenDownstreamDenied (GW-1) | Denied tab | PARTIAL → GW-1 |
| INT01-M3 | MAT version unsupported, no fallback | version gate | TestINT01_VersionUnsupported (GW-1) | Versions tab | PARTIAL → GW-1 |
| INT01-M4 | MAT request-size abuse refused before auth | Policy.Check | TestPolicyRequestSize_INT_NP_004, TestINT01_RequestSizeAbuse (GW-1) | Overview | PARTIAL → GW-1 |
| INT01-M5 | MAT rate-limit race never exceeds burst | IPLimiter concurrent | TestLimiterConcurrentRequestsCannotBypassQuota_INT_NP_005, TestINT01_RateLimitRace (GW-1) | Rate limits tab | PARTIAL → GW-1 |
| INT01-M6 | MAT route config rollback restores versions | policy reload | TestINT01_RouteConfigRollback (GW-1, new) | Versions tab | PENDING → GW-1 |
| INT01-M7 | MAT residency mismatch denied on a material request; fail closed on unknown (write) | GOV-01 resolve + GTRM region header | TestINT01_ResidencyDenied (GW-1, new check) | Denied tab | PENDING → GW-1 |
| INT01-M8 | MAT downstream timeout distinguishable | dependency timeouts | TestINT01_DownstreamTimeoutDistinguishable (GW-1, new) | Overview | PENDING → GW-1 |
| SEC8-E1 | Internet edge: DDoS/WAF/bot/abuse/TLS/request-size | GCP LB + Traefik middlewares (rate limit, buffering) | — | — | BLOCKED: platform decision S5-D1; placement documented in SPEC_DEVIATIONS |
| SEC8-I1 | Ingress: authenticated routes, hostname/tenant resolution, header sanitation | edge strip + /verify + tenant binding | TestEdgeContract_*, TestVerify_ResolvedTenantMismatch | Trusted-header policy panel | DONE |
| SEC8-S2 | Service-to-service mTLS, unique workload identity | :8449 listeners + internal/mtls | mtls_test suite, TestLoad_MTLS* | Versions & policy tab | DONE |
| SEC8-RT | Runtime: request-size limits, compressed-body refusal | admission Policy | TestPolicyAllowedEncodingStillCapped, TestPolicyRefusesCompressedBodiesByDefault | Versions & policy tab | DONE |
| GOV01-I1 | Ingress stamps server-resolved context; caller cannot override | /verify header half | TestVerify_SourceChannelFromEnvelopeOverridesClient, TestVerify_AssuranceLevelFromSessionTrust | Trusted-header policy panel | DONE |
| GOV01-I2 | Hostname ↔ tenant binding, no fallback | X-Zoiko-Resolved-Tenant-Id vs token claim | TestVerify_ResolvedTenantMismatch_Returns403 | Overview | DONE |
| GOV01-I3 | Tenant operability + entity-in-tenant check | tenantctx resolver | TestVerify_SuspendedTenantDenied, TestVerify_CrossTenantEntityDenied | Tenant usage tab | DONE |
| GOV01-I4 | Cache: fresh/stale/invalidated, bounded TTL | tenantctx cache | TestVerify_ResolutionIsCached | Versions & policy tab | DONE |
| D1-5a | /verify: envelope verify, issuer/audience pinned, incomplete claims refused | /verify | TestVerify_WrongAudience_Returns401, TestVerify_MissingPrincipalID_Returns401 | Verify my session tab | DONE |
| D1-5b | Revocation within TTL (AU-01) | identity introspection | TestVerify_RevokedSessionIsRefused | Overview | DONE |
| D1-5c | CARTA risk gate; STEP_UP_MFA → 401 step_up_required (AU-23); unreachable → closed writes / open reads (S5-D2) | carta stage | TestVerify_Carta* , TestVerify_CartaUnreachable_ClosedOnWritesOpenOnReads | Overview callout | DONE |
| D1-5d | SIEM denial stream + X-Auth-Denial-Reason | refuse()/dedicated events | TestVerify_TenantHostnameMismatchStreamsToSIEM | Denied tab | DONE |
| D1-5e | Assurance headers: X-Assurance-Level, X-Authn-Age-Seconds, source channel, support context | setSessionContext | TestVerify_AuthnAgeFromAuthTime, TestVerify_SupportContextOnlyFromEnvelope | Trusted-header policy panel | DONE |
| D1-5f | mTLS to identity-context + tenant registry (SEC-INV-05) | :8449 + mtls clients | TestLoad_MTLSEnabledSwitchesPeerURLs | Versions & policy tab | DONE |
| AU-04 | Source-level login throttling; 25 failed logins → 429 before the 25th | login-rate bucket (GW-2) | TestAU04_LoginRoutesUseStricterBucket, TestAU04_ThrottleKeyedOnRealClientIP; live 25-login probe | Rate limits tab | PENDING → GW-2 |
| S5-D3 | Client IP behind the GCP LB: trusted-hop design | clientIP + trusted CIDRs (GW-2) | TestClientIP_UntrustedPeerXFFIgnored, TestClientIP_TrustedChainTakesFirstUntrusted | Versions & policy tab | PENDING → GW-2 |
| S3-D1 | X-Commercial-Plan / X-Org-Unit-Id edge-set, stripped from clients | plan lookup (GW-3) + strip lists | TestS3D1_* | Trusted-header policy panel | PENDING → GW-3 |
| AU-12 | Per-service audience; token exchange at the gateway (IAM-1) | GW-5 exchange | TestVerify_ExchangesForRouteAudience, TestIAM01_EnvelopeForPolicyRefusedByWorkflow | Overview | PENDING → GW-5 |
| AU-27 | Sender-constrained envelopes (DPoP/mTLS binding) | — | — | — | BLOCKED: Security sign-off (D-7); interface note in SPEC_DEVIATIONS |
| S5-B2 | Console → service calls bypass ForwardAuth | audience exchange + per-service gates (GW-5) | TestVerify_UnknownRouteFailsClosed | — | PENDING → GW-5 (exchange half); console routing stays frontend follow-up |
| S4-B1 | Traefik serversTransport client certs for edge → service hop | — | — | — | BLOCKED: infrastructure (mtls-management-svc enrolment) |
| R-1 console | Human login gets a verified channel | console client at login (done 3 Nov) | E2E-identity-context | — | DONE (IC-FE-1) |
| GW-4 | Admin read surface enabled in compose | GATEWAY_ADMIN_ADDR :8192 | admin_test + compose probe | all tabs | PENDING → GW-4 |
| GW-5 | Audience exchange + rollout mode | ForwardTo/Exchanger | TestVerify_ExchangeFailureIs503NotPassThrough | Overview | PENDING → GW-5 |

## Decisions taken

- (none yet)

## GW-0 — Baseline and feature matrix (2026-11-04)

**Suite:** `docker run --rm --memory=1400m -e GOFLAGS=-mod=mod -e GOPROXY=off
-e CGO_ENABLED=0 -v <repo>:/repo -v <modcache>:/go/pkg/mod
-w /repo/services/gateway-auth-svc golang:1.26-alpine go test -p 2 ./...`
(repo root mounted, not services/ — the edge-contract tests read
`../../../../deployments/`). Result: ok admin, admission, config, handler,
jwks, mtls, problem, siem (8 packages); 6 packages without test files; 0 fail.
One pre-existing WARN line (canonical input contract on the metrics probe) is
noise from TestRouter_ExposesMetrics, not a failure.

**Stale text fixed:** openapi.yaml /verify description, RELEASE_CERTIFICATE.md
"Not done" list, internal/telemetry/telemetry.go CartaDecisionsTotal comment and
internal/handler/handler.go DomainMetrics.CartaDecision comment — all four said
STEP_UP_MFA is not enforced; the code (and
TestVerify_CartaStepUpMFA_Returns401StepUpRequired +
TestVerify_CartaStepUpMFA_Returns401AndCounted) answers 401 step_up_required.
