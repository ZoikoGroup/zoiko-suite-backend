# secret-vault-integration-svc — Operational Runbook

The six alerts in `deployments/prometheus-rules.yml` under the
`secret-vault-integration` group each name a section here by number. Section 4
is that map: **4.1 → SecretBrokerDenialRateHigh, 4.2 → SecretPathNotProvisioned,
4.3 → SecretVaultBackendFailing, 4.4 → SecretVaultAuthorizationUnavailable,
4.5 → SecretMassRevocation, 4.6 → SecretVaultReadinessFailing.**

Port **8087**. Database `secret_vault_integration`. Topic
`zoiko.secretvault.events`.

---

## 1. What this service is, in one paragraph

Workloads do not hold long-lived credentials. They ask this service for a
short-lived **lease** over a secret, and it decides against an effective-dated
policy. It holds four tables — policies, versions, leases, an append-only audit
log — and **none of them contains a secret value**. The material lives behind a
vault backend and is reachable only through a `lease_token` that this service
mints and never stores. That matters during an incident: no amount of database
access here recovers a credential, and conversely a database outage does not
leak one.

It sits on the **startup path** of anything that needs a credential. When this
service is down, the symptom the on-call sees is usually some *other* service
failing to boot. Check here early.

---

## 2. First response

```bash
# The three commands worth running before anything else.
curl -s localhost:8087/readyz                      # dependency health, not just liveness
curl -s localhost:8087/metrics | grep '^secret_vault_'
docker logs --tail 100 secret-vault-integration-svc
```

`/healthz` answers 200 whenever the process is up and checks nothing. Always use
`/readyz` — it is what the container's own `HEALTHCHECK` runs, and a dead
connection pool reads as healthy on the other one.

Then decide which of two very different things you are looking at:

| Symptom | What it is | Section |
|---|---|---|
| `403 access_denied` | Policy resolved and refused the caller | 4.1 |
| `404 no_applicable_secret_policy` | No `ACTIVE` policy for that path and scope | 4.2 |
| `503 vault_backend_unavailable` | Policy said yes; material could not be produced | 4.3 |
| `503 authz_unavailable` | authorization-svc is down; writes fail closed | 4.4 |
| `503 store_unavailable` | Postgres | 4.6 |

Those first two look identical from a dependent workload's logs ("could not get
credential") and have opposite fixes. Do not skip the distinction.

The full request-side detail of any incident is already recorded. Every
brokerage attempt writes a `REQUESTED` row before the outcome is known and an
outcome row after, denials included:

```bash
curl -s "localhost:8087/v1/secrets/audit?secret_path=<path>&limit=50" \
  -H "X-Tenant-Id: <tenant>" -H "X-Principal-Id: <you>" -H "X-Source-Channel: api" \
  | python -m json.tool
```

---

## 3. The 404-vs-403 question, and why it stays open

A caller probing `POST /v1/secrets/broker` can tell a registered path (`403`)
from an unregistered one (`404`). That is deliberate, and it is a real,
bounded trade-off rather than an oversight — it was raised during design review,
left unresolved through implementation, and is settled here.

**Kept distinct, because:** the difference between "policy refused you" and "no
policy exists" is the difference between a permissions fix and a provisioning
fix, and the second is the single most common failure mode of this service —
an operator registers a path, creates a version, and forgets to activate it or
to seed material. Collapsing the two codes would make section 4.2 undiagnosable
from the outside and would convert the commonest misconfiguration into a silent
one.

**What bounds the exposure:** the broker is reachable only by a caller that
already passed gateway verification and carries a verified tenant, so this is
not an unauthenticated oracle. What leaks is the existence of a path name to a
party already inside the estate, and path names are not secrets — the material
behind them is, and neither response code brings a caller any closer to it.
Every probe, including every `404`, writes a `DENIED` audit row naming the
caller, so enumeration is expensive and self-incriminating rather than free.

**Where the same question is answered the other way:** `GET`/`revoke` on
`/v1/secrets/leases/{id}`. There a foreign tenant's lease returns `404`,
identical to an id that was never issued, because a lease id is a *guessable
address for a specific row* and confirming existence one guess at a time is a
genuine enumeration primitive. A `secret_path` is a name the caller already
had to know to ask. The asymmetry is intentional; section 8 of
`scripts/audit.sh` pins the lease side so it cannot regress.

Revisit if the broker ever becomes reachable from outside the estate.

---

## 4. Alerts

### 4.1 `SecretBrokerDenialRateHigh`

> Over 20% of brokerage refused for 10 minutes. **Page.**

Every one of these is a well-formed `403`, so error-rate alerting never sees it.
Almost always: a deployment changed a workload's identity and
`allowed_workload_ids` still names the old one.

