# SLO — fiscal-calendar-svc

Spec profile: **C1, high durability, as-of reconstruction** (REF-04 NFR). These
are *proposed* objectives. None has been load-tested or measured; there is no
production history behind them (see RELEASE_CERTIFICATE.md).

| SLI | Objective (proposed) | Notes |
|---|---|---|
| Availability of read endpoints (`GET /v1/fiscal-calendars*`, `periods-preview`, `:resolve`) | 99.95% / 30 days | Reads depend on Postgres only, not on authorization-svc, accounting-period-svc or Kafka. |
| Latency of `periods-preview`, `:resolve`, `GET /v1/fiscal-calendars/{id}` | p99 < 50 ms at the service (excluding network) | Each is 1-3 small indexed queries; `periods-preview` is pure date arithmetic over one stored version. **Unmeasured.** |
| Availability of commands | 99.9% / 30 days | Commands fail closed (503 `DEPENDENCY_UNAVAILABLE`) if authorization-svc is down; activation of a non-first version also fails closed if accounting-period-svc is down. |
| Command latency (create, propose, approve, plan) | p99 < 300 ms | Includes one authorization-svc call (decision cached 5 s). **Unmeasured.** |
| Activation latency | p99 < 1 s | Adds an advisory lock, 5-7 queries and, for non-first versions, one REF-05 call (5 s client timeout, so the worst case is above the objective). **Unmeasured.** |
| Event delivery lag (outbox) | oldest unpublished event < 30 s (p99) | `fiscal_calendar_outbox_oldest_age_seconds`. |
| Event durability | 0 lost events | Transactional outbox; at-least-once delivery. |
| Determinism of `periods-preview` | 100% byte-identical for the same (version, fiscal_year) | Pure function; covered by golden and property tests. A violation is a defect, not a budgeted error. |

## Alerts to wire (not wired in this change)

No Prometheus rules were added to `deployments/prometheus-rules.yml` (that file
belongs to shared deployment config outside this change). Suggested rules:

- `fiscal_calendar_outbox_oldest_age_seconds > 60` for 5 m (stalled relay).
- `rate(fiscal_calendar_outbox_failures_total[5m]) > 0` for 10 m.
- `rate(fiscal_calendar_commands_total{command="activate_version",outcome="unavailable"}[5m]) > 0`
  (authorization-svc, accounting-period-svc or the store is down: calendar
  changes are blocked, by design).
- `rate(fiscal_calendar_commands_total{command="activate_version",outcome="refused"}[1h]) > 0`
  (every refused activation - overlap, missing transition plan - deserves a
  human).
- `rate(fiscal_calendar_resolutions_total{result="not_found"}[5m])` spike vs
  baseline (a consumer is resolving dates no version covers: a gap in the
  calendar, or a missing activation).

## Recovery

RPO/RTO follow the platform Postgres policy; this service holds no state outside
Postgres. Versions, plans and status history are append-only or version-bumped
(never deleted), so a point-in-time restore is consistent. Outbox rows
unpublished at the restore point are re-delivered (consumers must be
idempotent). After a restore, reconcile against REF-05: every period it holds
must still point at a `version_id` that exists here, and `periods-preview` for
that version must reproduce its period boundaries.
