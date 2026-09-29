# Release Certificate — gateway-auth-svc

**Date:** 2026-09-21
**Scope:** the service and the console surface that reads it, end to end.
**Result:** `scripts/audit.sh` — **60 checks, 0 failures**, against a running
stack.

This certifies what was verified, against what, and what was deliberately not
done.

---

## What this service is, and why the gaps mattered

Traefik calls `/verify` before routing **any** gated request. A 2xx lets it
through; anything else is returned to the client verbatim and the backend never
sees the request. It is the front door for the whole estate.

Which is exactly why the state it was found in was serious. It had **no
telemetry of any kind** — no `/metrics` endpoint, no Prometheus client
dependency, no scrape job, no alert rules. The one service whose failure
presents as *every other service returning 401* was the only one in the estate
with no signal of its own. An operator watching this platform could not, from
any dashboard, tell a healthy gateway rejecting scanner traffic from a JWKS
outage locking every user out.

The code itself was in good shape: 36 tests, correct nil-receiver guards on
every optional dependency, and a genuinely careful fail-closed posture. The
defects were all in what it *reported*, not in what it decided.

---

## Verification performed

Every figure below came from a run on 2026-09-21.

| Check | Result |
|---|---|
| `go build ./...` | clean |
| `go vet ./...` | clean |
| `go test ./...` | **57 tests**, 0 failing (was 36) |
| Console `tsc --noEmit` | clean |
| Console `eslint` on changed files | clean |
| Playwright, whole suite | **35 passing** (4 new here, 0 regressions) |

### Live, against the running stack

`scripts/audit.sh` sections 3–14: health and the container's own `HEALTHCHECK`,
a **real end-to-end verification with a genuinely minted envelope**, every
refusal path naming itself, the case-insensitive bearer scheme, token/hostname
tenant binding, ungated probes, 19 metric series, Prometheus actually scraping
the target, all 6 alert rules loaded and evaluating, contract-document parity,
and Traefik's `authResponseHeaders` covering every header the service produces.

The envelope is not a stub. It is minted through the real two-hop flow
(`/v1/authenticate` → `/v1/context/resolve`) against a live
identity-context-svc, so the audit proves the one path that matters actually
works rather than only exercising refusals.

---

## Contract surface

One functional endpoint and three probes — all four documented in
`openapi.yaml` and compared against the router **in both directions** by
section 13 of the audit.

| Route | Purpose |
|---|---|
| `/verify` | The ForwardAuth decision. Accepts every method, because ForwardAuth replays the client's. |
| `/healthz` | Liveness. Checks nothing, by design. |
| `/readyz` | Readiness. Pings the JWKS endpoint. |
| `/metrics` | Prometheus exposition. **New.** |

### No `asyncapi.yaml`, deliberately

This service publishes no events. It has no broker dependency at all — security
signals go to siem-integration-svc over HTTP, which is that service's contract,
not this one's. Section 13 asserts `go.mod` still pulls in no Kafka client, so
if that ever changes the missing event contract fails the audit rather than
going unnoticed.

---

## Defects found and fixed

Six. Each is now pinned by a check that fails if it returns.

| # | Defect | Pinned by |
|---|---|---|
| 1 | **No telemetry at all.** No `/metrics`, no Prometheus dependency, no scrape job, no alert rules, on the service every gated request passes through. | audit §10–§12, `TestRouter_ExposesMetrics` |
| 2 | **Token/hostname spoofing was logged and nothing else.** The one refusal here that is evidence of an attack rather than a misconfiguration — by construction the token verified, so whoever sent it holds genuine credentials — reached a log line, while strictly less serious events (every CARTA decision, every tenant-context denial) already streamed to SIEM. Now streamed as `auth.tenant_hostname_mismatch` at CRITICAL. | `TestVerify_TenantHostnameMismatchStreamsToSIEM` |
| 3 | **That rejection was reported as a CARTA decision.** A shared helper set `X-Carta-Decision: tenant_hostname_mismatch`, attributing the refusal to a component that had not been consulted and sending anyone reading the header to carta-svc's logs for a decision that was never there. | audit §8, `TestVerify_ResolvedTenantMismatch_Returns403` |
| 4 | **The bearer scheme was matched case-sensitively.** RFC 7235 §2.1 defines auth-scheme as case-insensitive and RFC 6750 inherits it, so `bearer <token>` — a spelling several HTTP clients emit by default — was refused with the same 401 as a forged credential. | audit §7, `TestBearerSchemeIsCaseInsensitive` |
| 5 | **The container healthcheck probed `/healthz`.** Liveness answers 200 whenever the process is up, so with an unreachable JWKS endpoint the container reported healthy while refusing every request in the estate. Now `/readyz`. | audit §3, which asserts the healthcheck command itself, not just its result |
| 6 | **JWKS failures were unstructured strings**, so a total identity-context-svc outage and a signing-key rotation lag were indistinguishable — both surfaced as one undifferentiated "invalid token". Now `ErrUnavailable` / `ErrKeyNotFound`, counted separately. | `TestVerify_JWKSOutageIsNotReportedAsABadToken`, `TestVerify_UnknownKidIsAKeyMissNotAnOutage` |

