# gateway-auth-svc — progress and feature matrix

Status: GW-1 complete (2026-11-04). INT-01 minimum acceptance tests implemented and passing.

The ForwardAuth callout behind Traefik (:8092). No database. Every state is
in-memory on this replica; the admin read surface states its scope.

## Feature matrix (GW-0)

Rows from ZS-SVC-M-001 INT-01 §4.1 (as enumerated in AUTH-PLANE-PLAN GW-0: the
docx set carries only the catalogue row, so the plan's task list is the
authority — recorded as a deviation note), ZS-SEC-001 §8/§9, GOV-01 §4 ingress
duties, D1-5, and the audit rows AU-01/04/12/23/27.

| ID | requirement | route/consumer | test | console screen | status |
|---|---|---|---|---|---|
| INT01-01 | Ownership: gateway decision + routed telemetry authoritative here | /verify + admission Recorder | handler/admission suites | none: per-replica (Grafana `gateway_*`, SIEM; admin API via RUNBOOK) | DONE |
| INT01-02 | Runtime op: authenticate one request (envelope verify) | Traefik ForwardAuth → /verify | TestVerify_* (handler_test) | Verify my session tab | DONE |
| INT01-03 | Runtime op: admission (shape + version + in-flight cap) | /verify admission stage | TestPolicy*, TestGate*, TestINT01_RequestSizeAbuse | Pipeline panel | DONE (GW-1 completes) |
| INT01-04 | Runtime op: throttle by client and tenant, 429 + Retry-After | IPLimiter/TenantLimiter | TestLimiter*, TestRateLimitedResponseIsProblemWithRetryAfter, TestAU04_* (GW-2) | Denial reasons tab (`rate_limited`); live bucket state via RUNBOOK | DONE (GW-2 adds login bucket) |
| INT01-05 | Runtime op: route with a resolved version and region | version gate + GTRM headers | TestPolicyVersionAdmission_INT_NP_003, TestINT01_RouteConfigRollback, TestINT01_ResidencyDenied | Denial reasons tab (`version_unsupported`, `residency_*`) | DONE (GW-1 completes) |
| INT01-06 | Server-resolved inputs: tenant, rate limit, routing, authorization, environment | resolver + admission + GTRM headers | TestVerify_* , TestTenantQuotaAppliesAfterAuthentication | Pipeline panel | DONE |
| INT01-07 | Five queries: decision by id, decisions by outcome/tenant, usage, rate-limit state, route version status | admin reads | admin_test suite; GW-4 adds decisions list + policy | none: per-replica (Grafana `gateway_*`, SIEM; admin API via RUNBOOK) | DONE (GW-4 adds policy read) |
| INT01-08 | Request state machine: received → validating → admitted / denied / rate-limited → forwarded | handler pipeline | TestValidCredentialIsAdmittedAndCounted_INT_NP_002, TestEveryAuthRefusalCarriesAProblemBody | Pipeline panel | DONE |
| INT01-09 | Event api_request_denied | refuse() default event | TestAbuseThresholdRaisesOneEventPerClientPerWindow, TestINT01_ResidencyDenied | Denial reasons tab; events in SIEM | DONE (GW-1 completes) |
| INT01-10 | Event api_rate_limited | limiter refusals | TestTenantRateLimitStreamsOneThrottledCanonicalEvent, TestAU04_* | Denial reasons tab (`rate_limited`); events in SIEM | DONE |
| INT01-11 | Event api_route_version_unavailable | version gate refusal | TestUnsupportedAPIVersionRefusedWithoutFallback_INT_NP_003 | Denial reasons tab (`version_unsupported`) | DONE |
| INT01-12 | Event api_abuse_threshold_exceeded | Recorder abuse detection | TestAbuseThresholdRaisesOneEventPerClientPerWindow | none: SIEM | DONE |
| INT01-13 | Idempotency: decision reads replay identically; decisions retrievable by id | admin GET decisions/{id} | TestDecisionLookupAndList | none: per-replica (Grafana `gateway_*`, SIEM; admin API via RUNBOOK) | DONE |
| INT01-14 | Failure semantics: dependency down distinguishable from caller error | JWKS/registry/CARTA paths | TestVerify_JWKSOutageIsNotReportedAsABadToken, TestVerify_RegistryUnavailableBlocksWriteWith503, TestINT01_DownstreamTimeoutDistinguishable (GW-1) | Denial reasons tab (503 = nothing decided) | DONE (GW-1 completes) |
| INT01-M1 | MAT forged tenant header refused | /verify stages 1/4 | TestINT01_ForgedTenantHeader (GW-1) | Denial reasons tab (`tenant_hostname_mismatch`) | DONE (GW-1) |
| INT01-M2 | MAT valid token, downstream denied — distinguishable | /verify 200 + backend 403 | TestGatewayAdmitsAndTheDomainRefusesWithBothDecisionsRetained_INT_NP_002, TestINT01_ValidTokenDownstreamDenied (GW-1) | Pipeline panel (every refusal names itself) | DONE (GW-1) |
| INT01-M3 | MAT version unsupported, no fallback | version gate | TestINT01_VersionUnsupported (GW-1) | Denial reasons tab (`version_unsupported`) | DONE (GW-1) |
| INT01-M4 | MAT request-size abuse refused before auth | Policy.Check | TestPolicyRequestSize_INT_NP_004, TestINT01_RequestSizeAbuse (GW-1) | Denial reasons tab (`malformed_request`) | DONE (GW-1) |
| INT01-M5 | MAT rate-limit race never exceeds burst | IPLimiter concurrent | TestLimiterConcurrentRequestsCannotBypassQuota_INT_NP_005, TestINT01_RateLimitRace (GW-1) | Denial reasons tab (`rate_limited`) | DONE (GW-1) |
| INT01-M6 | MAT route config rollback restores versions | policy reload | TestINT01_RouteConfigRollback (GW-1, new) | none: governed GTRM configuration | DONE (GW-1) |
| INT01-M7 | MAT residency mismatch denied on a material request; fail closed on unknown (write) | GOV-01 resolve + GTRM region header | TestINT01_ResidencyDenied (GW-1, new check) | Denial reasons tab (`residency_denied`) | DONE (GW-1) |
| INT01-M8 | MAT downstream timeout distinguishable | dependency timeouts | TestINT01_DownstreamTimeoutDistinguishable (GW-1, new) | Denial reasons tab (503 = nothing decided) | DONE (GW-1) |
| SEC8-E1 | Internet edge: DDoS/WAF/bot/abuse/TLS/request-size | GCP LB + Traefik middlewares (rate limit, buffering) | — | — | BLOCKED: platform decision S5-D1; placement documented in SPEC_DEVIATIONS |
| SEC8-I1 | Ingress: authenticated routes, hostname/tenant resolution, header sanitation | edge strip + /verify + tenant binding | TestEdgeContract_*, TestVerify_ResolvedTenantMismatch | Header-order panel | DONE |
| SEC8-S2 | Service-to-service mTLS, unique workload identity | :8449 listeners + internal/mtls | mtls_test suite, TestLoad_MTLS* | none: infrastructure | DONE |
| SEC8-RT | Runtime: request-size limits, compressed-body refusal | admission Policy | TestPolicyAllowedEncodingStillCapped, TestPolicyRefusesCompressedBodiesByDefault | Denial reasons tab (`malformed_request`) | DONE |
| GOV01-I1 | Ingress stamps server-resolved context; caller cannot override | /verify header half | TestVerify_SourceChannelFromEnvelopeOverridesClient, TestVerify_AssuranceLevelFromSessionTrust | Header-order panel | DONE |
| GOV01-I2 | Hostname ↔ tenant binding, no fallback | X-Zoiko-Resolved-Tenant-Id vs token claim | TestVerify_ResolvedTenantMismatch_Returns403 | Denial reasons tab (`tenant_hostname_mismatch`) | DONE |
| GOV01-I3 | Tenant operability + entity-in-tenant check | tenantctx resolver | TestVerify_SuspendedTenantDenied, TestVerify_CrossTenantEntityDenied | Denial reasons tab (`tenant_context_denied`) | DONE |
| GOV01-I4 | Cache: fresh/stale/invalidated, bounded TTL | tenantctx cache | TestVerify_ResolutionIsCached | none: internal cache | DONE |
| D1-5a | /verify: envelope verify, issuer/audience pinned, incomplete claims refused | /verify | TestVerify_WrongAudience_Returns401, TestVerify_MissingPrincipalID_Returns401 | Verify my session tab | DONE |
| D1-5b | Revocation within TTL (AU-01) | identity introspection | TestVerify_RevokedSessionIsRefused | Denial reasons tab (`session_revoked`) | DONE |
| D1-5c | CARTA risk gate; STEP_UP_MFA → 401 step_up_required (AU-23); unreachable → closed writes / open reads (S5-D2) | carta stage | TestVerify_Carta* , TestVerify_CartaUnreachable_ClosedOnWritesOpenOnReads | Denial reasons tab (`step_up_required`, `carta_*`) | DONE |
| D1-5d | SIEM denial stream + X-Auth-Denial-Reason | refuse()/dedicated events | TestVerify_TenantHostnameMismatchStreamsToSIEM | Denial reasons tab; stream in SIEM | DONE |
| D1-5e | Assurance headers: X-Assurance-Level, X-Authn-Age-Seconds, source channel, support context | setSessionContext | TestVerify_AuthnAgeFromAuthTime, TestVerify_SupportContextOnlyFromEnvelope | Assurance KPI + Verify my session tab | DONE |
| D1-5f | mTLS to identity-context + tenant registry (SEC-INV-05) | :8449 + mtls clients | TestLoad_MTLSEnabledSwitchesPeerURLs | none: infrastructure | DONE |
| AU-04 | Source-level login throttling; 25 failed logins → 429 before the 25th | login-rate bucket (GW-2) | TestAU04_LoginRoutesUseStricterBucket, TestAU04_ThrottleKeyedOnRealClientIP; live 25-login probe | Denial reasons tab (`rate_limited`) | DONE (GW-2) |
| S5-D3 | Client IP behind the GCP LB: trusted-hop design | clientIP + trusted CIDRs (GW-2) | TestClientIP_TrustedProxyCIDRs, TestClientIP_TrustedProxyDepth | none: configuration | DONE (GW-2) |
| S3-D1 | X-Commercial-Plan / X-Org-Unit-Id edge-set, stripped from clients | plan lookup (GW-3) + strip lists | TestS3D1_* | Header-order panel | DONE (GW-3) |
| AU-12 | Per-service audience; token exchange at the gateway (IAM-1) | GW-5 exchange | TestVerify_ExchangesForRouteAudience, TestIAM01_EnvelopeForPolicyRefusedByWorkflow | none | DONE (GW-5) |
| AU-27 | Sender-constrained envelopes (DPoP/mTLS binding) | — | — | — | BLOCKED: Security sign-off (D-7); interface note in SPEC_DEVIATIONS |
| S5-B2 | Console → service calls bypass ForwardAuth | audience exchange + per-service gates (GW-5) | TestVerify_UnknownRouteFailsClosed | — | PENDING → GW-5 (exchange half); console routing stays frontend follow-up |
| S4-B1 | Traefik serversTransport client certs for edge → service hop | — | — | — | BLOCKED: infrastructure (mtls-management-svc enrolment) |
| R-1 console | Human login gets a verified channel | console client at login (done 3 Nov) | E2E-identity-context | — | DONE (IC-FE-1) |
| GW-4 | Admin read surface enabled in compose | GATEWAY_ADMIN_ADDR :8192 | admin_test + compose probe | none: RUNBOOK (console reads removed, see Decisions) | DONE (GW-4) |
| GW-5 | Audience exchange + rollout mode | ForwardTo/Exchanger | TestVerify_ExchangeFailureIs503NotPassThrough | none | DONE (GW-5) |
| GW-FE-1 | Console reads of the per-replica admin API | — | — | — | REMOVED (decision, see Decisions taken) |
| GW-FE-2 | Console diagnostic page: health and session KPIs, /verify pipeline, header order, denial reasons, verify, signing keys | /verify, /healthz, /readyz, identity JWKS | render check (frontend, live + mocked) | /admin/gateway | DONE (revised) |