```bash
# Who is being refused, and for what.
curl -s "localhost:8087/v1/secrets/audit?event_type=DENIED&limit=50" \
  -H "X-Tenant-Id: $TENANT" -H "X-Principal-Id: $YOU" -H "X-Source-Channel: api" \
  | python -c "import sys,json;[print(e['secret_path'], e['requested_by_principal_id'], e['outcome_detail']) for e in json.load(sys.stdin)]"
```

`outcome_detail` separates the two causes outright:

- `requesting principal not in allowed_workload_ids` → the policy is live and
  the caller is not on it. Fix by creating a **new version** with the corrected
  list and activating it. Versions are immutable; there is no edit.
- `no applicable secret policy for this path/scope` → this is really 4.2.

```bash
# The list currently in force for a path.
curl -s "localhost:8087/v1/secret-policies?secret_class=$CLASS&tenant_id=$TENANT" \
  -H "X-Tenant-Id: $TENANT" -H "X-Source-Channel: api" | python -m json.tool
```

Do **not** widen `allowed_workload_ids` to make the alert stop. If a workload's
identity changed, the correct new value is the new identity — not both.

### 4.2 `SecretPathNotProvisioned`

> Sustained brokerage against paths with no `ACTIVE` policy version. **Ticket.**

Answers `404`, so it is invisible to every error-rate rule. Walk the access path
in order and find the step that was skipped:

```bash
# 1. Is the path registered at all?
docker exec zoiko-postgres psql -U postgres -d secret_vault_integration -c \
  "SELECT secret_policy_id, secret_class FROM secret_policies WHERE secret_path = '$PATH';"

# 2. Does it have a version, and is that version ACTIVE?
docker exec zoiko-postgres psql -U postgres -d secret_vault_integration -c \
  "SELECT version_status, tenant_id, legal_entity_id, effective_from
     FROM secret_policy_versions v JOIN secret_policies p USING (secret_policy_id)
    WHERE p.secret_path = '$PATH' ORDER BY v.created_at DESC;"
```

Three findings, three fixes:

- **No policy row** — the path was never registered. `POST /v1/secret-policies`.
- **Version exists but is `DRAFT`** — by far the most common. A `DRAFT` is
  invisible to the broker; activation is what publishes it.
  `POST /v1/secret-policies/{id}/versions/{version_id}/activate`.
- **`ACTIVE` but in the wrong scope** — a version bound to tenant A does not
  serve a caller in tenant B. Compare the version's `tenant_id` against the
  caller's. Create a version in the right scope.

A steady trickle of these is normal probing and not worth a ticket; the rule
fires on a *sustained* rate for 15 minutes.

### 4.3 `SecretVaultBackendFailing`

> The vault backend is failing on `{{ $labels.operation }}`. **Page.**

This is the serious one. Policy says yes and the material cannot be produced, so
every dependent workload fails to start. The `operation` label says which call:

- **`get`** — brokerage is down. Dependent services cannot obtain credentials.
- **`put`** — provisioning is down. Existing leases are unaffected.
- **`rotate`** — rotation is down. Existing material is intact and still valid.

```bash
docker logs --tail 200 secret-vault-integration-svc | grep -i "vault backend"
curl -s localhost:8087/metrics | grep secret_vault_backend_errors_total
```

With the local file backend (`VAULT_LOCAL_STORE_PATH`), the causes in order of
likelihood:

1. **`VAULT_MASTER_KEY_HEX` changed or was lost.** Everything written under the
   old key is unreadable, permanently. The service refuses to start without a
   key and will not generate one — there is no default, deliberately, because a
   defaulted key silently makes the encryption ornamental.
2. **The store path is not writable, or is not persisted.** A container restart
   with the path on an ephemeral layer loses every seeded secret. `get` then
   fails for paths that were provisioned before the restart.
3. **Disk full.**

There is no recovery of material encrypted under a lost key. If the key is gone,
the path must be re-seeded with fresh material and rotated.

### 4.4 `SecretVaultAuthorizationUnavailable`

> Cannot reach authorization-svc. **Page.**

This service fails closed, so this is a **total write outage**: every policy
mutation, material write, revocation and rotation is refused. Brokerage keeps
working — `POST /v1/secrets/broker` is deliberately not RBAC-gated, because the
policy version's `allowed_workload_ids` is its authorization. Workloads are
unaffected; operators cannot change anything.

This is a dependency outage, **not** a permissions problem, and the alert exists
separately from 4.1 because the two are indistinguishable from the caller's side
and need opposite responses.

```bash
curl -s localhost:8089/readyz          # authorization-svc
docker ps --filter name=authorization-svc
curl -s localhost:8087/metrics | grep secret_vault_authz_decisions_total
```

