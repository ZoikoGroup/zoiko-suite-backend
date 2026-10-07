# SLO — accounting-period-svc

Spec profile: **C0/C1 posting gate, strongly consistent state, low-latency
read** (REF-05 NFR). These are *proposed* objectives. None has been load-tested
or measured; there is no production history behind them (see
RELEASE_CERTIFICATE.md).

| SLI | Objective (proposed) | Notes |
|---|---|---|
| Availability of `GET /v1/accounting-periods:resolve` (the gate) and `:status-by-key` | 99.99% / 30 days | Depends on Postgres only (primary). Not on authorization-svc, REF-04, ACC-14 or Kafka. A gate outage means posting stops (callers must treat non-200 as "do not post"), so this is the tightest target. |
| Gate latency | p99 < 25 ms at the service (excl. network) | One indexed range query on `(tenant_id, legal_entity_id, start_date, end_date)` in one short transaction. **Unmeasured.** |
| Gate staleness | 0: every answer reflects the latest commit | Primary read, `Cache-Control: no-store`. No replica or cache is allowed on this path. Reopen expiry is evaluated at read time. |
| Availability of other reads (`GET` period/list/history, `calendar-usage`) | 99.9% / 30 days | Postgres only. |
| Availability of state commands | 99.5% / 30 days | Fail closed if authorization-svc or ACC-14 is down; ACC-14 availability bounds this SLO. Until the ACC-14 endpoint exists, 0%. |
| State command latency | p99 < 500 ms | Includes one authorization-svc call (decision cached 5 s) and one ACC-14 call (3 s timeout). **Unmeasured.** |
| Materialise latency | p99 < 3 s for 12-60 periods | One REF-04 round trip plus one transaction. **Unmeasured.** |
| Event delivery lag (outbox) | oldest unpublished event < 30 s (p99) | `accounting_period_outbox_oldest_age_seconds`. |
| Event durability | 0 lost events | Transactional outbox; at-least-once delivery. `PeriodCommandRejected` is the exception: best effort (see SPEC_DEVIATIONS 5). |

## Alerts to wire (not wired in this change)

No Prometheus rules were added to the shared deployment config. Suggested:

- Gate availability: burn-rate on `http_requests_total{route="/v1/accounting-periods:resolve",status_code=~"5.."}`.
- `rate(accounting_period_gate_decisions_total{result="error"}[5m]) > 0` for 2m (page).
- `accounting_period_gate_decisions_total{result="ambiguous"}` increasing (ticket: periods need reconciling).
- `accounting_period_gate_decisions_total{result="not_found"}` spike (a calendar year was not materialised, or a caller sends wrong scope).
- `accounting_period_outbox_oldest_age_seconds > 120` (relay stalled).
- `accounting_period_commands_total{outcome="unavailable"}` sustained (ACC-14 or authorization-svc down).
- Any `PeriodCommandRejected` event with `rejection_code` in (`SOD_DENIED`, `SOURCE_UNVERIFIED`) routed to security monitoring.