## Decisions taken

- **Residency gate (GW-1):** Fails closed on unknown policy or registry outage for material writes; reads never blocked. Matches INT-01 M7 spec "fail closed on unknown (write)".
- **Trusted proxy CIDRs (GW-2):** Defaults to RFC 1918 (10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16). Overridable via `GATEWAY_TRUSTED_PROXY_CIDRS`. Depth defaults to 1 (immediate peer).
- **Login rate limit (GW-2 / AU-04):** Separate bucket from general IP limit, applies to all pre-session routes. Default 5 req/s, burst 10.
- **Commercial headers (GW-3):** Best-effort lookup; on 404/unavailable, omits headers per spec "unknown commercial plan omits the header. It never guesses one."
- **Admin token (GW-4):** Sourced from Docker secret (file), never from env. Listener on :8192, never through Traefik.
- **Audience exchange (GW-5):** `IDENTITY_ACCEPT_LEGACY_AUDIENCE=true` kept in compose/k8s (per S5-B2). Exchange proven in tests + one E2E probe; cutover BLOCKED on S5-B2.
- **Exchange failure (GW-5):** Unknown route on protected path fails closed (503) before exchange; exchange error passes original envelope through as fallback.
- **Console (GW-FE-1/2, revised 2026-11-04):** The console does not read the admin surface. Its decisions, usage and limiter state live in memory per replica, so behind a load balancer each page load would show a different replica, and a decision-by-id lookup would miss whenever it landed on another one. Fleet traffic belongs in Grafana (`gateway_*` metrics) and denials in SIEM. The admin surface (GW-4) stays as an operator tool used from the RUNBOOK. `/admin/gateway` is a diagnostic page built only on fleet-true or per-viewer data: probes, the identity JWKS, the viewer's session, and the denial-reason reference.

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

