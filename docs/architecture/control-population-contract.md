# Control Population Contract (ZS-CONTROL-001 §9)

`financial-control-svc` never reads another service's database and never trusts a
headline total. To test a financial population it asks the **owning service** for the
records, under a contract every source service implements once:

```
GET {service}/v1/control-populations/{population}
      ?legal_entity_id=<uuid>&period_id=<optional YYYY-MM>&limit=<n>&cursor=<opaque>
      &<population-specific params, see below>
Headers forwarded from the verified caller: X-Tenant-Id, X-Principal-Id,
                                            X-Legal-Entity-Id, X-Correlation-ID

200 {
  "records": [
    { "record_id": "…", "reference": "…", "amount": "1250.50",
      "currency": "USD", "date": "2026-09-01", "attributes": { "k": "v" } }
  ],
  "next_cursor": "",            // empty on the last page
  "watermark": "…",             // REQUIRED, identical on every page of one extraction
  "declared_totals": {          // REQUIRED for Wave 2 sources
    "row_count": 1234,
    "totals": { "USD": "98765.43" }
  }
}
```

A *population name* is `a-z 0-9 _ -`. A control definition references it as
`{"system": "<registered name>", "population": "<name>", "params": {"k": "v"}}` in
`source_spec` (side A) and `target_spec` (side B). `params` keys are `a-z_` (max 32 chars),
values max 512 chars; the reserved names `legal_entity_id`, `period_id`, `limit`, `cursor`
cannot be used. **A source must reject (400) any query parameter it does not define** —
a misspelt filter must never silently widen or narrow a population.

## Rules every source must honour

| Rule | Why |
|---|---|
| **`record_id` is the source's stable authoritative id.** | Identity of the tested record; duplicates are detected on it. |
| **`reference` is the business key both sides share.** | Records are paired on `reference`. |
| **`amount` is a decimal string, never a JSON number.** | Money is never binary floating point (ZS-DATA-001 D06). |
| **`date` is `YYYY-MM-DD`.** | |
| **Ordering & paging are keyset on `record_id` ascending;** `cursor` is opaque (encode the last `record_id`). Never OFFSET. | A row inserted mid-extraction must not shift pages. |
| **`watermark` changes when — and only when — the dataset in scope changes**, and is identical on every page of one extraction. Where the service has a commit sequence, use it. Otherwise use a content digest computed by the database over the WHOLE in-scope set (e.g. `md5(string_agg(id||'\|'||status||'\|'||amount, ',' ORDER BY id))` plus the count). | Reproducibility (Invariant 3). If the data changes between page 1 and page 2 the watermark changes and the control fails closed. |
| **`declared_totals` covers the WHOLE in-scope population, not the page,** and is computed by the database in the same statement/transaction as the watermark. | Lets the control prove the transfer was complete (FIN-CTRL-033). |
| **Tenant and entity scoping is applied by the source** from the forwarded verified headers, with an explicit `tenant_id` predicate in SQL (not RLS alone), and the caller must be authorized for the entity. `legal_entity_id` is mandatory. | Cross-tenant population construction must be blocked (scenario 28). |
| **Return every record in scope, including cancelled/reversed/paid ones**, marked in `attributes`. | A source that pre-filters hides the omissions the control exists to find (scenario 13). |
| **`period_id` is an "as of the end of that month" cut-off** for balance populations (all records dated on/before the last day of the month). Absent means no date cut-off. | Balance tie-outs are cumulative. |
| **Hard cap: 200,000 records.** Above it the source returns 422, never a truncated page. | No silent truncation. |
| **Read-only.** | The control framework never mutates source state (Invariant 6). |

## How the control treats failures

The control **fails closed**. Any of the following ends the run `FAILED` with result
`INDETERMINATE` — never `PASS`: source unreachable / non-200 / unreadable body; system not
registered in `SOURCE_ENDPOINTS`; no `watermark` or one that changed between pages;
`declared_totals` not tying to the received records; an invalid record; more than 200,000
records.

## Wave 2 population definitions

