# SLO — financial-statements-svc

Spec profile: REP-01 (ZS-SVC-H-001 §4.1), "C1 authoritative reporting;
deterministic rerun; certified instance immutable; source-to-report lookup
p95 target <2s for indexed lineage." These are *proposed* objectives. None
has been load-tested or measured; there is no production history (see
RELEASE_CERTIFICATE.md).

| SLI | Objective (proposed) | Notes |
|---|---|---|
| Availability of reads (`GET /v1/statement-sets`, `/{id}`, `/{id}/facts`) | 99.9% / 30 days | Postgres only. |
| `ExplainReportedFact` latency | p95 < 2s | Spec's own named target for source-to-report lookup. One indexed row read per call. **Unmeasured.** |
| Availability of `CreateStatementSet` / `:refresh-draft` | 99.5% / 30 days | Depends on financial-close-svc (ACC-16) and authorization-svc; fails closed on either being down, which bounds this below pure-Postgres availability. |
| Availability of `:validate` / `:submit-for-review` / `:approve` / `:certify` / `:publish` | 99.9% / 30 days | Postgres + authorization-svc only; no financial-close-svc round trip once facts already exist. |
| Command latency | p99 < 500 ms | Includes one authorization-svc call. **Unmeasured.** |
| `CreateStatementSet` / `:refresh-draft` latency | p99 < 2s | Includes one financial-close-svc round trip (5s client timeout). **Unmeasured.** |
| Event delivery lag (outbox) | oldest unpublished event < 30s (p99) | `outbox_oldest_backlog_age_seconds{service="financial-statements-svc"}`. |
| Event durability | 0 lost events | Transactional outbox; at-least-once delivery. |
| Certified-content immutability | 100% | No code path updates a CERTIFIED or PUBLISHED set's facts — enforced structurally (no handler or store method exists for it), not by a runtime check. |

## Alerts to wire (not wired in this change)

No Prometheus rules were added to the shared deployment config. Suggested:

- `outbox_oldest_backlog_age_seconds{service="financial-statements-svc"} > 120` (relay stalled).
- `outbox_quarantined_events{service="financial-statements-svc"} > 0` (needs an owner's decision).
- Burn-rate on `http_requests_total{route=~"/v1/statement-sets.*",status_code=~"5.."}`.
- A spike in `SOURCE_UNVERIFIED` or `SOD_DENIED` error codes on statement-set commands routed to security monitoring.