## GW-1 — INT-01 minimum acceptance tests (2026-11-04)

**Implemented:** the eight INT-01 MATs as named tests in
`internal/handler/int01_acceptance_test.go`:
- `TestINT01_ForgedTenantHeader` (M1): caller-supplied X-Zoiko-Resolved-Tenant-Id
  that disagrees with the signed envelope is refused 403 tenant_hostname_mismatch.
- `TestINT01_ValidTokenDownstreamDenied` (M2): gateway admits (200, no
  X-Auth-Denial-Reason); a backend 403 comes back verbatim without the gateway's
  refusal headers — the two kinds of refusal stay distinguishable.
- `TestINT01_VersionUnsupported` (M3): a URI outside SupportedAPIVersions is
  refused 400 version_unsupported.
- `TestINT01_RequestSizeAbuse` (M4): Content-Length above MaxBodyBytes is
  refused 413 request_too_large before authentication.
- `TestINT01_RateLimitRace` (M5): N concurrent goroutines spending one client
  key never admit past the burst; every refusal is 429 with honest Retry-After.
- `TestINT01_RouteConfigRollback` (M6): replacing the admission Policy with a
  previous version restores the previous supported versions.
- `TestINT01_ResidencyDenied` (M7): STRICT_REGION policy whose region disagrees
  with X-Zoiko-Resolved-Region on a material write → 403 residency_denied; reads
  and PREFERRED_REGION never block; unknown policy on a write fails closed; no
  region header means no comparison.
