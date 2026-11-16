# policy-svc: Runbook

Port **8085** (HTTP) and **8449** (mTLS peer listener). Database `policy`. Tier 0 governance
service: business, financial and internal-control policies, GOV-05 lifecycle, evaluation with
durable evidence, REF-07 accounting policy, doc7 control tests and attestations, and V-001
PDC-05 certification.

## Dependencies

| Dependency | Used for | When it is down |
|---|---|---|
| PostgreSQL `policy` | everything | `/readyz` 503; every route 503 |
| authorization-svc :8089 | every command and (in enforce) every read and evaluation | fail closed: 503 `authz_unavailable` |
| identity-context-svc JWKS and introspection | identity gate (`POLICY_IDENTITY_VERIFY`) | enforce: 401; warn: logged |
| governance-decision-log-svc :8450 (mTLS) | evidence relay | evaluations still answer; evidence waits in `policy_evidence_outbox` |
| mtls-management-svc :8140 | workload certificates (relay client, :8449 listener) | the plaintext listener keeps serving |
| Kafka `zoiko.policy.events` | outbox relay | events wait in `outbox`; `outbox_oldest_pending_seconds` grows |

## Configuration (beyond DB and Kafka)

| Variable | Default | Meaning |
|---|---|---|
| `AUTHZ_SERVICE_URL`, `AUTHZ_PLATFORM_SCOPE_ID` | — | authorization-svc, and the legal entity presented for unscoped decisions |
| `POLICY_EVALUATE_AUTHZ`, `POLICY_READ_AUTHZ` | enforce | `warn` only during a caller migration |
| `POLICY_IDENTITY_VERIFY` | enforce | compose runs `warn` until EST-1 |
| `POLICY_CERTIFICATION_GATE` | enforce | PDC-05 activation gate; `warn`/`off` only during migrations |
| `POLICY_LEDGER_PRINCIPAL_ID` | — | the ledger writer (`svc-policy`); needs `GOVERNANCE_DECISION_RECORD` |
| `GOVERNANCE_DECISION_LOG_MTLS_URL` | — | relay over mTLS (recommended) |
| `MTLS_ENABLED`, `MTLS_PORT`, `MTLS_ALLOWED_CLIENTS` | false, 8449, any | peer listener for system callers |
| `CURSOR_SIGNING_KEY_HEX` | random in local/dev | ≥32-byte hex; required outside local/dev (API-4 cursors) |
| `EVENT_RESIDENCY_REGION` | — | stamped on every event |
| `POLICY_SCHEDULER_INTERVAL`, `POLICY_EVIDENCE_RELAY_INTERVAL` | 15s, 2s | background workers |

## Deploy

1. Apply the migrations before the new binary:
   `python deployments/migrate.py up --db policy`. 000010 puts the outbox tables under RLS,
   000011 adds family retirement, and 000012 adds the PDC-05 tables.
2. Apply access-control-svc migrations 000057 and 000058. They register
   `POLICY_EVIDENCE_REDELIVER` and the five PDC-05 actions. Then grant those actions to the
   roles that need them; who holds them is a role-design decision.
3. Set `CURSOR_SIGNING_KEY_HEX` outside local/dev, or the service refuses to start.
4. Before turning on `POLICY_CERTIFICATION_GATE=enforce`, give every evaluable family that will
   take new versions a suite with at least one mandatory case. Until then its successors cannot
   go live: the API answers 409 `CERTIFICATION_REQUIRED`.
5. Rebuild the image from the repository root
   (`docker compose -p deployments -f deployments/docker-compose.yml build policy-svc`). Check
   the image timestamp, then run `up -d --no-deps --no-build --force-recreate policy-svc`.

## Operations

### Evidence stuck at FAILED
`policy_evidence_failed` > 0 means governance-decision-log-svc refused a record for good, for
example a conflicting `decision_id`. Read `last_error`:

```sql
SELECT evaluation_id, last_error FROM policy_evidence_outbox WHERE failed_at IS NOT NULL;
```

Run that as an operator role in the relay context (`SET app.outbox_relay = 'true'`). Fix the
cause, then re-queue the evidence:
`POST /v1/policy-evaluations/{id}/redeliver` with `{"reason_code": "..."}`. This needs
`POLICY_EVIDENCE_REDELIVER` and writes `policy.evidence.redelivery_requested`.

### Evidence pending grows
`policy_evidence_pending` is rising. Check that `POLICY_LEDGER_PRINCIPAL_ID` holds
`GOVERNANCE_DECISION_RECORD`, and that the relay's certificate is admitted by
governance-decision-log-svc.

### Outbox backlog
`outbox_oldest_pending_seconds` is rising, so Kafka is down or refusing messages. Rows stay in
`outbox` and retry with backoff (max 5 min). Consumers deduplicate on `event_id`.

### 409 CERTIFICATION_REQUIRED on activate or schedule
`GET /v1/policy-versions/{id}/certification` says why:
- **NOT_RUN** or **STALE**: run the suite (`POST …/test-runs`).
- **FAILED**: fix the version or the case, or request a waiver
  (`POST …/waivers`, at most 180 days). Someone other than the requester approves it.

A waiver never makes a run PASS; it makes the version WAIVED.

### 409 POLICY_IN_USE on family retire
Retire or supersede the family's ACTIVE or SCHEDULED versions first.

### 409 POLICY_CONFLICTED on evaluate
Two equally specific policies of one type govern one scope. Nothing is applied, and
`policy.applicability.conflicted` is emitted. Retire one, or narrow its scope.

### 401 from a system caller
A caller without a person's signed envelope must present its workload certificate on `:8449`
(`MTLS_ALLOWED_CLIENTS`). notification-svc does this with `LEGAL_BASIS_POLICY_MTLS_URL`.

### Emergency suspension of an accounting policy
This needs a live break-glass session (`X-Break-Glass-Session-Id`). A grant whose basis is not
that session is refused 403 `EMERGENCY_SESSION_REQUIRED`.

### Resolution cache
The cache is keyed by type, tenant and entity, and is valid only while
`policy_resolution_watermark` is unchanged. Every lifecycle change bumps the watermark, so the
cache never needs a manual flush.

## Verification

- Unit and DB suites, against a scratch Postgres with a NOSUPERUSER NOBYPASSRLS role and 0 skips:
  `REQUIRE_DB_TESTS=1 TEST_DATABASE_URL=… go test -p 1 ./...` (see `progress.md` for counts).
- Contract: `TestOpenAPI_MatchesTheRouter`, `TestAsyncAPI_MatchesTheCode`,
  `TestR5_EveryCheckedActionIsRegistered`, `TestMigrations_NumberedOnceWithDownFiles`.
- Live: `python deployments/scripts/e2e-policy-deps.py`.
