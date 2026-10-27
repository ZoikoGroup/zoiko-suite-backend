# REF-05 cutover, phase 1 - financial-close-svc notes

Scope: financial-close-svc (ACC-14) stays AUTHORITATIVE for period state. Phase 1
adds (a) the provenance endpoint REF-05 calls back and (b) a best-effort
dual-write mirror of lock/reopen into accounting-period-svc (REF-05). Nothing here
changes live behaviour with the defaults. `GetPeriodStatus` still fails OPEN for an
unregistered period (phase 4 changes that, with separate sign-off).

## Flags (all default to the safe state)

| Variable | Default | Meaning |
|---|---|---|
| `PERIOD_SERVICE_MIRROR` | `off` | `on` enables the mirror and the replay endpoint. Any other value is treated as `off` (and logged). |
| `ACCOUNTING_PERIOD_URL` | `http://accounting-period-svc:8174` | REF-05 base URL. |
| `PERIOD_MIRROR_REOPEN_WINDOW` | `24h` | `expires_at` of a mirrored AUTHORIZE_REOPEN = now + this. Clamped to 72h (REF-05 `REOPEN_MAX_WINDOW` default); unparseable or <= 0 -> 24h. Logged at startup when adjusted. |
| `WORKFLOW_REF_CALLERS` | `accounting-period-svc` | Comma list of `X-Workload-Id` values allowed to call `GET /v1/close/workflow-refs/{ref}`. Empty list refuses everyone. |

## Decisions and deviations

1. **Who "requests" which step (SoD).** REF-05 refuses a HARD_CLOSE or
   AUTHORIZE_REOPEN requested by the same actor that requested the soft close.
   ACC-14 has one human action (Lock), so the mirror splits it: the SOFT_CLOSE step
   is attributed to the *workload* `financial-close-svc` (REF-05 actor = the
   `X-Workload-Id`, because no `X-Principal-Id` is sent on that call;
   `close_workflow_refs.requested_by = 'financial-close-svc'`), and the
   HARD_CLOSE / RECLOSE / AUTHORIZE_REOPEN step to the *human principal* who
   performed the local lock/reopen (`X-Principal-Id`, `requested_by = principal`).
   This satisfies REF-05's check mechanically; it is NOT an independent approval -
   one person's single Lock click produces both steps. The real independence
   control in phase 1 remains ACC-14's own authorization. Revisit before REF-05
   becomes authoritative.
2. **Prerequisite grants.** REF-05 authorizes `PERIOD_STATE_COMMAND` on the actor
   of every command: both the workload `financial-close-svc` and every human who
   locks/reopens/replays need that grant in authorization-svc, or each mirrored
   step fails closed (counted as `forbidden`).
3. **Reopen scope.** The reopen is mirrored with `reopen_scope: {}` = the entire
   period (REF-05 requires book/module scope to equal the period's own; an empty
   scope only matches entity-wide periods - a scoped period fails with
   `context_invalid`/`rejected`). ACC-14 has no scoped reopen, so none is invented.
4. **Period lookup.** REF-05 periods are found with
   `GET /v1/accounting-periods:resolve?legal_entity_id=&date=<UTC date of the legacy
   period_start>&purpose=read`. No book/module scope is sent. `PERIOD_NOT_FOUND`
   (404) or ambiguous (409) records a failure and stops. The legacy period is not
   cross-checked against REF-05's start/end (resolve does not return them); a
   calendar mismatch between the two services would mirror the wrong period.
5. **Legacy lock after a mirrored reopen = RECLOSE.** If REF-05 is
   `REOPEN_AUTHORIZED` when the legacy period is locked again, the mirror sends
   RECLOSE (requested by the human principal) instead of soft/hard close. If
   REF-05 is already HARD_CLOSED/RECLOSED the lock is a no-op (idempotent).
   A reopen when REF-05 is OPEN/SOFT_CLOSED cannot be mirrored (no such
   transition) and is recorded as failure `state_mismatch`.
6. **Refs are written first and kept.** A `close_workflow_refs` row (APPROVED) is
   committed in its own transaction *before* each REF-05 command, because REF-05
   calls back to verify it. If the command then fails the row stays (append-only;
   deleting a record of a decision that was presented is worse than keeping it). A
   retry creates a *new* ref; REF-05's own state, not the ref table, decides what
   is still to do. Idempotency-Key = `close-mirror-<ref_id>`.
7. **control_snapshot_ref.** `sha256(hex)` of the canonical JSON
   `{"v":1,"fiscal_period_id","command","evidence_document_id","readiness":{"is_ready","blocking_issues":[sorted]}|null}`
   (`internal/periodmirror/snapshot.go`; also in migration 000015). For a live
   lock the readiness is the (empty) blocking-issue list that passed the lock; for
   a *replay* it is recomputed at replay time (so a replay hash can differ from
   what a live close would have produced if readiness changed since); for a reopen
   it is `null`. If readiness cannot be computed during a replay the action is
   `failed: readiness_unavailable` and nothing is sent.
8. **Failed close evidence.** If the local lock succeeds but the close-evidence
   write fails (the existing 500 `evidence_not_recorded` path) the mirror is NOT
   run: that close is flagged "not evidenced". Use the replay endpoint once fixed.
9. **Mirror runs synchronously, bounded.** After the local lock/reopen, before the
   response, under a total deadline of 5s (3s per request), detached from the
   caller's cancellation. A hung REF-05 can therefore add up to ~5s to a lock/
   reopen response when the mirror is ON (never when off). The HTTP server
   WriteTimeout is 15s. Failures are logged at error level and counted; the
   local result is never changed. A panic in the mirror is recovered.
10. **Replay endpoint always answers 200 with per-action outcomes** (`applied` |
    `skipped` | `failed`, plus an additive `reason` on failures) - a REF-05 failure
    is NOT an HTTP error, so callers must inspect `actions`. Legacy `OPEN` => 200 with
    `actions: []`. 409 `period_mirror_disabled` when the mirror is off. It authorizes
    as `PERIOD_CLOSE_INITIATE` (same as lock). `Idempotency-Key` is required but not
    stored: idempotence comes from REF-05 state (a second run reports `skipped`).
    The response field is spelled `legacy_state`.
11. **A REJECTED ref** is served as 200 with `"status":"REJECTED"` (never APPROVED);
    nothing creates REJECTED rows yet. REF-05 treats any non-APPROVED as
    `SOURCE_UNVERIFIED`.

## Known risks

- The workflow-ref endpoint is a GET. Under the default `write-strict` envelope
  enforcement it is admitted without request-id/correlation/source-channel; if the
  deployment sets `ZS_ENVELOPE_ENFORCEMENT=strict`, REF-05's callback (which sends
  only `X-Tenant-Id` and `X-Workload-Id`) would be refused 400/401 and every mirrored
  command would fail closed `source_unverified`. `contract.go` is generated, so this
  was not changed here.
- The callback is authenticated only by the `X-Workload-Id` header (no mTLS in this
  path), the same trust model as the rest of the internal estate.
- REF-05 itself is not yet end-to-end tested against a real instance: the mirror
  was tested against an `httptest` stub written from REF-05's code and openapi.