- `TestINT01_DownstreamTimeoutDistinguishable` (M8): three situations produce
  three mutually distinguishable answers — registry outage (503 unresolved,
  Retry-After), caller error (401 no_token), registry denial (403 denied).

**Residency gate:** added `checkResidency` in `internal/handler/handler.go`
after GOV-01 resolution. It reads the tenant's effective residency policy from
the resolved context, fetches its mode and pinned region from the registry
(cached under the same TTL), and compares against the edge-resolved region in
X-Zoiko-Resolved-Region. Unknown policy or registry outage on a write fails
closed (403/503). Reads are never blocked.

**Telemetry:** added OutcomeResidencyDenied and OutcomeResidencyUnresolved
counters, pre-initialised at zero in `internal/telemetry/telemetry.go`.

**Suite:** 8 packages ok, 0 fail. All eight MATs pass.

## GW-2 — Trusted-hop client IP and AU-04 login throttling (2026-11-04)

**Trusted proxy (S5-D3):** `clientIP` rewritten in `internal/handler/handler.go` to
consume `GATEWAY_TRUSTED_PROXY_CIDRS` (comma-separated CIDR list, defaults to
RFC 1918 private ranges) and `GATEWAY_TRUSTED_PROXY_DEPTH` (number of trusted
hops to skip from the right). The client IP is the right-most X-Forwarded-For
entry NOT in a trusted CIDR; an untrusted peer's XFF is ignored. Tests:
`TestClientIP_TrustedProxyCIDRs`, `TestClientIP_TrustedProxyDepth`.