### The thread running through all six

Five of the six are the same class of fault: **the service decided correctly
and then described the decision badly.** It refused the right requests for the
right reasons throughout — it just could not tell anyone which reason, so an
outage and an attack and ordinary scanner noise all presented identically. That
is why the fix is overwhelmingly labels, sentinels and headers rather than
control flow.

---

## Artifacts added

- `internal/telemetry/` — the Observability Baseline package, with four domain
  counter families and an outcome label per terminal path.
- `openapi.yaml` — the four routes, and the header contract, which is the part
  that has no other written home.
- `RUNBOOK.md` — first response, how to mint a token by hand, one section per
  alert (§4.1–4.6), the configuration that changes behaviour, and what not to
  do.
- `scripts/audit.sh` — 60 checks.
- `X-Auth-Denial-Reason` on every refusal.
- Console: `e2e/gateway-auth.spec.ts` (4 specs) against
  `e2e/mock/tenant-registry-service.mjs`, wired into `playwright.config.ts`.
- Infra: a Prometheus scrape job, six alert rules, and an
  `OTEL_EXPORTER_OTLP_ENDPOINT` in compose.

### On the console surface

gateway-auth-svc has **no console page, and correctly should not** — it is a
router callout with one machine endpoint that application code never calls.
Its frontend contract is the refusal signal: `X-Tenant-Context: denied |
unresolved`, which Traefik returns verbatim and which `lib/api/client.ts` reads
off every response.

That path had no test coverage. It matters because it reuses 403 and 503 — the
same statuses a backend uses for unrelated reasons with unrelated fixes — so a
change to this service's header names would have silently made the console
report a suspended tenant as "authorization-svc refused your principal",
sending the reader to an RBAC screen that could never help. The four new specs
pin both signals and, explicitly, that they do not render identically.

---

## Not done, and deliberately

- **The spoofing check does not run on most routes.** It requires
  `X-Zoiko-Resolved-Tenant-Id`, which today only GTRM's Phase 1 proof slice
  sets. On a route not yet onboarded, a token minted for one tenant and
  presented against another's hostname passes. This is an honest gap in
  coverage, not a bug in the check, and it is stated in `RUNBOOK.md` §4.2 so an
  on-call is not misled about what the alert can see.
- **`STEP_UP_MFA` is not enforced.** There is no step-up challenge flow
  anywhere downstream to redirect to, and hard-blocking with no path to recover
  would be a silent lockout wearing a different name. It is counted and
  streamed, not enforced.
- **Resource sensitivity for risk scoring is a hardcoded `MEDIUM`.** No
  resource-classification registry exists on this platform yet; the placeholder
  is labelled as one at the call site.
- **No rate limiting or lockout.** Repeated `invalid_token` from one source is
  counted and alertable but not throttled here. That belongs at the edge.
- **CARTA and GOV-01 resolution are both optional** and skipped when their
  service URL is unset. That is deliberate — failing closed on a dependency a
  deployment never configured would make the gateway unusable — but it does
  mean an unset `TENANT_REGISTRY_URL` silently disables tenant-operability
  checking estate-wide. The startup log says which mode it is in, and
  `RUNBOOK.md` §5 and §7 both flag it.

---

## Re-proving this

```bash
cd services/gateway-auth-svc && bash scripts/audit.sh
```

Needs the service on `:8092`, identity-context-svc on `:8080` with
access-control-svc and tenant-entity-registry-svc reachable (the envelope
cannot be faked — see `RUNBOOK.md` §3), Prometheus on `:9090`, and the
console's `node_modules` for §15. `SKIP_FE=1` skips the console section.

Exits non-zero on any failure, so CI can gate on it.

---

## Certification

As of 2026-09-21, gateway-auth-svc and the console surface that reads it pass
**60 of 60** audited checks against a live stack: build, vet, 57 unit and
integration tests, a real end-to-end verification with a minted envelope, every
refusal path, tenant binding, telemetry, an active scrape target, six
evaluating alert rules, contract parity in both directions, Traefik header
coverage, and 4 console end-to-end specs.

The six defects this pass found are fixed and each is pinned by a check that
fails if it returns. What remains undone is listed above and is deliberate —
with one item, the limited reach of the spoofing check, that is a genuine
coverage gap rather than a design choice, and is documented as such where an
on-call will read it.