### accounts-receivable-svc — `open-invoices`

One record per customer invoice with `created_at::date <= period end`.

| Field | Value |
|---|---|
| `record_id` | `invoice_id` |
| `reference` | `invoice_number` |
| `amount` | **outstanding** amount as of the cut-off: `0` when `status = PAID` and `payment_received_at::date <= cut-off`, otherwise `amount` |
| `currency` | `currency_code` |
| `date` | `created_at::date` |
| `attributes` | `status`, `customer_id`, `due_date`, `invoice_amount` (original amount) |

### accounts-payable-svc — `open-invoices`

One record per vendor invoice with `created_at::date <= period end`.

| Field | Value |
|---|---|
| `record_id` | `invoice_id` |
| `reference` | `invoice_number` |
| `amount` | invoice total (positive liability) |
| `currency` | invoice currency |
| `date` | `created_at::date` |
| `attributes` | `status`, `vendor_id`, `due_date` |

AP invoices have no paid state today, so every invoice is reported at its total; that is
truthful and any settled liability shows up as a difference — the control is *supposed* to
surface it until AP records settlement.

### general-ledger-svc — `account-postings`

One record per **posted** ledger entry (`ledger_entries`, i.e. FINALIZED journals only) on
the requested accounts, dated on/before the cut-off.

Required param `account_codes` (comma list, 1–20 codes). Required param `normal_balance`
= `DEBIT` or `CREDIT`: `amount = debit − credit` for `DEBIT`, `credit − debit` for `CREDIT`
(so a receivable and a payable both report a positive open balance).

| Field | Value |
|---|---|
| `record_id` | `journal_line_id` |
| `reference` | the journal header's `source_event_id`; when empty, `journal:<journal_id>` — so an unattributable posting can never falsely match a subledger record and surfaces as an exception |
| `amount` | per `normal_balance` above |
| `currency` | `currency_code` |
| `date` | `transaction_date` |
| `attributes` | `account_code`, `journal_id`, `fiscal_period`, `book_id` |

**Convention:** a journal posted for a subledger document carries that document's
business reference (`invoice_number`) in `source_event_id`. Several ledger lines may share
one reference (invoice, credit note, cash application); the control's group matching nets
them per reference.

### banking-connector-svc — `bank-transactions`, `bank-statements`

`bank-transactions`: one record per canonical bank transaction of the entity (optional param
`bank_account_id`), dated on/before the cut-off. `record_id` = canonical transaction id,
`reference` = bank reference, `amount` signed (credits to the account positive), `date` =
booking date, `attributes` = `bank_account_id`, `statement_id`.

`bank-statements`: one record per ingested statement. `record_id` = statement id,
`reference` = `<bank_account_id>|<statement_date>`, `amount` = closing balance,
`date` = statement date, `attributes` = `bank_account_id`. Used for statement-day coverage.

## What is stored

For each side the control freezes an **append-only** snapshot: row count, per-currency
control totals, `sha256` population hash, the source `watermark`, and every record. The
snapshot is referenced by the sealed evidence package. Data arriving later at the source
never changes a frozen population; it is picked up by a **new run**.

## Implementation status (Wave 2)

| Source | Populations | Authz action (must be granted to callers of the control) | limit default / max |
|---|---|---|---|
| accounts-receivable-svc | `open-invoices` | `AR_CONTROL_POPULATION_READ` | 1000 / 5000 |
| accounts-payable-svc | `open-invoices` | `AP_CONTROL_POPULATION_READ` | 1000 / 10000 |
| general-ledger-svc | `account-postings` | `GL_CONTROL_POPULATION_READ` | 1000 / 5000 |
| banking-connector-svc | `bank-transactions`, `bank-statements` | `BANKING_CONTROL_POPULATION_READ` | 1000 / 5000 |

**The source authorizes the human caller, not the control service.** `financial-control-svc`
forwards the verified `X-Principal-Id` of whoever executed the run, so that principal needs the
source's read action for the entity as well as `FINCTRL_EXECUTE`. None of these actions is seeded
in authorization-svc yet.