Fix authorization-svc. Nothing to do here — the fail-closed behaviour is
correct and must not be worked around. In particular, do not set
`AUTHZ_SERVICE_URL` to an empty value to "unblock" writes: that switches the
service to a permit-all stub, which is a local-development affordance and an
unauthenticated write surface anywhere else.

### 4.5 `SecretMassRevocation`

> A rotation revoked more than 50 live leases in 10 minutes. **Ticket.**

Correct behaviour, deliberately alerted. Rotation revokes every live lease on
the path in the same transaction — that is the point of it, because leases
pointing at replaced material would otherwise fail later, at the workload,
away from the cause.

The question is only whether the rotation was intended and whether the blast
radius was understood.

```bash
# What was rotated, by whom, and how many leases it killed.
curl -s "localhost:8087/v1/secrets/audit?event_type=ROTATED&limit=20" \
  -H "X-Tenant-Id: $TENANT" -H "X-Principal-Id: $YOU" -H "X-Source-Channel: api" \
  | python -m json.tool
```

`acted_by_principal_id` names the operator. Each affected holder has a `REVOKED`
row in **its own tenant** carrying `revoked as a side effect of secret
rotation`, so a tenant that lost access can see why without being shown the
rotation itself.

Expect dependent workloads to re-broker and recover on their own. If they do
not, they are caching a lease token rather than re-fetching — a bug in the
consumer, not here. There is no un-rotate.

### 4.6 `SecretVaultReadinessFailing`

> Not ready for 1 minute. **Page.**

Deliberately tighter than the platform baseline: this service is on the startup
path of anything needing a credential, so a minute here is a longer outage than
a minute elsewhere.

```bash
curl -s localhost:8087/readyz
docker exec zoiko-postgres pg_isready
docker logs --tail 50 secret-vault-integration-svc | grep -i "database\|pool"
```

Almost always Postgres or the pool. No lease, policy or audit read will succeed
while this is firing. Already-issued lease tokens remain valid — they are
verified by the vault backend, not by this database — so workloads holding a
live lease keep working, and only new brokerage fails. That is a meaningful
difference when deciding how hard to escalate.

---

## 4.7 Break-glass, the exception register, and lease verification

This service's normal path never returns material. When an incident genuinely
requires it, the documented override is:

1. Register a time-boxed, evidence-backed exception:
   ```
   POST /v1/shared-secret-exceptions
   { "secret_path": "<path>", "reason": "...", "evidence_reference": "<ticket>",
     "expires_at": "<future>" }
   ```
   Requires `SECRET_EXCEPTION_CREATE`. An already-`ACTIVE` exception for the
   same path returns `200` with the existing record (idempotent); a conflicting
   one answers `409`.
2. Retrieve the material:
   ```
   POST /v1/secret-policies/{id}/emergency-retrieval
   { "request_id": "...", "reason": "..." }
   ```
   Requires `SECRET_EMERGENCY_RETRIEVAL` **and** the exception from step 1 —
   no exception is a `403 no_active_exception`, and the vault is never touched.
   The response is the raw secret, base64. Handle it like the credential it is.
3. Close the door when done: `POST /v1/shared-secret-exceptions/{id}/revoke`
   (`SECRET_EXCEPTION_REVOKE`), or let the exception expire — an expired or
   revoked exception blocks retrieval exactly as though none existed.

Every retrieval writes an `EMERGENCY_RETRIEVAL` audit row naming the actor and
the authorizing exception id, so after the incident the register shows who
opened the door, against what evidence, and when.

Separately, a workload holding a `lease_token` (and a load balancer/edge that
terminates TLS and forwards headers) can ask whether a token is still good:

```
POST /v1/secrets/leases/{lease_id}/verify   { "lease_token": "..." }
```

This is the surface that makes the lease's two controls reachable end-to-end:
expiry is refused by the token itself (no database read) and revocation by the
lease register (`200 valid:false reason=lease_revoked`). A `valid:true` answer
also proves the token and the lease agree on the secret path and the tenant.

---

## 5. Configuration that changes behaviour