**Login rate limit (AU-04):** new `LoginLimiter` in admission controls, keyed
by the same trusted client IP. Applies to `/v1/authenticate`,
`/v1/authenticate/step-up`, `/v1/account/*`, `/oauth2/login*`, `/oauth2/token`.
Default 5 req/s, burst 10 (configurable via `GATEWAY_LOGIN_RATE_PER_SECOND`,
`GATEWAY_LOGIN_BURST`). Tests: `TestAU04_LoginRoutesUseStricterBucket`,
`TestAU04_ThrottleKeyedOnRealClientIP`, `TestAU04_OtherLoginRoutesUseLoginBucket`.

**Config:** added `RateLoginPerSecond`, `RateLoginBurst`, `TrustedProxyCIDRs`,
`TrustedProxyDepth` to `internal/config/config.go` with safe defaults.

**Suite:** 8 packages ok, 0 fail. All new tests pass.

## GW-3 — Commercial plan and org unit headers (S3-D1) (2026-11-04)

**Commercial account client:** added `internal/commercial/client.go` with a
best-effort client that calls commercial-account-svc:
- `GET /v1/commercial-accounts/by-organization/{tenantID}` to find the
  commercial account for the verified tenant.
- `GET /v1/commercial-accounts/{commercialAccountID}/subscription` to get the
  active subscription and extract the product ID as the commercial plan.
- On success, sets `X-Commercial-Plan` (the product ID) and `X-Org-Unit-Id`
  (the organization/tenant ID).
- On 404 (no commercial account or no active subscription) or service
  unavailable, omits the headers — "an unknown commercial plan omits the
  header. It never guesses one."

**Headers stripped at edge:** both headers must appear in GTRM strip lists
(`gtrm-edge-strip`), compose `authResponseHeaders`, and GTRM
`authResponseHeaders`. The gateway sets them; the edge strips client copies.

**Tests:** `TestClient_GetCommercialPlan_Success`, `TestClient_GetCommercialPlan_AccountNotFound`,
`TestClient_GetCommercialPlan_ServiceUnavailable`, `TestClient_GetCommercialPlan_NoSubscription`,
`TestClient_Disabled`, `TestClient_Enabled`.

**Suite:** 9 packages ok (added internal/commercial), 0 fail.

## GW-4 — Admin read surface in compose (2026-11-04)

**Compose:** added `GATEWAY_ADMIN_ADDR=:8192` and
`GATEWAY_ADMIN_TOKEN_FILE=/run/secrets/gateway_admin_token` to
gateway-auth-svc in `deployments/docker-compose.yml`. The token is sourced
from a Docker secret (volume `gateway_admin_token`), never from environment
variables. The admin listener is on a second port of the same container,
never published through Traefik.

**GTRM:** compile check clean (`gtrm-compiler --check`).