Implementation notes and deviations from the ideal contract, as built:

- **Watermarks** are `<count>:<md5 digest of the ordered in-scope set>` for AR/AP/banking. The GL
  uses `gl1:n=<count>;entry_seq=<max>;journal_seq=<max>;md5=<digest>` because it has real
  sequences. All are computed in the same `REPEATABLE READ` snapshot as the page.
- **banking-connector** validates `legal_entity_id` / `bank_account_id` as `[A-Za-z0-9][A-Za-z0-9_.:-]{0,63}`
  (its ids are VARCHAR, e.g. `le-101`), not as UUIDs. Its `amount` sign follows the statement
  invariant (opening + Σlines = closing), so credits are positive; the sign is supplied by the
  caller at normalisation time and is not otherwise enforced by the schema.
- **banking-connector returns `SUPERSEDED` re-normalised rows** (nothing is pre-filtered). The
  bank control therefore carries an explicit, owned exclusion in its pinned rule
  (`status = SUPERSEDED`, authority `TREASURER`); the exclusion's count, value and a digest of the
  excluded ids are recorded on the frozen snapshot and in the evidence package.
- **AP has no paid state**, so every invoice is reported at its total; settled liabilities appear
  as differences until AP records settlement.
- **AR does not post to GL and GL has no AR-specific link.** Matching AR to GL relies on the
  convention that a journal posted for an invoice carries the invoice number in `source_event_id`.
  Where that convention is not followed, the ledger lines surface as `MISSING_SOURCE` exceptions.
- banking-connector runs in `docker-compose.phase7.yml`, not the main compose file.

## Wave 3 population definitions

**Non-monetary populations** (quantities) use currency `XXX` (ISO 4217 "no currency"); the
unit of measure goes in `attributes.uom`. Control totals for `XXX` therefore sum mixed units and
are informational only — matching is per record.

