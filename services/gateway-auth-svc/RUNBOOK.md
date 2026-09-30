# gateway-auth-svc — Operational Runbook

The six alerts in `deployments/prometheus-rules.yml` under the `gateway-auth`
group each name a section here by number. Section 4 is that map:
**4.1 → GatewayAuthJWKSUnavailable, 4.2 → GatewayAuthTokenSpoofingDetected,
4.3 → GatewayAuthTenantContextUnresolved, 4.4 → GatewayAuthTenantContextStale,
4.5 → GatewayAuthRejectionRateHigh, 4.6 → GatewayAuthReadinessFailing.**

Port **8092**. No database. No message broker. No persistent state of any kind.

---

## 1. What this service is, in one paragraph

Traefik calls `/verify` before routing **any** gated request. A 2xx lets the
request through and its headers are copied onto the forwarded request; anything
else is returned to the client verbatim and the backend never sees the request
at all. It verifies a signed identity envelope against identity-context-svc's
JWKS, checks the token's tenant against the tenant the caller's hostname
resolved to, optionally scores the request for risk, and resolves the
authoritative tenant context from tenant-entity-registry-svc.

**Read this next part before anything else during an incident.** When this
service is broken, it does not look broken. The symptom is *every other service
in the estate returning 401*, users reporting "the whole platform is down", and
nothing in any backend's logs — because the requests never reached them. If
several unrelated services are all failing authentication at once, come here
first.

It is also stateless and holds nothing that needs recovering. Restarting it is
cheap and loses only two in-memory caches that rebuild on demand.

---

## 2. First response

```bash
# The three commands worth running before anything else.
curl -s -o /dev/null -w '%{http_code}\n' localhost:8092/readyz   # 200 or the JWKS is gone
curl -s localhost:8092/metrics | grep '^gateway_auth_verify_decisions_total'
docker logs --tail 100 gateway-auth-svc
```

`/healthz` answers 200 whenever the process is up and checks nothing. Always
use `/readyz` — it pings the JWKS endpoint, which is the only dependency that
can silently stop this service working.

Then read the outcome counter. One route, one status code, several completely
different situations behind it:

| Outcome label | What it is | Section |
|---|---|---|
| `no_token` | No bearer credential. Ordinary noise on a public gateway. | 4.5 |
| `invalid_token` | Signature, expiry, issuer, audience or `kid` failed | 4.1 / 4.5 |
| `incomplete_claims` | Verified token with no principal or tenant — **upstream** defect | 4.5 |
| `tenant_hostname_mismatch` | Valid token, wrong tenant's hostname — **attack** | 4.2 |
| `carta_blocked` | Risk assessment returned ISOLATE or DENY | — |
| `tenant_context_denied` | Registry refused the tenant | — |
| `tenant_context_unresolved` | Registry unreachable | 4.3 |

A wall of `invalid_token` with `gateway_auth_jwks_errors_total{operation="fetch"}`
climbing is an outage (4.1). The same wall without it is a credential problem.
They look identical in the HTTP status and in every log line.

To reproduce a caller's refusal and see which stage fired:

```bash
curl -s -D- -o /dev/null localhost:8092/verify -H "Authorization: Bearer $TOKEN" \
  | grep -iE 'HTTP|x-auth-denial-reason|x-tenant-context|x-carta-decision'
```

`X-Auth-Denial-Reason` is set on every refusal and reaches the client, because
Traefik returns an unsuccessful ForwardAuth reply verbatim.

---

## 3. Minting a token by hand

Several checks below need a real envelope. It cannot be faked — it is RS256 and
verified against identity-context-svc's live JWKS — so mint one through the
real two-hop flow:

```bash
TEN=11111111-1111-1111-1111-111111111111
ENTITY=22222222-2222-2222-2222-222222222222
PRIN=33333333-3333-3333-3333-333333333333

ACCESS=$(curl -s -X POST localhost:8080/v1/authenticate -H 'Content-Type: application/json' \
  -d "{\"tenant_id\":\"$TEN\",\"email\":\"admin@zoikosuite.com\",\"password\":\"Zoiko@Governance1\"}" \
  | python -c 'import sys,json;print(json.load(sys.stdin)["access_token"])')

ENVJWT=$(curl -s -X POST localhost:8080/v1/context/resolve -H 'Content-Type: application/json' \
  -H "X-Tenant-Id: $TEN" -H "X-Principal-Id: $PRIN" -H "X-Legal-Entity-Id: $ENTITY" \
  -H 'X-Source-Channel: api' -H 'X-Request-Id: rb-1' -H 'X-Correlation-ID: rb-1' -H 'Idempotency-Key: rb-1' \
  -d "{\"bearer_token\":\"$ACCESS\",\"legal_entity_id\":\"$ENTITY\"}" \
  | python -c 'import sys,json;print(json.load(sys.stdin)["envelope_jwt"])')
```

`/v1/context/resolve` needs **access-control-svc** reachable (it reads
permission bundles). If resolve returns `upstream dependency unavailable`, that
is what is missing — not a problem with this service.

