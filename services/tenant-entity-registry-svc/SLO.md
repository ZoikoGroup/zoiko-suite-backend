# tenant-entity-registry-svc — Service Level Objectives

ORG §4.2 NFR profile: "C0 isolation context; strong consistency for
status/home-region pointer; durable lifecycle evidence". §4.3: "C1
master-data authority; bitemporal reconstruction; high durability".
§9.2 gate 7 requires SLOs, alerts, backup/restore and reconciliation.

Alert rules implementing these: `deployments/alerts/tenant-entity-registry.rules.yml`.
Response procedures: `RUNBOOK.md`.

## 1. Availability — 99.9% monthly

| | |
|---|---|
| SLI | `1 − (5xx requests / all requests)` from `http_requests_total` |
| Objective | 99.9% over 30 days (≈ 43 min of 5xx-equivalent budget) |
| Alerts | `TenantRegistryNotReady` (readiness), `TenantRegistryAvailabilityBurn` (14.4× burn over 1h) |
| Not counted | 4xx. A fail-closed 503 because authorization-svc or jurisdiction-rules-svc is down **is** counted — it is unavailability to the caller even when correct. |

Tenant resolution (`GET /v1/resolve-tenant`) is on the C0 isolation path of every
request in the estate; it is the first thing to verify in any incident.

## 2. Latency — reads

| | |
|---|---|
| SLI | `http_request_duration_seconds`, method GET |
| Objective | p99 < 300 ms, p50 < 50 ms, measured over 10 min |
| Alert | `TenantRegistryReadLatency` |

Writes carry synchronous dependency calls (authorization, jurisdiction,
entitlement), each with a 2 s client timeout; they have no latency SLO of their
own, only availability.

## 3. Event delivery

Every event is written to `event_outbox` in its write's transaction (since
28 Sep 2026 there is no other path), so delivery is measurable end to end.

| | |
|---|---|
| SLI | `outbox_pending_events`, `outbox_dead_letter_events` |
| Objective | an event is published within 60 s of commit, 99.9% of the time; zero dead letters |
| Alerts | `TenantRegistryOutboxBacklog` (>100 pending for 5 min), `TenantRegistryOutboxDeadLetters` (any) |
| Semantics | at-least-once; consumers dedupe on `event_id` and pin `object_version` |

## 4. Durability and recovery

| | Objective | Evidence |
|---|---|---|
| RPO | ≤ 5 min (managed Postgres continuous WAL archiving / PITR) | platform setting — not verifiable from this repo |
| RTO | ≤ 30 min to a verified restore | `scripts/backup_restore_drill.sh`: logical restore of the local database in 4 s, 22/22 fidelity checks (row counts, FORCE RLS, every policy and named capability, control constraints, history invariants) — 28 Sep 2026 |
| History | no silent rollback (§8 NP48): after a restore, run `scripts/audit.sh` §13 (profile-history coherence) and re-drive the outbox; delivery is at-least-once, so re-publication is safe |

## 5. Correctness objectives (zero-tolerance, checked, not sampled)

| Invariant | Check |
|---|---|
| No entity has two open-ended profile versions | `scripts/audit.sh` §13; drill §3 |
| No approval decided by its requester | constraint `ar_no_self_decision` |
| No home-region change without decision evidence and an approver | constraint `tlh_home_region_evidenced` |
| No event stuck past 8 attempts | `scripts/audit.sh` §9 |

## What is not certified here

Production certification needs the production platform: the RPO depends on the
managed database's WAL settings, and the RTO above is a local logical restore of
a small database, not a production-sized point-in-time recovery. Both must be
re-measured in the target environment before go-live.