**Flow populations vs balance populations.** The Wave 2 `period_id` rule ("all records on/before
the end of the month") is right for *balance* populations (open invoices, ledger postings).
Payroll populations are *flow* populations: `period_id` selects records whose **pay date falls in
that calendar month exactly**.

### payroll-run-svc — `pay-slips`, `payroll-runs`  (personal data: no names, ever)

Required param `measure` = `gross` | `net` selects what `amount` carries.

`pay-slips` — one record per pay slip:

| Field | Value |
|---|---|
| `record_id` | `slip_id` |
| `reference` | `run_id` (so slips group under their run) |
| `amount` | `gross_pay` or `net_pay` per `measure` |
| `currency` | slip `currency` |
| `date` | run `pay_date` |
| `attributes` | `run_id`, `run_status`, `is_shadow_run` (`true`/`false`), `employee_number`, and ALWAYS all four components as decimal strings: `gross_pay`, `tax_withheld`, `benefits_deductions`, `net_pay`. **Never** an employee name or contact detail. |

`payroll-runs` — one record per run:

| Field | Value |
|---|---|
| `record_id` / `reference` | `run_id` |
| `amount` | the run's stored `total_gross_pay` or `total_net_pay` per `measure` (the *declared* control total) |
| `currency` | the single currency of the run's slips; `XXX` if the run has no slips or mixes currencies |
| `date` | `pay_date` |
| `attributes` | `status`, `is_shadow_run`, `employee_count`, `snapshot_hash` |

Every status and shadow runs are returned; the control excludes non-final and shadow runs by an
explicit, evidenced exclusion rule.

### inventory-management-svc — `stock-count-lines`

Required params: `count_id` (uuid) and `side` = `book` | `physical`.

| Field | Value |
|---|---|
| `record_id` | `line_id` |
| `reference` | `<count_id>\|<item_id>\|<location_id>` |
| `amount` | `side=book`: the **book quantity as of the count cut-off**, computed from committed movements (`GetOnHandAsOf(cutoff_at)`) — deliberately NOT the frozen `system_quantity`, so tampering with it is detectable. `side=physical`: `observed_quantity`; a line with no observation is **omitted** from this side (so it surfaces as a missing counted line). |
| `currency` | `XXX` |
| `date` | the count `cutoff_at` date |
| `attributes` | `uom`, `item_id`, `location_id`, `count_status`, `line_status`, `variance_approved` (`true`/`false`), `adjustment_movement_id`, `frozen_system_quantity` |

## Implementation status (Wave 3)

| Source | Populations | Authz action | limit default / max |
|---|---|---|---|
| payroll-run-svc | `pay-slips`, `payroll-runs` | `PAYROLL_CONTROL_POPULATION_READ` | 1000 / 5000 |
| inventory-management-svc | `stock-count-lines` | `INVENTORY_CONTROL_POPULATION_READ` | 1000 / 5000 |

- **payroll-run-svc** returns `employee_number` only (no names, contact or bank details) and treats
  `period_id` as an exact pay-date month. `legal_entity_id` is validated as
  `[A-Za-z0-9][A-Za-z0-9_.:-]{0,63}` (its column is VARCHAR). Reads do not require `X-Purpose-Context`
  (the envelope requires it on writes only).
- **inventory-management-svc** validates `legal_entity_id` and `count_id` as UUIDs (per this contract),
  although its `legal_entity_id` column is VARCHAR(255): an entity with a non-UUID id cannot use the
  endpoint. It rejects `period_id` (a count is identified by `count_id`, not a period). Book quantity is
  computed in SQL from committed movements dated on/before the count cut-off, in the same snapshot.
- **Per-run params.** A population param of the form `${scope.<key>}` is bound from the *run's* scope
  when the run executes (e.g. `{"count_id": "${scope.count_id}"}` + run scope `{"count_id": "…"}`), so a
  control definition stays static and reviewable while the per-run value comes from the run. A run
  that lacks the required scope key ends `FAILED / INDETERMINATE`.

### Known gaps that block Wave 3 controls (see `GET /controls/v1/catalogue/status`)

- **general-ledger-svc `POST /v1/postings/events`** (updated in the closure pass): the GL now requires
  `transaction_currency` and `document_date` (`posting_date` optional) and stores them on the journal and its
  ledger entries. Callers that cannot supply a real currency and business date are refused with 400; see
  `financial-control-closure.md` s1 for the per-caller position (only asset-management's asset-event apply was
  fixed; depreciation, consolidation, inventory, project-accounting and treasury postings need the missing data).
- **financial-close-svc** creates journals via `POST /v1/journals` without `journal_type`,
  `transaction_date`, `posting_date` or `currency_code`, which the GL requires (by code inspection; not
  run), and uses `correlation_id` rather than `source_event_id`.

## Wave 4 population definitions

**Lifetime populations.** The three leg populations below (`entry-legs`, `journal-account-totals`) are
*lifetime* populations: they take **no** `period_id` (reject it as unknown). An intercompany entry's
`created_at` and its journal's `transaction_date` are unrelated clocks, so a month cut-off applied to
each side would manufacture false omissions. `trial-balance` and `balance-contributions` are scoped by
an explicit `fiscal_period` / `run_id` instead.

**Amounts are read from the database as exact `NUMERIC` text — never through a Go `float64`** (the
intercompany and consolidation services use `float64` internally; the population endpoints must not
inherit that).

### intercompany-accounting-svc — `entry-legs`

Required param `leg` = `source` | `target`. `legal_entity_id` is the entity the leg belongs to.

| Field | Value |
|---|---|
| scope | `leg=source`: entries where `source_legal_entity_id = legal_entity_id`. `leg=target`: entries where `target_legal_entity_id = legal_entity_id` **and `target_journal_id IS NOT NULL`**. All statuses returned. |
| `record_id` | `intercompany_entry_id` |
| `reference` | the **journal id of that leg**: `source_journal_id` (source leg) / `target_journal_id` (target leg), lower-case canonical text |
| `amount` | the entry `amount` |
| `currency` | `currency_code` |
| `date` | `created_at::date` (UTC) |
| `attributes` | `intercompany_entry_id`, `match_status`, `leg`, `counterparty_entity_id`, `source_journal_id`, `target_journal_id` (empty if none) |

### general-ledger-svc — `journal-account-totals`, `trial-balance`

`journal-account-totals` — required params `account_codes` (1–20) and `normal_balance` (`DEBIT`|`CREDIT`);
posted `ledger_entries` on those accounts for the entity, grouped **by journal and currency**:

| Field | Value |
|---|---|
| `record_id` | `<journal_id>:<currency_code>` |
| `reference` | `journal_id` (canonical lower-case text) |
| `amount` | Σ(debit − credit) for `DEBIT`, Σ(credit − debit) for `CREDIT`, over that journal's lines on the requested accounts |
| `currency` | `currency_code` of those entries |
| `date` | the journal's `transaction_date` (the max over the grouped entries) |
| `attributes` | `journal_id`, `fiscal_period`, `account_codes` (the distinct accounts hit, comma separated), `entry_count` |

A journal that posts in two currencies yields two records with the same `reference` — deliberately, so
the control reports a currency mismatch instead of netting across currencies.

`trial-balance` — required param `fiscal_period` (exact text, ≤ 20 chars; the caller supplies it, this
contract does not assume a `YYYY-MM` format); every posted `ledger_entries` row for the entity in that
`fiscal_period`, grouped by account and currency:

| Field | Value |
|---|---|
| `record_id` | `<account_code>:<currency_code>` |
| `reference` | `account_code` |
| `amount` | Σ(debit − credit) — signed, exact |
| `currency` | `currency_code` |
| `date` | max `transaction_date` in the group |
| `attributes` | `account_code`, `fiscal_period`, `entry_count`, `max_entry_seq` |

It includes **all** posted ledger entries of the period (originals of reversed journals and their
reversals both), because that is what the ledger holds. Watermark includes `max(entry_seq)`. It is a
**read-only** query: it must never call the trial-balance *compile* route, which inserts a snapshot.

### consolidation-svc — `balance-contributions`

Required param `run_id` (uuid of a consolidation run of this tenant; unknown → 404).
`legal_entity_id` is the **child entity** (`source_legal_entity_id`).

| Field | Value |
|---|---|
| `record_id` | the contribution row id |
| `reference` | `account_code` |
| `amount` | `gross_amount` (pre-elimination), exact |
| `currency` | the run's `target_currency` |
| `date` | the run `started_at` date (UTC) |
| `attributes` | `consolidation_run_id`, `source_legal_entity_id`, `currency_basis` = `target_currency_label` (the service records **no** currency per contribution and translates nothing, so this is a label, not evidence of translation), `run_status` |

## Implementation status (Wave 4)

| Source | Populations | Authz action | limit default / max |
|---|---|---|---|
| intercompany-accounting-svc | `entry-legs` | `INTERCOMPANY_CONTROL_POPULATION_READ` | 1000 / 5000 |
| consolidation-svc | `balance-contributions` | `CONSOLIDATION_CONTROL_POPULATION_READ` | 1000 / 5000 |
| general-ledger-svc | `journal-account-totals`, `trial-balance` | `GL_CONTROL_POPULATION_READ` (existing) | 1000 / 5000 |
| general-ledger-svc | `journal-balances`, `control-account-postings`, `unposted-events`, `manual-journals` | `GL_CONTROL_POPULATION_READ` (existing) | 1000 / 5000 |

- Both new services validate `legal_entity_id` as `[A-Za-z0-9][A-Za-z0-9_.:-]{0,63}` (their columns are VARCHAR).
- **`no_period` specs.** These populations reject `period_id`, yet a `PERIOD_END` run must carry one. A
  spec therefore sets `"no_period": true` and the client omits `period_id` for it. Every Wave 3/4
  population that rejects `period_id` is flagged this way in the catalogue.
- **Consolidation carries no currency and translates nothing.** `balance-contributions.currency` is the
  run's `target_currency` *label*; a ledger balance in another currency will (correctly) surface as a
  currency mismatch.
- **Money is read as exact `NUMERIC` text** in every new endpoint, although the intercompany and
  consolidation services use `float64` internally.

### general-ledger-svc — Wave 5 populations

All four take `legal_entity_id`, `limit`, `cursor` plus the one scope parameter below, and **reject
`period_id`** (and any other parameter). Grouped-population paging: `record_id` text, `COLLATE "C"`
keyset. Watermark `gl4`/`gl5`/`gl6`/`gl7` `:n=<count>;entry_seq=<max>;md5=<digest over every wire
field and attribute>`.

| Population | Param | `record_id` / `reference` | `amount` | `date` | `attributes` |
|---|---|---|---|---|---|
| `journal-balances` | `fiscal_period` (exact text) | `<journal_id>:<currency_code>` / `journal_id` | Σ debit of the journal's `ledger_entries` in that currency | max `transaction_date` | `debit_total`, `credit_total`, `entry_count` (exact decimal text). Control: `debit_total = credit_total`. `record_id` carries the currency so a two-currency journal never repeats an id. |
| `control-account-postings` | `fiscal_period` | `ledger_entry_id` / `journal_id` | signed net `debit − credit` of the entry | `transaction_date` | `account_code`, `journal_id`, `journal_type`, `created_by`, `approved_by` (empty if none). **Violation set:** entries on accounts with `is_control_account AND direct_posting_restricted` whose journal header has NULL/blank `source_event_id`. |
| `unposted-events` | `created_before` (RFC3339 or `YYYY-MM-DD`=00:00Z; strict) | `execution_id` / `source_event_id`, else `execution_id` | `0` (executions carry no amount) | UTC date of `created_at` | `status`, `kind`, `failure_reason` (≤200 chars), `age_days` = floor((`created_before` − `created_at`)/1 day), never from `now()`. **Violation set:** `posting_executions` with `status <> 'COMMITTED'` and `created_at < created_before`. `currency` is `XXX` (ISO 4217 "no currency"): the consumer requires `^[A-Z]{3}$` and executions have no currency. |
| `manual-journals` | `fiscal_period` | `journal_id` / `journal_id` | Σ `debit_amount` of the journal's lines | `transaction_date` | `status` (FINALIZED/REVERSED), `fiscal_period`, `journal_type`, `created_by`, `approved_by`, `approval_status`, `review_gap` = `NO_APPROVER` (approver NULL/blank) / `SELF_APPROVED` (approver = creator) / empty. Journals with NULL/blank `source_event_id` in status FINALIZED or REVERSED. |

## financial-close-svc — Wave 6

`GET /v1/control-populations/migration-batch-tieout` — authz action `FINANCIAL_CLOSE_CONTROL_POPULATION_READ`
(checked against `legal_entity_id`; 401 no tenant/principal, 403 deny, 503 authz outage), limit 1000 / 5000.
Params: `legal_entity_id` (`[A-Za-z0-9][A-Za-z0-9_.:-]{0,63}`), required `batch_id` (lower-case uuid; unknown
batch for the tenant/entity → 404), `limit`, `cursor`. **`period_id` is rejected** (lifetime population), as is
any unknown or repeated parameter (400).

Exactly one record per batch: `record_id` = `reference` = `batch_id`; `amount` = `expected_total_debits`;
`currency` = `XXX` (`migration_batches` has no currency column); `date` = UTC date of `created_at` (the load
date). `attributes` (exact decimal strings): `expected_total_debits`, `crosswalk_debits`, `expected_total_credits`,
`crosswalk_credits`, `expected_row_count`, `crosswalk_row_count`, `batch_status`, `source_extract_hash`,
`journal_id` (empty if none). Crosswalk sums are `COALESCE(SUM(..),0)::numeric(18,2)::text` (empty crosswalk →
`0.00`, never null), computed in SQL in a `REPEATABLE READ` read-only transaction with explicit `tenant_id`
predicates. Watermark `fc1:n=1;md5=<digest over every wire field and attribute>`; `declared_totals` =
`{row_count: 1, totals: {"XXX": expected_total_debits}}`. No personal data is exposed.

## payee-banking-identity-svc — Wave 6

`GET /v1/control-populations/destination-changes` — authz action `PAYEE_BANKING_CONTROL_POPULATION_READ`
(checked against `legal_entity_id`; 401 no tenant/principal, 403 deny, 503 authz outage or store failure), limit
1000 / 5000. Params: `legal_entity_id` (`[A-Za-z0-9][A-Za-z0-9_.:-]{0,63}`), required `changed_from` and
`changed_to` (`YYYY-MM-DD`, inclusive UTC dates, `changed_from <= changed_to`, span <= 400 days), `limit`,
`cursor`. **`period_id` is rejected**, as is any unknown or repeated parameter (400).

**Population.** Bank details of a `payee_destinations` row are immutable (trigger), so a *change of bank
details* is always a new destination, recorded by its `PAYEE_DESTINATION_PROPOSED` event. A destination is in
scope when (a) that event's `created_at` UTC date lies in `[changed_from, changed_to]` and (b) it reached an
approved state: an `PAYEE_DESTINATION_APPROVED` or `PAYEE_DESTINATION_ACTIVATED` event exists, or `approved_at`
is set, or status is `APPROVAL_PENDING`/`ACTIVE`/`SUSPENDED`. Approval is deliberately not gated on a non-blank
approver, so an `ACTIVE` destination with no approver is reported (`NO_APPROVER`). Destinations that were later
superseded are included if they had reached approval; never-approved ones (`CANDIDATE`, `VERIFIED`, unapproved
`SUPERSEDED`) are not.

| Field | Value |
|---|---|
| `record_id` / `reference` | `destination_id` |
| `amount` / `currency` | `0` / `XXX` (no monetary value; ISO 4217 "no currency") |
| `date` | UTC date of the `PAYEE_DESTINATION_PROPOSED` event |
| `attributes` | `party_ref`, `status`, `proposed_by`, `verified_by`, `approved_by`, `sod_gap` (all strings) |

`sod_gap` is computed in SQL on trimmed principal ids, first match wins: `NO_APPROVER` (approver NULL/blank),
`SELF_APPROVED` (approver = proposer), `SELF_VERIFIED` (verifier present and = proposer), `NO_VERIFIER` (verifier
blank — the workflow only allows approval from `VERIFIED`, so a verifier is always required), else empty.

Strict privacy: only opaque ids and principal ids are exposed — never account number/IBAN/sort code, institution,
payee name, country, last4 or fingerprint. Explicit `tenant_id` predicates on both tables, `REPEATABLE READ`
read-only transaction, keyset paging on `destination_id`. Watermark `pb1:n=<count>;md5=<digest over every wire
field and attribute>`; `declared_totals` = `{row_count: n, totals: {"XXX": "0"}}`.

**Not covered.** This population lists approved bank-detail changes and whether maker/checker separation held. It
does NOT link a change to the payment released to that destination afterwards; that spans
payment-authorization-svc and needs its own population.

## tax-authority-interface-svc — Wave 6

`GET /v1/control-populations/unacknowledged-filings` — authz action `TAX_AUTHORITY_CONTROL_POPULATION_READ`,
limit 1000 / 5000. Params: `legal_entity_id` (validated `[A-Za-z0-9][A-Za-z0-9_.:-]{0,63}`; the column is VARCHAR),
required `submitted_before` (RFC3339, converted to UTC, or `YYYY-MM-DD` = 00:00 UTC; strict `<`), `limit`, `cursor`.
`period_id` and any other or repeated parameter are rejected with 400.

**Violation set:** `tax_filing_submissions` with `submitted_at < submitted_before` AND (`ack_reference` NULL or
blank after trim). The entity is resolved through the owning `tax_interfaces` row (submissions carry no entity
column); explicit `tenant_id` predicates on both tables, `REPEATABLE READ` read-only tx, keyset paging on
`submission_id` (`COLLATE "C"`).

| Field | Value |
|---|---|
| `record_id` / `reference` | `submission_id` (no separate authority reference exists; `ack_reference` is the *absent* value) |
| `amount` | `tax_amount` as `NUMERIC::text` |
| `currency` | `XXX` — `tax_filing_submissions` has no currency column |
| `date` | UTC date of `submitted_at` |
| `attributes` | `interface_id`, `tax_period`, `filing_type`, `status`, `age_days` = floor((`submitted_before` − `submitted_at`)/1 day), computed in SQL from the parameter, never `now()` |

No taxpayer identifiers, credentials or authority payloads are exposed. Watermark `tx1:n=<count>;md5=<digest over
every wire field and attribute>`; `declared_totals` = row count and per-currency sum (currency is always `XXX`, so
the total is informational).

**Caveat — no acknowledgement write-back exists today.** Nothing in the service ever sets `ack_reference` or changes
a submission's status after creation (`SubmitTaxFiling` inserts `PENDING` with no acknowledgement and there is no
transmission adapter or update path). Until one is built, every submission older than the cut-off appears in this
population; a non-empty result is currently a statement about the missing integration, not about individual filings.

