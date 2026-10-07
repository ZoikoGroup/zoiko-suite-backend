# financial-control-svc — Wave 5 (accounting kernel, certification, close gate)

## New check kind: `EXCEPTION_SCAN`

The population *is* the finding set. Used when the source can already answer "which records violate
the policy" (direct posting to a restricted control account, an event that never became a journal).

| Rule-logic field | Meaning |
|---|---|
| `finding_category` | one of CLASSIFICATION, AUTHORIZATION, LATE_DATA, DATA_QUALITY, MISSING, MISMATCH, TIMING |
| `finding_reason` | `UPPER_SNAKE_CASE` reason code stored on each exception |
| `finding_assertion` | assertion the finding is against |
| `conditions[]` | optional `{attr, op: nonempty\|empty\|eq\|ne, value, detail}`; a record is a finding if **any** holds. No conditions = every record is a finding |

An empty population passes, so the source endpoint must be complete for its scope; completeness is
proven the usual way (watermark, declared totals, frozen population hash). Exposure = `abs(amount)`.

## Catalogue controls (`POST /controls/v1/catalogue/seed {"wave":5,"effective_from":…}`)

| Control | Source population (general-ledger) | Check | Run scope |
|---|---|---|---|
| FIN-CTRL-020 Journal debit-credit balance | `journal-balances` | ARITHMETIC `debit_total = credit_total` | `{"fiscal_period"}` |
| FIN-CTRL-021 Control-account direct posting | `control-account-postings` | EXCEPTION_SCAN | `{"fiscal_period"}` |
| FIN-CTRL-022 Unposted accounting events | `unposted-events` | EXCEPTION_SCAN | `{"created_before"}` |
| FIN-CTRL-023 Manual journal independent review | `manual-journals` | EXCEPTION_SCAN on `review_gap` | `{"fiscal_period"}` |

020, 021 and 023 are KEY and `close_gating`. 021/022/023 are PARTIAL against the catalogue wording;
the status report says exactly what each does not cover.

## Certification — `POST /controls/v1/runs/{run_id}/certification`

Body `{"decision":"CERTIFY"|"REJECT","reason":"…"}`; `If-Match` is mandatory; action `FINCTRL_CERTIFY`
against the run's own legal entity.

* Only a `READY_TO_CERTIFY` run (result PASS / PASS_WITH_APPROVED_EXCEPTIONS) can be decided.
* **Maker-checker:** the certifier may not be the run's creator nor appear anywhere on its transition
  history (executor, exception assigner…). Violation → `403 segregation_of_duties`.
* CERTIFY → lifecycle `CERTIFIED`, certification `CERTIFIED`. REJECT → lifecycle `EXCEPTION_REVIEW`,
  certification `REJECTED`, event `control.run.certification_rejected`.

## FIN-CTRL-041 — `GET /controls/v1/close-gate?legal_entity_id=&period_id=`

Open only when every `close_gating` control's **latest** run for that entity and period is CERTIFIED.
An earlier certified run does not survive a later failed or open one. Fail-closed: no configured
mandatory control ⇒ `open:false, configured:false`. financial-close-svc does not call this yet.

## FIN-CTRL-042 — `GET /controls/v1/exception-summary?legal_entity_id=&period_id=`

Unresolved exceptions (not CLOSED / WAIVED / CARRIED_FORWARD) of each control's latest run, per
currency and severity, against the entity's `aggregate_threshold`. Exposure from different controls can
describe the same issue, so the total is an upper bound. Currencies other than the policy currency are
listed as `unassessed_currencies` (no FX basis is assumed); with no policy `aggregate_material` is `null`.

## Still BLOCKED / not started

| Control | Reason |
|---|---|
| FIN-CTRL-019 | `PostAccountingEvent` cannot post today (insert fails on `transaction_date`, 503); no event population |
| FIN-CTRL-024 | GL has no record of period closure (lives in financial-close-svc) |
| FIN-CTRL-031 | no suspense designation on the chart of accounts, no item-level clearing |

New authz actions to grant: `FINCTRL_CERTIFY` (plus the population action already in use).
