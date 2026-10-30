# ZS-JUR-001 s23, s29, s31: push invalidation of resolver caches, and the remediation workflow

Builds on [Wave 7](jurisdiction-pack-wave7.md) (release, rollout, rollback) and [Wave 2](jurisdiction-pack-wave2.md) (resolver). Implemented in `jurisdiction-rules-svc` (migrations `000025` and `000026`; four action codes registered in
`access-control-svc` migration `000028`). No new service and no new infrastructure.

## 1. Push invalidation of resolver caches
**The gap.** A withdrawal, emergency block, supersession, deployment, rollback, signing-key revocation or rollout suspension reached a running resolver only when its cache expired (`RESOLVER_CACHE_TTL_SECONDS`) or someone called
the local `resolver-cache:invalidate` endpoint on that one replica. With several replicas, an emergency block could keep being served by the others for the whole TTL.

**The fix.** Migration `000025` adds database triggers that publish a Postgres `NOTIFY` on channel `jur_resolver_invalidate` when anything a resolver caches changes: a pack version's status, a deployment, a signing key, a rollout's status.
Every replica `LISTEN`s and drops its cache at once.

* **Triggers, not application code**, so every path is covered, including a direct SQL change by an operator.
* **NOTIFY is delivered only when the transaction commits**, so a replica never invalidates for a change that was rolled back (tested).
* **It is a wake-up, not a queue.** A replica whose connection drops misses notifications, so the listener invalidates on **every (re)connect**, reconnects with backoff (1 s doubling to 30 s), and a change after a reconnect still reaches it (tested by killing the listener's connection).
* **Configuration:** `RESOLVER_PUSH_INVALIDATION` (default `true`). The listener holds one dedicated database connection outside the pool's normal use. **A pooler that multiplexes sessions per transaction (PgBouncer in transaction mode) cannot carry `LISTEN`**: with one,
  set it to `false` (the service then logs a warning) and rely on the TTL and the invalidate endpoint, or point the service at the database or a session-mode pooler.
* The cache TTL remains the backstop if a notification is ever lost.

## 2. The remediation workflow (JUR-NEG-19, s31)
**The gap.** Rolling back or withdrawing a version changes future resolution only. The decisions already produced with it are not rewritten (s31: "do not silently rewrite history; explicit remediation workflow"), and the rollback response said, in its own words, that this
workflow "does not exist".

**Model (migration `000026`).** A *remediation case* snapshots the decisions made with a version and tracks **one disposition per decision, with evidence**, until every decision is answered for and someone other than the opener closes it.

* **Opened automatically, in the same transaction**, by a **withdrawal** (every ring and region) and by a **rollback** (the rolled-back ring and region, decisions since that deployment began) when decisions exist. No decisions, no case. A repeat attaches to the case already open for the
  same version, trigger and scope. It can also be opened by hand (`POST /v1/admin/remediation-cases`) for an incident the automatic triggers do not cover; that is refused when no decision was made with the version in that scope. An emergency **block** does not open one (it is reversible pending
  incident review; open one by hand if the review concludes outcomes were wrong).
* **Dispositions** (`POST .../dispositions`, 1 to 100 per call, all or nothing): `RECONFIRMED` (re-checked, stands), `NOT_AFFECTED`, `RECALCULATED` and `SUPERSEDED_BY_NEW_DECISION`. Each needs an `evidence_ref`; the last two name `new_decision_id`, which must be a decision made with a **different**
  pack version than the one under remediation (redoing it with the same faulty version remediates nothing). They are **final**: repeating an identical one is a replay, a different one for the same decision is refused (409).
* **Closing** (`POST .../close`, with a note): refused while any decision has no disposition (409, with the count), and refused for **the person who opened the case** (403). Both rules are also enforced by database triggers, so a writer that bypasses the service is stopped (tested in SQL).
  Closing a closed case changes nothing; a closed case takes no more dispositions.
* **The decisions are never changed.** The evidence ledger rows stay append-only; the register only records who answered for each one.
* Reads: list cases (`?status=`), a case with its progress (total, dispositioned, pending, by disposition), and its decisions (`?pending=true`).
* Events: `jurisdiction-remediation.opened` (automatic and manual cases, once per case) and `jurisdiction-remediation.closed`.
* The withdraw and rollback responses carry `remediation_case_id`, `remediation_opened` and `remediation_decisions`; the rollback note now points at the case instead of saying the workflow is missing.

## What this does not do
* **It does not re-evaluate anything.** Recalculating an outcome is the job of the service that used it (tax, payroll, retention, ...). Those services must read their pending decisions here and record what they did. **None of them calls this yet**, so today the register is worked by people
  (or by a service's own tooling) through the API.
* **It does not tell the consumers.** `jurisdiction-remediation.opened` is the signal, but no consuming service subscribes yet.
* **Decision-to-business-object linkage** (which invoice or return a decision produced) is not here: the ledger records the decision, the caller's identity and the facts, not the caller's business object. A consumer that needs that must keep its own link from its records to `decision_id`.
* **No SLA or ageing** on open cases, and no escalation. An open case simply stays visible in the list.
* **Reconciliation** after a rollback is still not performed (the rollback response says so).

## Deploy order
Apply migrations `000025` and `000026` before the new binary, and `access-control-svc` `000028` so the actions are known. The `000026` down script drops the remediation tables (like every migration that created tables) and so **destroys case history: archive it first**; it must run before
the down of `000008`, which the tables reference. The `000025` down script only removes the triggers, leaving replicas on the TTL.

## Verification
Unit: disposition validation (every disposition, evidence, replacement rules, self-replacement, note length). Integration (embedded Postgres 16): a withdrawal opening a case sized to only the withdrawn version's decisions; progress and the pending list; refusal to close with decisions pending (with counts); every
malformed disposition; a decision outside the case; a replacement made with the same version; replay versus a different answer; the opener refused (403) and another closer accepted; close idempotent and announced once; a closed case refusing more dispositions; the decisions untouched; the manual case (idempotent, nothing-to-remediate, unknown
version); the database refusing a premature close, a self-close, edits, deletes and a disposition without evidence; the rollback test extended to assert the automatic case, scope and event; and, for push invalidation, two replicas with a one-hour cache where the listening one follows a block, an unblock and (after its connection is killed
and re-established) a withdrawal while the control without a listener keeps serving the stale answer, and a rolled-back transaction announcing nothing.