## general-ledger-svc — event-journal-breaks

`GET /v1/control-populations/event-journal-breaks` — ZS-CONTROL-001 FIN-CTRL-019 "Accounting event to journal":
every accepted accounting event yields exactly one posted journal, and every event-sourced journal traces to an
accepted event. Same auth (`GL_CONTROL_POPULATION_READ` against `legal_entity_id`), limits, cursor and error
mapping as `unposted-events`. Params: `legal_entity_id`, required `created_before` (RFC3339 converted to UTC, or
`YYYY-MM-DD` = 00:00 UTC; strict `<`), `limit`, `cursor`. `period_id` and any other or repeated parameter → 400.

**Violation set**, one record per break (`REPEATABLE READ` read-only tx, explicit `tenant_id` predicates, keyset on
`record_id` under `COLLATE "C"`). Only `posting_executions.kind = 'EVENT'` is considered (`REVERSAL` and
`APPROVED_JOURNAL` are out of scope).

| `break_type` | Rule |
|---|---|
| `EVENT_WITHOUT_JOURNAL` | `kind='EVENT'`, `status='COMMITTED'`, `created_at < created_before`, and `journal_id` is NULL, or references no `journal_headers` row of the tenant, or the journal's status is not `FINALIZED`/`REVERSED`. |
| `JOURNAL_WITHOUT_EVENT` | Journal in `FINALIZED`/`REVERSED` with a non-blank `source_event_id`, `created_at < created_before` **and** `COALESCE(posted_at, created_at) < created_before`, with no `COMMITTED` execution of the same tenant having that `source_event_id` and `journal_id` = the journal. |
| `DUPLICATE_JOURNAL_FOR_EVENT` | Among non-`REVERSED` journals sharing one (tenant, non-blank `source_event_id`), every journal after the first in (`created_at`, `journal_id`) order, if it was created before `created_before` and belongs to `legal_entity_id`. |

| Field | Value |
|---|---|
| `record_id` | `<break_type>:<execution_id or journal_id>` (execution for `EVENT_WITHOUT_JOURNAL`, journal otherwise) |
| `reference` | `source_event_id`; the execution id if blank |
| `amount` | Σ `debit_amount` of the journal's lines (`NUMERIC::text`) when a journal row is involved, else `0` |
| `currency` | the journal's `currency_code` when a journal row is involved, else `XXX` |
| `date` | UTC date of the execution's `created_at`, or the journal's `posting_date` |
| `attributes` | `break_type`, `status` (journal status if a journal row exists, else the execution status), `journal_id` (empty if none), `execution_id` (empty for journal breaks), `source_event_id` |

One journal can appear under two break types (e.g. a duplicate is also unlinked from the execution); each is its own
record. Watermark `gl8:n=<count>;entry_seq=0;md5=<digest over every wire field and attribute>`; `declared_totals` =
row count and per-currency amount sums (`XXX` total is `0`).