| Variable | Effect if wrong |
|---|---|
| `VAULT_MASTER_KEY_FILE` | (**Preferred** key source.) Path to a file holding 32 bytes hex (AES-256). The file must be owner-only (0600) on POSIX platforms or the service refuses to start. Used in preference to `VAULT_MASTER_KEY_HEX`. Losing the file, like losing the raw key, permanently unreadable for everything written under it. |
| `VAULT_MASTER_KEY_HEX` | 32 bytes hex (AES-256). No default — the service refuses to start without a key of some kind. **Refused outright when `ENV` is production or staging**, because a literal key in environment is the very long-lived plaintext configuration the doctrine forbids; supply `VAULT_MASTER_KEY_FILE` there instead, or set `ALLOW_PLAINTEXT_MASTER_KEY=true` as an explicit, logged exception. Change it and every previously written secret becomes permanently unreadable. |
| `ALLOW_PLAINTEXT_MASTER_KEY` | Default false. `true` lifts the production/staging refusal on `VAULT_MASTER_KEY_HEX`. Every local stopgap; the startup log loudly states when it is set. |
| `TLS_CERT_FILE` / `TLS_KEY_FILE` | Server certificate and key. When set, the service terminates TLS on its own port (plain HTTP refused in production/staging unless `ALLOW_INSECURE_INBOUND=true`). Omitted in front of the gateway, which terminates TLS and forwards mTLS-verified headers. |
| `TLS_CLIENT_CA_FILE` | CA bundle trusted for client certificates. When set, inbound connections must present one (`RequireAndVerifyClientCert`). |
| `MTLS_IDENTITY_CHECK` | Default false. `true` re-validates on every request (not just at the TLS handshake) that the gateway-verified `X-Workload-Id`/`X-Principal-Id` matches the presented client certificate — the identity-spoofing control that closes the "broker trusts a header alone" gap. Refuses 401/403 rather than trusting a header alone. Requires client-cert-enabled ingress. |
| `ROTATION_SWEEP_INTERVAL` | Default unset (0) — the rotation sweeper is off. Set e.g. `1m` to run the automated-rotation loop on that cadence; any version created with `rotation_interval_seconds` is then rotated on schedule and its live leases mass-revoked at each rotation. See 4.5 for the alert that makes this visible. |
| `ROTATION_SWEEP_ACTOR` | Principal id stamped on sweeper-driven rotations and their audit rows. Default `system:rotation-sweeper`. |
| `ALLOW_INSECURE_INBOUND` | Default false. `true` lets the service bind plain HTTP in production/staging (TLS absent). A deliberately loud escape hatch, not a deployment option. |
| `VAULT_LOCAL_STORE_PATH` | Where encrypted material lives. On an ephemeral path, every restart silently loses every seeded secret and presents as 4.3. |
| `AUTHZ_SERVICE_URL` | Empty switches to a **permit-all stub**. Confirm the startup log says `using HTTP authorization client`, not `using PERMIT-ALL authorization stub`. |
| `AUTHZ_PLATFORM_SCOPE_ID` | Role assignments granting the `SECRET_*` actions must use this as `legal_entity_id`; authorization-svc rejects an empty one. Wrong value means every mutation is a correct-looking 403. |
| `KAFKA_BROKERS` | Unset drops events at debug level and the service runs normally. `cmd/server` refuses to start this way when `ENV` is production or staging. |

```bash
docker logs secret-vault-integration-svc 2>&1 | head -30 | grep -i "authorization\|vault\|kafka"
```

---

## 6. Verifying the whole service

`scripts/audit.sh` re-proves every behaviour in this document against a running
stack — brokerage, tenant isolation, rotation, audit evidence, the input
contract, the metrics the alerts above read, and the alert rules themselves. It
exits non-zero on any failure.

```bash
cd services/secret-vault-integration-svc && bash scripts/audit.sh
```

It needs the `CONSOLE_DEMO_OPERATOR` grants from
`deployments/scripts/seed-demo-rbac.ps1` — this service's six `SECRET_*` actions
are in its `VAULT_FULL` bundle. Without them every mutation is a correct 403 and
the script reports a working service as broken.

---

## 7. What NOT to do

- **Do not go looking for a secret value in Postgres.** There is none, by
  design. If you find one, that is the incident.
- **Do not paste a `lease_token` into a ticket, a log, or a chat.** It is a live
  bearer credential. The `lease_id` is the safe handle and is what the audit log
  and every event carry.
- **Do not edit a policy version.** They are immutable. Create a new version and
  activate it; the old one is superseded, not deleted, which is what keeps
  "which rule was in force when this lease was granted" answerable.
- **Do not UPDATE or DELETE `secret_access_audit_log`.** Append-only is the
  guarantee that makes it evidence.
- **Do not work around a fail-closed refusal** by emptying `AUTHZ_SERVICE_URL`
  (section 4.4) or by widening `allowed_workload_ids` (section 4.1).
- **Do not register a shared-secret exception to make a refusal go quiet.**
  The exception register exists for genuine break-glass, and every entry names
  its evidence and author. An exception to bypass 4.1 or 4.3 is an incident
  record, not a fix — revoke it the moment the real fix lands.
- **Do not paste emergency-retrieval material into a ticket or chat.** It is
  the one value this API hands back raw; the `request_id` in the `EMERGENCY_RETRIEVAL`
  audit row is the safe reference for the follow-up.
- **Do not point the store test suite at a live database.** Its last test drops
  all four tables on purpose to prove the error path, and does not restore them.
  `scripts/audit.sh` uses a scratch database for exactly this reason.