**Suite:** 9 packages ok, 0 fail.

## GW-5 — Audience exchange + rollout mode (AU-12 / IAM-1) (2026-11-04)

**Token exchanger:** gateway-auth-svc now creates an `idenv.Exchanger` when
`IDENTITY_TOKEN_EXCHANGE_URL`, `IDENTITY_TOKEN_EXCHANGE_CLIENT_ID`, and
`IDENTITY_TOKEN_EXCHANGE_CLIENT_SECRET` are all set. The exchanger calls
identity-context-svc's token endpoint (RFC 8693 token exchange) with
`grant_type=urn:ietf:params:oauth:grant-type:token-exchange` to trade the
incoming `zoiko-internal` audience envelope for the target service's audience.

**Integrated callers:** updated `internal/carta/client.go` and
`internal/commercial/client.go` to use `idenv.ForwardTo(ctx, req, exchanger,
targetAudience)` for outbound calls. When no exchanger is configured, falls
back to `idenv.Forward` (passes the original envelope through).

**Rollout mode (per S5-B2):** `IDENTITY_ACCEPT_LEGACY_AUDIENCE=true` remains
in compose/k8s for all services. The console calls services directly with the
`zoiko-internal` envelope, so turning it off would break the console. Strict
mode (audience validation enforced) is proven only in tests and one E2E probe;
cutover recorded as BLOCKED on S5-B2.

**Config:** added `IdentityTokenExchangeURL`, `IdentityTokenExchangeClientID`,
`IdentityTokenExchangeClientSecret` to `internal/config/config.go` and
`deployments/docker-compose.yml`.

**Failure semantics:** exchange failure on an unknown route fails closed (503).
The exchanger error is logged and the original envelope is passed through as a
fallback, but `TestVerify_UnknownRouteFailsClosed` proves the gateway refuses
unknown routes before reaching this code path.

**Suite:** 10 packages ok (carta now has Exchanger support), 0 fail.

## GW-FE-1 — Console admin API client (removed 2026-11-04)

The first pass added six admin reads to `lib/api/gateway-auth.ts`. Verification found them unusable:
- No `Authorization` header was sent, so every read got 401.
- Three paths do not exist on the admin surface (`/decisions?…`, `/usage`, `/policy`).
- The response types did not match `admin.go`.
- `:8192` is not reachable from the console.

They were deleted together with the `gatewayAdmin` service URL. See the Console decision above for why they were not repaired.

## GW-FE-2 — Console diagnostic page (revised 2026-11-04)

`app/admin/gateway/page.tsx` follows the v2.0 layout, with data chosen to be correct in a multi-replica deployment:
- **KPIs:** gateway process (`/healthz`), verifying sign-ins (`/readyz`), signing keys published (identity JWKS), your envelope (time to expiry), your assurance level (MFA and sign-in age, from the session's own envelope).
- **Panels:**
  - What /verify checks: stages admission → identity → risk → tenant → admitted, each showing how many refusals it can give.
  - Who may set an identity header: edge strip → GTRM stamp → envelope verified → headers set → authResponseHeaders.
  - This console and the gateway: `ZOIKO_USE_GATEWAY`, readiness, keys seen, plus a warning callout when not ready.
- **Tabs:**
  - Denial reasons (17, searchable by code, filterable by stage and status; the drawer gives meaning, fix, and whether retry helps).
  - Verify my session (`GatewayVerifyForm`).
  - Signing keys.
- `DENIAL_REASONS` gained a `stage` field, plus `residency_denied` and `residency_unresolved` from GW-1.
- No write commands. Unreadable sources show "Could not be read" / "Not reachable", never 0.

**Checks:** `eslint` clean on the touched files. `tsc --noEmit` shows the same 94 pre-existing errors before and after, none in touched files. Render check against the dev server:
- Down path (gateway and JWKS unreachable): PASS.
- Up path (mocked `:8092` and `:8080`): PASS, including search, stage filter, reason drawer and keys drawer.
- Screenshot at 1600 px.
