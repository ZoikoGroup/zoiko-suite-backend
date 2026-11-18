# evidence-requirements-svc — Release Certificate

Certified against the evidence of the verification pass of 5 Nov 2026 only. Every figure
below was produced in that pass; see progress.md "Verification evidence" for the commands
and the verbatim output.

| | |
|---|---|
| Service | evidence-requirements-svc (Governance Platform 7/7), port 8130 |
| Image | `deployments-evidence-requirements-svc:latest`, built 5 Nov 2026 13:26 IST |
| Migrations | 000001–000011, applied to the local `evidence_requirements`; 000010/000011 up/down/up on a scratch database |
| Contract | openapi.yaml 1.4.0 (kin-openapi valid, route parity test), asyncapi.yaml 1.1.0 |

## Tests

| Suite | Pass | Fail | Skip |
|---|---|---|---|
| internal/handler | 72 | 0 | 0 |
| internal/store (real Postgres 16) | 24 | 0 | 0 |
| internal/store, `-tags=integration` | 35 | 0 | 0 |
| internal/events | 5 | 0 | 0 |
| internal/manifest | 3 | 0 | 0 |
| internal/esignature | 3 | 0 | 0 |
| internal/registry | 1 | 0 | 0 |
| internal/telemetry | 1 | 0 | 0 |
| `scripts/audit.sh` (live) | 42 | 0 | — |
| `e2e-evidence-requirements-deps.py --with-outage` (live) | 84 | 0 | 3 (owner-blocked, never counted as passes) |

Mutation check: breaking the distinct-artifact key, the owner-unavailable branch and the
replay fingerprint each fails a named test.

## Controls proven live
- An artifact counts only when its owner confirms it in the caller's tenant and entity:
  random document → MISSING; one document twice vs minimum 2 → MISSING; document of another
  entity → MISSING; empty reference → MISSING.
- An owner outage (document-vault stopped) → 503, ledger row count unchanged.
- Replay → same evaluation_id, no second event; same key, different action → 409.
- Each SATISFIED/MISSING produced exactly one Kafka record keyed `<tenant>/<evaluation_id>`;
  NO_REQUIREMENTS_DEFINED and replays produced none; outbox rows published.
- Identity: tampered envelope 401 (warn mode); invalidated session refused within the
  revocation TTL; through Traefik + gateway-auth no bearer is 401 `no_token` and forged
  identity headers are replaced by the verified ones.
- API-3: 428 without If-Match, 412 on a stale one, on every manager command; API-4: signed
  cursors, tampered cursor 400, unknown filter 400.

## Not certified (BLOCKED, external)
- A real evidence manifest counting: `EVIDENCE_MANIFEST_READ` is not registered, so
  evidence-manifest-svc refuses every read; such references fail closed (503).
- SIGNATURE references live: esignature-integration-svc runs in the phase7 stack.
  Unit-tested (`TestER02_Signature*`, client tests) and fail-closed when unreachable (live).
- Caller-side gating (general-ledger, financial-close, vat-gst, corporate-tax) and the full
  envelope / allow-list in board-resolutions, corporate-actions, filing-preparation.
- Production mTLS to authorization-svc (`AUTHZ_MTLS_ENABLED`) was not exercised.

## Known deviations
SPEC_DEVIATIONS.md D-ER-1 … D-ER-28.