---

## 4. Alerts

### 4.1 `GatewayAuthJWKSUnavailable`

> The JWKS endpoint cannot be read and nothing is cached. **Page.**

**This is a total authentication outage.** Every gated request in the estate is
failing closed. Nothing else surfaces it: the gateway answers a correct 401 and
no backend ever sees the request, so every other service's logs are quiet.

```bash
curl -s -o /dev/null -w '%{http_code}\n' localhost:8080/.well-known/jwks.json
curl -s -o /dev/null -w '%{http_code}\n' localhost:8092/readyz
docker logs --tail 50 identity-context-svc
```

The fix is almost always in identity-context-svc, not here. Note what this
service does on its own to soften it: `jwks.Client` keeps serving a **cached**
key for a `kid` it already trusts even when a refresh fails, so a brief blip
does not take the gateway down. This alert means the cache could not cover it —
either the outage outlasted the cache, or the tokens arriving are signed by a
key that was never cached.

Distinguish the two before escalating:

```bash
curl -s localhost:8092/metrics | grep gateway_auth_jwks_errors_total
```

`operation="fetch"` is the outage. `operation="key_lookup"` is a signing-key
rotation this gateway has not caught up with — see 4.5, it self-heals on the
next refresh and only affects tokens signed by the new key.

**Do not** work around this by disabling verification. There is no flag for it,
deliberately.

### 4.2 `GatewayAuthTokenSpoofingDetected`

> A validly-signed token was presented against another tenant's hostname.
> **Page.**

Treat as credential theft or replay until shown otherwise. This is the only
refusal in this service that is evidence of an **attack** rather than a
misconfiguration: by construction the token verified, so whoever sent it holds
genuine credentials and is using them against a tenant that is not theirs
(GTRM decision doc §6.2). The threshold is *any* occurrence — a rate threshold
here would mean tolerating some amount of it.

```bash
docker logs gateway-auth-svc 2>&1 | grep 'token/hostname tenant mismatch'
```

The log line carries `token_tenant_id`, `resolved_tenant_id` and
`forwarded_uri`. The same event is streamed to SIEM as
`auth.tenant_hostname_mismatch` at **CRITICAL** — higher than a tenant-context
denial (HIGH), because that is a caller asking for something it may not have,
and this is a caller *holding* something it should not.

Response: identify the principal from the log, revoke its sessions in
identity-context-svc, and check whether the same principal appears in
successful verifications around the same time. A partial success rate is worse
news than a total failure — it means some of the attempts landed on the right
hostname.

**Note what this alert cannot see.** The check only runs when
`X-Zoiko-Resolved-Tenant-Id` is present, which today is only the GTRM Phase 1
proof slice. On a route not yet onboarded, no comparison is made and this
spoofing shape would pass. That is an honest gap, not a silent one — see §7.

### 4.3 `GatewayAuthTenantContextUnresolved`

> tenant-entity-registry-svc is unreachable. **Page.**

The gateway fails closed. Writes are refused with `503` outright; reads survive
only inside the stale-grace window (`TENANT_CONTEXT_STALE_GRACE_SECONDS`,
default 120s) and then stop too. So this starts as a write outage and becomes a
full one a couple of minutes later.

```bash
curl -s -o /dev/null -w '%{http_code}\n' localhost:8081/readyz
docker logs --tail 50 tenant-entity-registry-svc
curl -s localhost:8092/metrics | grep gateway_auth_tenant_context_total
```

`503`, not `403`, is deliberate: no decision was obtained, and answering `403`
would tell the caller it had been refused when in fact nothing could be
determined. The response carries `X-Tenant-Context: unresolved` and
`Retry-After`, and the console renders it as "retry", not "ask for a grant".

Fix the registry. Do **not** unset `TENANT_REGISTRY_URL` to make the alert
stop — that disables GOV-01 resolution estate-wide, so tenant operability and
cross-tenant entity ownership stop being checked at all, and everything starts
passing. See §5.

### 4.4 `GatewayAuthTenantContextStale`

> Over 10% of reads are being served from an unconfirmed cache. **Ticket.**

Correct, bounded behaviour — and the early warning that 4.3 is coming. The
registry is intermittently unreachable, reads are being served from cache, and
**writes are already being refused** even though nothing has paged yet.

```bash
curl -s localhost:8092/metrics | grep 'gateway_auth_tenant_context_total'
docker logs --tail 50 tenant-entity-registry-svc
```

A backend receiving one of these sees `X-Tenant-Context-Stale: true` and can
decide for itself whether that is good enough. Nothing is wrong with this
service; go and look at the registry before it stops answering entirely.

### 4.5 `GatewayAuthRejectionRateHigh`

> Over half of all requests refused for 10 minutes. **Ticket.**

Deliberately a *ratio*, and deliberately not alerting on 401s alone: a public
endpoint always refuses some traffic, and a raw count would either page on
ordinary scanner noise or need a per-deployment threshold nobody maintains.

The outcome label is the whole diagnosis:

