# financial-control-svc: ZS-CONTROL-001 Waves 7 and 8, integration and conformance

This file uses the **document's** wave numbers (ZS-CONTROL-001 s33): Wave 7 = continuous monitoring, dashboards,
recurrence/root-cause metrics; Wave 8 = scale, DR, evidence export, external-audit support. The earlier file
`financial-control-wave7.md` describes exception-resolution work that belongs to the document's Waves 1 and 5.

## Wave 7: monitoring

`GET /controls/v1/monitoring/metrics?legal_entity_id=&period_id=` (period optional; action `FINCTRL_READ`).
Computed on read from authoritative rows, over the **latest run of each control** so a superseded run is never
double counted. The s28 signals:

| s28 signal | Field |
|---|---|
| Control completion rate | `control_completion`, `rates.control_completion_rate`, `rates.control_certification_rate` |
| Run duration / lag | `run_duration_and_lag` (avg, max, oldest never-started run) |
| Exception count and amount | `exceptions`, `unresolved_by_assertion`, `unresolved_by_category` (exact amounts per currency) |
| Exception aging | `unresolved_exception_aging` (7 / 30 / 90 / older), `exceptions.sla_breached` |
| Recurring exception rate | `exceptions.recurring`, `rates.recurring_exception_rate` |
| Late-data rate | `exceptions.late_data`, `rates.late_data_rate` |
| Certification latency | `certification_latency` (READY_TO_CERTIFY to CERTIFIED) |
| Control failure / indeterminate rate | `rates.failure_or_indeterminate_rate` |
| Root causes | `root_causes` (latest root cause per exception) |
| Manual-match rate | **unavailable**: matching is fully automatic, no manual-match action exists |
| Suspense aging / value | **unavailable**: FIN-CTRL-031 is BLOCKED |

Rates are `null` when the denominator is zero ("no data" is not 0%). Unavailable signals are listed with their reason.
This is the data feed for a command center; **no user interface exists in this service or was built.**

Root cause (s21): resolving (REMEDIATED), waiving or carrying forward a HIGH-severity or **recurrent** exception
requires `root_cause_code` (UPPER_SNAKE) and `root_cause_note`. Recurrent = the same control, reason code and an
overlapping record already raised earlier for the entity. The taxonomy is a controlled decision (s34), so the code is
validated for shape only; no code list was invented.

## Wave 8: evidence export and operations

`GET /controls/v1/runs/{id}/evidence-export` (action `FINCTRL_EVIDENCE_EXPORT`, separate from read because an export
leaves the platform): run and state history, control definition, the pinned rule version, frozen population summaries,
every exception with authority / evidence / root-cause history, the sealed evidence package (re-verified at export).
`manifest.digest` is the canonical sha256 of the bundle without the manifest, so a recipient recomputes it to detect
alteration; `manifest.evidence_integrity_verified` is false if the stored package no longer matches its seal.

Scale, as built: keyset paging everywhere; a 200,000-record cap per population (fails closed above it); exceptions
listed 200 per page in the export; sweeper batches of 200; per-tenant RLS plus explicit predicates.
DR, as built: all state is in Postgres (backup and restore are the platform's); events go through a transactional
outbox so unpublished facts survive a broker outage and are relayed on recovery; evidence packages verify themselves
and the export verifies offline. **Numeric RPO/RTO, SLOs and load targets are controlled decisions (s34
"Continuous-control SLOs") that the specification leaves open, so none is asserted here and none was load-tested.**

## Integration

* **Authorization actions.** Added to `deployments/scripts/seed-demo-rbac.ps1` as bundles `FINCTRL_FULL` (all
  `FINCTRL_*`, including `FINCTRL_EVIDENCE_EXPORT`) and `FINCTRL_POPULATION_READ` (the eleven source population
  actions). authorization-svc needs no code change: action codes are free-form strings in permission bundles. Real
  tenants should grant DEFINE / APPROVE_RULE / EXECUTE / CERTIFY / WAIVE / REPERFORM through separate roles.
* **financial-close-svc.** `checkReadiness` calls `GET /controls/v1/close-gate` when `FINCTRL_CLOSE_GATE_MODE=enforce`
  (default `off`, a strict no-op). Fail-closed on an unreachable service or an unconfigured entity. The control run's
  `period_id` must equal the close period's `PeriodName`.
* **Events (ZS-EVENT-001).** The envelope is the platform's canonical wrapper (`event_type`, `event_version`,
  `emitted_at`, `schema_version`, `source_service`, `correlation_id`, `tenant_id`, `legal_entity_id`, `actor_id`,
  `payload`), byte-compatible with every other producer and with audit-event-store-svc, which parses it. ZS-EVENT-001
  attributes are added **as extra fields only**: `event_id` (UUIDv7, the stable outbox id), `aggregate_type`,
  `aggregate_id`, `aggregate_version` (when the payload carries a version), `published_at`, `payload_hash`,
  `residency_region`, `classification`; `emitted_at` is the commit time; the Kafka key is a hash of tenant and
  aggregate; `correlation_id` is generated when a caller sends none. The CloudEvents naming ZS-EVENT-001 prescribes
  (`specversion`, `type`, `com.zoikosuite.*`, `source` URN) is **not applied**: no other producer or consumer in the
  repository uses it, and one service adopting it would become unreadable to the audit store. That is a
  platform-wide decision (see `financial-control-closure.md`). `residency_region` and `classification` publish
  `UNSPECIFIED` unless `EVENT_RESIDENCY_REGION` / `EVENT_CLASSIFICATION` are set. Nothing consumes these events yet.
* **State machines (ZS-STATE-001).** ZS-STATE-001 s9.6 defines a generic reconciliation lifecycle and has **no**
  control-run, certification or exception-item machine. The run/certification/exception states here come from
  ZS-CONTROL-001 s7 and s21 and are a superset. Nearest mapping: MATCHING to EXECUTING, EXCEPTIONS to
  EXCEPTION_REVIEW, REVIEW_PENDING to READY_TO_CERTIFY, SIGNED_OFF to CERTIFIED, LOCKED to CERTIFIED/SUPERSEDED.
  ZS-STATE-001 needs an entry for these machines; no states were added or removed here.
* **Accounting kernel.** See the general-ledger-svc changes reported for ZS-ACC-KERNEL-001 (reversal ledger entries and
  mandatory event fields).
