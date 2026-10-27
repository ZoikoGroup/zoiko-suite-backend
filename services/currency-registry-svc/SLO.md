# SLO — currency-registry-svc

Spec profile: **C0 reference, high-cacheability** (REF-02 NFR). These are
*proposed* objectives. None has been load-tested or measured; there is no
production history behind them (see RELEASE_CERTIFICATE.md).

| SLI | Objective (proposed) | Notes |
|---|---|---|
| Availability of read endpoints (`GET /v1/currencies*`, `:validate`) | 99.95% / 30 days | Reads depend on Postgres only, not on authorization-svc or Kafka. |
| Latency of `:validate` and `GET /v1/currencies/{code}` | p99 < 50 ms at the service (excluding network) | Each read is 2-3 small indexed queries. Callers should cache (`ETag`, `Cache-Control: no-cache` = revalidate) but must not cache `:validate` (`no-store`). **Unmeasured.** |
| Availability of commands | 99.9% / 30 days | Commands fail closed (503 `DEPENDENCY_UNAVAILABLE`) if authorization-svc is down. |
| Command latency (`:activate` etc.) | p99 < 300 ms | Includes one authorization-svc call (decision cached 5 s). **Unmeasured.** |
| Import latency | 5,000 rows < 30 s | Row validation does ~2-3 queries per row; **unmeasured**, and the 15 s server write timeout (`WriteTimeout` in `cmd/server/main.go`) will cut a request off first if it is slower. |
| Event delivery lag (outbox) | oldest unpublished event < 30 s (p99) | `currency_registry_outbox_oldest_age_seconds`. |
| Event durability | 0 lost events | Transactional outbox; at-least-once delivery. |

## Alerts to wire (not wired in this change)

No Prometheus rules were added to `deployments/prometheus-rules.yml` (that file
belongs to shared deployment config outside this change). Suggested rules:

- `currency_registry_outbox_oldest_age_seconds > 60` for 5 m (stalled relay).
- `rate(currency_registry_outbox_failures_total[5m]) > 0` for 10 m.
- `rate(currency_registry_imports_total{outcome="quarantined"}[1h]) > 0`
  (every quarantine deserves a human).
- `rate(currency_registry_commands_total{outcome="unavailable"}[5m]) > 0`
  (authorization-svc or store trouble).
- `rate(currency_registry_validations_total{result="not_supported"}[5m])` spike
  vs baseline (a consumer may be sending unknown codes, or a currency was
  restricted/retired).

## Recovery

RPO/RTO follow the platform Postgres policy; this service holds no state outside
Postgres. All tables are append-only or version-bumped, so a point-in-time
restore is consistent. Outbox rows unpublished at the restore point are
re-delivered (consumers must be idempotent).