```bash
curl -s localhost:8092/metrics | grep '^gateway_auth_verify_decisions_total' | sort -k2 -n -r
```

- **`invalid_token` dominant** — check `gateway_auth_jwks_errors_total` first.
  `key_lookup` climbing means a signing-key rotation; it self-heals once the
  cache refreshes (`JWKS_CACHE_TTL_SECONDS`, default 300). `fetch` climbing is
  4.1. Neither climbing means tokens really are expired or forged — check
  whether identity-context-svc's token lifetime changed.
- **`no_token` dominant** — a client stopped sending credentials. Usually a
  deployment that lost an env var, or scanner traffic. Correlate with
  `X-Forwarded-Uri` in the logs: one route means a broken caller, every route
  means a scanner.
- **`incomplete_claims` present at all** — identity-context-svc is minting
  envelopes with no principal or no tenant. This is an **upstream defect**, not
  a caller problem, and it should be zero. Escalate to that service.
- **`tenant_context_denied` dominant** — not this service's problem: the
  registry is refusing tenants. Check for a tenant that was suspended or a
  lifecycle state that changed.

### 4.6 `GatewayAuthReadinessFailing`

> Not ready for 1 minute. **Page.**

Tighter than the platform baseline on purpose. This is not one service
degrading — it is the front door, and nothing gated is reachable while it is
firing.

```bash
curl -s -o /dev/null -w '%{http_code}\n' localhost:8092/readyz
docker inspect --format '{{.State.Health.Status}}' gateway-auth-svc
docker logs --tail 50 gateway-auth-svc
```

Readiness here is exactly one thing: can the JWKS endpoint be reached. So this
alert and 4.1 usually fire together, and 4.1 is the one with the diagnosis.

If `/readyz` answers 200 but the container still reads unhealthy, check what
the healthcheck is probing:

```bash
docker inspect --format '{{json .Config.Healthcheck.Test}}' gateway-auth-svc
```

It must name `/readyz`. It used to name `/healthz`, which answers 200 whenever
the process is up — so the container reported healthy while refusing every
request in the estate.

---

## 5. Configuration that changes behaviour

| Variable | Effect if wrong |
|---|---|
| `IDENTITY_JWKS_URL` | Wrong or unreachable → total authentication outage (4.1). |
| `EXPECTED_ISSUER` / `EXPECTED_AUDIENCE` | Mismatched → every token fails as `invalid_token`, with no other symptom. |
| `JWKS_CACHE_TTL_SECONDS` | How long a key-rotation lag lasts. Default 300. |
| `TENANT_REGISTRY_URL` | **Unset disables GOV-01 resolution entirely.** Tenant operability and cross-tenant entity ownership stop being checked and everything passes. The startup log says which mode it is in — check it. |
| `TENANT_CONTEXT_STALE_GRACE_SECONDS` | How long reads survive a registry outage. Default 120. Writes are never served from stale context regardless. |
| `CARTA_SERVICE_URL` | Unset → no risk scoring. Degrades to "not scored", never to a forced deny. |
| `SIEM_SERVICE_URL` | Unset → security events are logged but **not streamed**, including 4.2. |

```bash
docker logs gateway-auth-svc 2>&1 | head -30 | grep -i 'tenant context\|tracing\|starting'
```

Look for `tenant context resolution enabled`. The warning form —
`tenant context resolution DISABLED` — is the one that matters.

---

## 6. Verifying the whole service

`scripts/audit.sh` re-proves every behaviour in this document against a running
stack, including a real end-to-end verification with a genuinely minted
envelope. Exits non-zero on any failure.

```bash
cd services/gateway-auth-svc && bash scripts/audit.sh
```

Needs the service on `:8092`, identity-context-svc on `:8080` with
access-control-svc and tenant-entity-registry-svc reachable (see §3), and
Prometheus on `:9090`. `SKIP_FE=1` skips the console section.

---

## 7. What NOT to do

- **Do not assume a wall of 401s is an attack.** On this service it is equally
  the shape of a total JWKS outage. Check
  `gateway_auth_jwks_errors_total{operation="fetch"}` before escalating to
  security.
- **Do not unset `TENANT_REGISTRY_URL` to clear 4.3 or 4.4.** It does not fix
  the registry; it stops the check, estate-wide, and everything starts passing.
- **Do not treat a missing `X-Zoiko-Resolved-Tenant-Id` as suspicious.** It
  means the route is not on GTRM-resolved routing yet, so no comparison is
  made. The corollary is the real gap: the spoofing check in 4.2 does not run
  on those routes at all.
- **Do not add a header to the 200 response without adding it to
  `authResponseHeaders`** in the ForwardAuth middleware. Traefik drops
  unlisted headers silently — the backend simply never sees it, with no error
  at either end. Section 14 of the audit checks this.
- **Do not read `X-Carta-Decision` as a general denial reason.** It is set only
  when carta-svc actually made a decision. `X-Auth-Denial-Reason` is the one
  that is always present.
- **Do not look for persistent state to recover.** There is none. Restarting is
  cheap and loses only two rebuildable caches.
