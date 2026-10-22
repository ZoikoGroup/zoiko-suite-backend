# Spec deviations — treasury-svc (BNK-01, BNK-08, BNK-09, BNK-10)

Governing document: `ZoikoSuite_Banking_Cash_Treasury_Detailed_Service_Specifications_v1_0.docx`
(ZS-SVC-E-001 v1.0), §7 profiles "BNK-01 — Bank Account", "BNK-08 — Cash
Position", "BNK-09 — Treasury Transfer" and "BNK-10 — FX Exposure"; the §2
invariants 1, 2, 18, 20–25; the §4 shared contract (context, concurrency,
idempotency, errors, external state, sensitive data, evidence); the §8 payment
execution state matrix; the §9 rows "Treasury transfer | BNK-09 → BNK-06/07 →
ACC", "Cash position | BNK-08 projection only" and "FX exposure | BNK-10
analytical view"; §12 negative paths 1–4 and 30–40; and the §17 anti-patterns
"Cash position treated as ledger/bank source of truth", "Cross-entity transfer
treated as internal same-entity movement" and "FX exposure triggers autonomous
hedge/payment". The copy in `docs/architecture/original_doc/` is byte-identical
to the one in `docs/architecture/`.
First survey of this service against the spec (2026-10-22).

**Citation check.** Every BNK-01/08/09/10 line cited in the review request was
compared with the docx text: commands, queries, state models, SoD lines,
failure/degradation semantics and minimum negative-path certification all match
the document word for word. Nothing was missing or misquoted. Two lines the
request did not quote are used below: BNK-09's permissions
("treasury.transfer.read/create; treasury.transfer.approve;
treasury.transfer.authorize; treasury.transfer.cancel") and BNK-10's state
model and negative paths ("Forecast assumption changed without versioning;
Stale FX rate used without warning; Cross-entity exposure netted without
policy; Exposure scenario automatically triggers payment/trade").

**File mapping.** The `bnkNN_handler.go` / `bnkNN_store.go` names were checked
against the routes, not assumed. They are accurate, with three exceptions:
- BNK-01's `CreateBankAccount` and `ListBankAccounts` (`RegisterBankAccount`,
  `ListBankAccounts`) live in `handler.go`, not `bnk01_handler.go`.
- The pre-BNK-08 cash views (`/positions`, `/effective-cash`, `/forecasts`,
  `/thresholds`) also live in `handler.go`. They are not BNK-08 commands, but
  they compose the same bank balances and are covered in the BNK-08 section.
- `bnk09_handler.go` also serves `/transfers/{id}/fingerprint`, a
  service-to-service read that payment-initiation-adapter-svc (BNK-06) uses
  to verify a treasury payment.

**Overall.** The service has a real append-only spine:
- BNK-01: account history and ownership evidence are supersede-only, and
  triggers make legal entity and identifiers immutable outside a rotation.
- BNK-09: transfers have maker-checker controls backed by DB triggers, and
  the saga resumes by status, with every external call idempotent.
- BNK-08 and BNK-10: snapshots are immutable once calculated, and every
  write enqueues its event in the same transaction.

The most important finding is that **the cross-entity treasury transfer — the
one path that drives intercompany accounting — could never complete** (A18).
The transfer was approved and authorized, and Execute sent the payment
attempt to BNK-06. The store then refused to record the submission, so the
ledger and intercompany steps never ran, and the payment had already left.
A same-entity draft could also be amended into a cross-entity movement and
executed as same-entity (A19).

Both are closed in this pass, together with:
- BNK-09: authorization against the wrong scope (A20), a reject with no
  permission check (A21), and unauthenticated transfer reads (A22).
- BNK-01: an unscoped account list (A1) and an amend that always failed or
  blanked fields (A4).
- BNK-08: staleness that was frozen at calculation, so a published position
  was served as current forever (A10); silently dropped accounts (A11); and a
  refresh that could retire another entity's snapshot (A13).
- BNK-10: RecordFXRate failing on every call (A29) and a scenario silently
  using parity (A30).

The largest open items are structural:
- Nothing feeds real bank balances into the service (A9).
- Treasury transfers "complete" on BNK-06 acceptance rather than BNK-07
  settlement (A23), and a returned-then-resubmitted transfer reuses its
  original payment attempt (A24).
- Money is `float64` throughout (A34).

Rough coverage after this pass: BNK-01 ~70%, BNK-08 ~45%, BNK-09 ~55%, BNK-10
~50%.

## Command-by-command

Status key: **Done** = implemented as named; **Partial** = exists with a gap
listed below; **Missing** = no route.

### BNK-01 — Bank Account

| Spec command / query | Route | Status |
|---|---|---|
| CreateBankAccount | `POST /accounts` (`RegisterBankAccount`) | Partial — always creates `ACTIVE` (A2); masking not enforced (A3) |
| VerifyBankAccountOwnership | `POST /accounts/{id}/verify-ownership` | Done — creator cannot verify; evidence append-only |
| AmendBankAccountMetadata | `POST /accounts/{id}/amend-metadata` | Done after A4 |
| ChangeOperationalUse | `POST /accounts/{id}/change-operational-use` | Done |
| SuspendBankAccount | `POST /accounts/{id}/suspend` | Done — reason not in history/event (A6) |
| ReactivateBankAccount | `POST /accounts/{id}/reactivate` | Done |
| CloseBankAccount | `POST /accounts/{id}/close` | Done — reason not in history/event (A6) |
| RotateAccountIdentifierToken | `POST /accounts/{id}/rotate-token` | Done — trigger enforces `token_version + 1` |
| GetBankAccount | `GET /accounts/{id}` | Done |
| GetBankAccountMasked | `GET /accounts/{id}/masked` | Done (separate, lower permission) |
| GetBankAccountAsOf | `GET /accounts/{id}/as-of` | Done — append-only history |
| GetOwnershipEvidence | `GET /accounts/{id}/ownership-evidence` | Done |
| ListConnectionOptions | `GET /accounts/{id}/connection-options` | Done — live from banking-connector-svc, fails closed |
| GetAvailableActions | `GET /accounts/{id}/available-actions` | Done |
| (list) | `GET /accounts` | Done after A1 |

### BNK-08 — Cash Position

| Spec command / query | Route | Status |
|---|---|---|
| CalculateCashPosition | `POST /cash-position/calculate` | Partial — no real bank-balance source (A9), single currency (A15) |
| RefreshCashPosition | `POST /cash-position/refresh` | Done after A13 |
| PublishCashPositionSnapshot | `POST /cash-position/{id}/publish` | Done after A10 |
| SupersedeCashPositionSnapshot | `POST /cash-position/{id}/supersede` | Done after A13 |
| GetCashPosition | `GET /cash-position` | Partial — returns latest regardless of status (B6) |
| GetCashPositionAsOf | `GET /cash-position/as-of` | Done |
| GetSourceFreshness | `GET /cash-position/{id}/freshness` | Done after A10/A12 |
| GetAccountDrilldown | `GET /cash-position/{id}/drilldown` | Done after A12 |
| GetCurrencyBreakdown | `GET /cash-position/currency-breakdown` | Partial — latest snapshot per currency, no translation (A15) |

### BNK-09 — Treasury Transfer

| Spec command / query | Route | Status |
|---|---|---|
| CreateTreasuryTransfer | `POST /transfers` | Done — after A25 also requires ACTIVE accounts; no value date/purpose/FX (A26) |
| AmendTreasuryTransfer | `POST /transfers/{id}/amend` | Done after A19 |
| SubmitTransfer | `POST /transfers/{id}/submit-for-approval` | Done (DRAFT → PENDING_APPROVAL) |
| ApproveTreasuryTransfer | `POST /transfers/{id}/approve` | Done after A20 |
| RejectTreasuryTransfer | `POST /transfers/{id}/reject` | Done after A21 |
| AuthorizeTreasuryTransfer | `POST /transfers/{id}/authorize` | Done after A18/A20 — cross-entity only (B9) |
| InitiateTreasuryTransfer | `POST /transfers/{id}/initiate` (alias of `/execute`) | Partial — completes without settlement (A23) |
| CancelBeforeSubmission | `POST /transfers/{id}/cancel` | Done after A18 (now covers AUTHORIZED) |
| ResolveTreasuryTransfer | `POST /transfers/{id}/resolve` | Partial — RESUBMIT reuses the original attempt (A24) |
| GetTreasuryTransfer | `GET /transfers/{id}` | Done after A22 |
| GetTransferApproval | `GET /transfers/{id}/approval` | Done after A22 |
| GetTransferExecution | `GET /transfers/{id}/execution` | Done after A22 |
| GetAvailableActions | `GET /transfers/{id}/available-actions` | Done after A22 |
| ListTransfers | `GET /transfers` | Done after A22 |
| (operator) | `POST /transfers/{id}/mark-returned` | Manual only (A23) |

### BNK-10 — FX Exposure

| Spec command / query | Route | Status |
|---|---|---|
| CalculateFXExposure | `POST /fx/exposure-snapshot/calculate` | Partial — source population incomplete (A31) |
| RefreshFXExposure | `POST /fx/exposure-snapshot/refresh` | Done after A32 |
| PublishFXExposureSnapshot | `POST /fx/exposure-snapshot/{id}/publish` | Done after A32 |
| RunFXScenario | `POST /fx/scenario` | Done after A30 — permission is the view permission (A33) |
| SupersedeFXExposureSnapshot | `POST /fx/exposure-snapshot/{id}/supersede` | Done after A32 |
| GetFXExposure | `GET /fx/exposure-snapshot` (snapshot) and `GET /fx/exposure` (live) | Done |
| GetFXExposureAsOf | `GET /fx/exposure-snapshot/as-of` | Done |
| GetCurrencyBreakdown | `GET /fx/exposure-snapshot/currency-breakdown` | Done |
| GetMaturityProfile | `GET /fx/exposure-snapshot/maturity-profile` | Done — fixed buckets |
| GetScenario | none | Missing — deliberate (B12) |
| GetSourceLineage | `GET /fx/exposure-snapshot/{id}/lineage` | Done after A32 — thin lineage (A31) |
| (rates) | `POST /fx/rates` | Done after A29 |

## A. Not implemented / weaker than the spec

### BNK-01 — Bank Account

- **A1. The account list skipped authorization when no legal entity was given
  — closed in this pass (8c90c0be).** §2 invariant 2 says raw values never
  appear in list APIs, and the §7 failure line says "sensitive values are
  never exposed in ordinary list/log/search APIs". `GET /v1/treasury/accounts`
  called `CheckAllowed` only `if legalEntityID != ""`. Without the parameter it
  made no permission check and returned every account in the tenant: masked
  number, bank identifier, creator principal, status. Any principal with a
  tenant header could enumerate the tenant's accounts across all legal
  entities. Masked numbers are the only identifier stored (A3), so no raw
  number was exposed. It is still a real authorization bypass, not a spec
  nicety. `legal_entity_id` is now required (400 otherwise) and always
  authorized. The only caller, financial-close-svc's `ListBankAccounts`,
  already sends it.
- **A2. `DRAFT → PENDING_VERIFICATION → ACTIVE` is not reachable.** The state
  model is "Draft → PendingVerification → Active → Suspended → Closed".
  `RegisterBankAccount` hard-codes `AccountStatus: "ACTIVE"`, and the request
  has no status field. No command moves an account from DRAFT or
  PENDING_VERIFICATION to ACTIVE. Migration 000003 records this as deliberate
  backward compatibility: a PENDING account would drop out of every cash
  aggregation until verified. The domain comment says callers "may explicitly
  use" DRAFT/PENDING_VERIFICATION, but no API path lets them. Ownership
  verification is the real gate: BNK-09 refuses unverified accounts. That
  makes the orthogonal "verified" fact load-bearing in place of the status
  leg. *Why not fixed:* changing the create default, or adding an activate
  command tied to verification, changes every caller's flow and cash
  aggregation. It is a product decision.
- **A3. Masking is trusted, not enforced.** `masked_account_number` (create)
  and `new_masked_account_number` (rotate) are stored exactly as sent. The
  service never sees a raw number, so nothing it stores is raw by its own
  doing. But if a caller sends an unmasked number, it is stored and returned
  by every read, the list included. That is the "Sensitive account number
  returned in list API" negative path, defended only by caller discipline.
  *Why not fixed:* rejecting "unmasked" input needs a defined mask/token
  format. A digit-count rule would also reject legitimate opaque tokens. This
  needs a ZS-SEC-001 tokenization decision, not a heuristic.
- **A4. Amend failed or blanked fields unless every field was resent — closed
  in this pass (8c90c0be).** The store wrote all five fields as sent, but
  migration 000003's trigger lets `bank_identifier` change only with
  `token_version + 1`.
  - An amend that left out `bank_identifier` (the handler test sends only
    `account_name`) wrote `''`, tripped the trigger and returned 503.
  - An amend that resent the identifier but not `branch_ref`, `country` or
    `account_type` blanked them.
  - The store tests always resent every field, and the handler test runs
    against a mock, so neither caught it.

  Amend is now partial: an empty field keeps its value, in one SQL
  statement. It never touches `bank_identifier`, and a request that tries to
  change the identifier gets 400 `identifier_change_requires_rotation`.
- **A5. No sensitive-read path, `bank.account.sensitive.read`, or
  purpose/step-up/JIT.** Nothing raw is stored (A3), so there is nothing to
  gate. If tokenized raw identifiers are ever stored, the SoD line
  "sensitive identifier access requires purpose + step-up/JIT policy" applies
  and is unimplemented.
- **A6. Thin evidence for status changes.**
  - The §7 evidence line asks for "prior/new values, source, verifier,
    effective dates, … actor/approver and reason". `suspend_reason` and
    `close_reason` are stored on the row, but not in `bank_account_history`
    or in the event payloads.
  - Every event after `created` carries an empty correlation ID.
  - `BankAccountChanged` is emitted as three finer events
    (`metadata-amended`, `operational-use-changed`, `token-rotated`).
- **A7. Maker-checker on ownership verification has a legacy hole.** The
  creator-cannot-verify check is `CreatedByPrincipalID != "" && == principal`.
  Accounts created before migration 000003 have an empty creator, so anyone
  can verify them. Re-verification (superseding evidence) is single-actor:
  any non-creator with `BANK_ACCOUNT_VERIFY` can replace the standing
  evidence, with no second approval.
- **A8. Other contract gaps.**
  - No expected-version/ETag on account mutations. CAS is on status only
    (§4 concurrency).
  - The §4 problem types (`BANK_ACCOUNT_UNVERIFIED`, etc.) are not used; the
    service returns its own snake_case codes.
  - Account creation writes a 0.00 "balance trace" with `_ =`, ignoring the
    error.
  - Legal-entity ownership is immutable by trigger (close and recreate), so
    the SoD line "entity ownership change requires independent evidence" is
    met by making the change impossible in place.

### BNK-08 — Cash Position

- **A9. There is no real bank-balance source.** BNK-08's dependencies are
  "BNK-01/02/03/07", and its inputs start with "bank balances". The only
  writer of `cash_balances` in the whole repository is `RegisterBankAccount`.
  It writes a 0.00 placeholder dated at registration, so:
  - Every cash position sums zeros.
  - Every balance is older than 24 hours one day after the account is
    registered, so the position is stale and cannot be published.
  - BNK-09's liquidity-threshold check also reads this placeholder (A27).

  Nothing ingests BNK-03 statement balances or BNK-07 settlement movements.
  *Why not fixed:* this is an integration (consume BNK-03 balance events into
  `cash_balances`), not a local patch.
- **A10. Staleness was frozen at calculation, so a published position was
  served as current forever — closed in this pass (29baaaaf).** The spec
  says "stale is derived from source freshness, not operator choice" and
  "never silently carry forward old bank balance as current".
  `has_stale_component` was computed once, at calculation, and
  `EffectiveStatus` turned a PUBLISHED snapshot into `STALE` only if that
  stored flag was set. Publish refuses a snapshot whose flag is set, so
  `STALE` was unreachable. A snapshot published from fresh balances stayed
  PUBLISHED and current however old it got, and a snapshot calculated three
  days ago could still be published. This is the "Stale bank balance
  displayed as current" negative path.

  Staleness is now derived on every read (`domain.CashPositionStale`): the
  stored flag, or the oldest composed balance aged past
  `BankBalanceStalenessThreshold`. Publish uses the same rule.
  `GetSourceFreshness` reported the bank-balance component's as-of time as
  the calculation time. It now reports the oldest composed balance's own
  timestamp.
- **A11. A failed or missing balance was silently left out of the total —
  closed in this pass (29baaaaf).** The composition did `if err != nil ||
  bal == nil { continue }`. A store error, or an ACTIVE account with no
  balance, dropped that account from the sum with nothing to say the figure
  was partial. That is exactly what the failure line forbids ("must show
  stale/partial confidence"). The same silent skip was in `GetEffectiveCash`,
  `GetForecasts` and `GetCashPositions`. A store error now fails closed (503)
  in all four views. An ACTIVE account with no balance marks the snapshot and
  effective cash stale, which blocks publish.
- **A12. Freshness and drill-down reads had no permission check — closed in
  this pass (29baaaaf).** They required a principal but no
  `CheckAllowed`. Any principal in the tenant could read any legal entity's
  per-account balances by snapshot ID. Both now check `TREASURY_POSITIONS_VIEW`
  in the snapshot's legal entity, like every other cash-position read.
- **A13. Refresh was two transactions and could retire another entity's
  snapshot — closed in this pass (29baaaaf).** Refresh created the new
  snapshot (one transaction), then superseded `prior_snapshot_id` in a
  second. Authorization covered only the request's legal entity, and
  supersede accepted any snapshot ID. As a result:
  - A principal authorized for entity B could retire entity A's published
    position by passing its ID.
  - A failed supersede returned an error while the new snapshot stayed, and
    a retry created another.

  `domain.RefreshCashPositionParams` already described the intended atomic
  version but was unused. Refresh is now one transaction. Both refresh and
  the standalone supersede lock the old snapshot and require a different,
  live replacement for the same legal entity and currency (409
  `snapshot_scope_mismatch`).
- **A14. Restricted cash is caller-supplied; `restricted.cash.read` is
  absent.** `restricted_amount` is a number in the calculate request. The
  migration says the classification source is undecided. Omitting it counts
  restricted cash as freely available: the "Restricted cash counted as
  freely available" negative path, defended only by the caller. Restricted
  figures are visible to anyone with the view permission. *Why not fixed:*
  the classification source and the permission are open product decisions.
- **A15. Only accounts in the reporting currency are counted; no
  translation.**
  - The composition filters `acct.CurrencyCode == currency`. Accounts in
    other currencies are silently excluded, not translated, and
    `fx_rate_version` is always `''`. That is the "FX rate version missing"
    negative path, met by never converting rather than by versioning the
    conversion.
  - Pending flows are AP plus payroll plus tax only. Pending receipts and
    in-flight treasury transfers, both listed as inputs, are absent.
  - Pending-flow weighting is absent.

  *Why not fixed:* reporting-currency translation needs the FX source/version
  decision BNK-10 also lacks (A31).
- **A16. `CashPositionBecameStale` fires only at calculation.** A snapshot
  that ages into STALE (A10) emits no event, because nothing runs when it
  crosses the threshold. That needs a scheduler or a read-side emitter.
- **A17. Stale positions do not block treasury decisions.** The failure line
  says a stale view "may block treasury decisions by policy". BNK-09 does not
  consult BNK-08. Policy-dependent, so recorded and not built.

The negative path "Cash position used to overwrite ledger balance" is met:
nothing in BNK-08 writes `cash_balances`, the ledger, or any other service's
data. Snapshots are only ever inserted, and their figures are immutable by
trigger.

### BNK-09 — Treasury Transfer

- **A18. A cross-entity transfer sent its payment and then could never be
  recorded — closed in this pass (e9cce85b).** This was the most serious
  finding. `ExecuteTreasuryTransfer` submits a cross-entity transfer only
  from AUTHORIZED, because it refuses APPROVED cross-entity transfers. It
  called `SubmitTreasuryPayment`, which really creates and submits the BNK-06
  attempt, and then `MarkTransferSubmitted`. That CAS accepted only
  `status='APPROVED'`, so it failed (409), and the transfer stayed AUTHORIZED
  with the payment already sent. A retry got the same attempt back from
  BNK-06's idempotency and failed the same way.
  - The ledger posting and intercompany pairing, the whole cross-entity
    consequence the brief asked about, were wired but unreachable.
  - The store saga test passed because it submitted a cross-entity transfer
    straight from APPROVED, skipping the step the handler requires.
  - The handler tests passed because the mock's `MarkTransferSubmitted`
    accepts any status.

  The CAS is now `(APPROVED AND NOT is_cross_entity) OR (AUTHORIZED AND
  is_cross_entity)`, so the store also refuses an unauthorized cross-entity
  submission. `CancelBeforeSubmission` now also covers AUTHORIZED, which
  `domain.CanCancelBeforeSubmission` and `GetAvailableActions` already
  advertised. The new tests failed at HEAD (reproduced first) and pass now.
- **A19. A same-entity draft could be amended into a cross-entity movement
  and executed as same-entity — closed in this pass (e9cce85b).** The spec
  says "cross-entity movement cannot be disguised as same-entity transfer to
  bypass intercompany accounting". Amend replaced the source and target
  accounts but kept the original `is_cross_entity`. It also never checked
  permission in the new accounts' legal entities, or their status or
  verification. A maker could create a same-entity draft and point its
  target at another entity's account. The result:
  - It went through approval as same-entity, with no AUTHORIZED step.
  - It submitted from APPROVED and was marked COMPLETED straight after
    BNK-06 acceptance.
  - It never touched general-ledger-svc or intercompany-accounting-svc.

  This is the "Cross-entity transfer omits intercompany accounting" negative
  path. Amend now runs the new accounts through Create's checks and
  re-derives the flag from their legal entities. Execute also refuses (409
  `cross_entity_classification_mismatch`) any transfer whose stored flag
  disagrees with its accounts, before anything is sent.
- **A20. Every command after create authorized against the tenant ID —
  closed in this pass (e9cce85b).** Approve, authorize, execute, amend,
  submit-for-approval, cancel, mark-returned and resolve all called
  `CheckAllowed(principal, transfer.TenantID, action)`. That put the tenant ID
  in authorization-svc's `legal_entity_id` slot, so the approver's grant was
  evaluated against an entity that doesn't exist, not the one owning the
  money. Create already checked the source entity, and the target entity too
  when cross-entity. Every command now does the same, from the accounts'
  current legal entities. For cross-entity transfers, approve and authorize
  therefore need the permission in both entities, which is the spec's
  "dual-entity" reading (B9).
- **A21. Reject had no permission check — closed in this pass (e9cce85b).**
  Any principal with a tenant header (other than the maker) could reject any
  pending transfer. It now needs `TREASURY_TRANSFER_APPROVE` in the
  transfer's entities, since rejecting is the checker's other answer.
- **A22. Transfer reads needed only a tenant header — closed in this pass
  (e9cce85b).** `GetTreasuryTransfer`, `GetTransferApproval`,
  `GetTransferExecution`, `GetTransferAvailableActions` and `ListTransfers`
  required no principal and made no permission check. `ListTransfers`
  returned every transfer in the tenant. All now require a principal with
  `TREASURY_POSITIONS_VIEW` in the transfer's entities (B10), and the list
  requires `legal_entity_id`. Nothing outside the service called these
  routes. The fingerprint read stays principal-free (B11).
- **A23. "Completed" means BNK-06 accepted the attempt, not that money
  settled.**
  - The state model goes "… Submitted/Pending → Settled / Rejected / Returned
    / Cancelled".
  - §2 invariant 18 says initiation is not settlement, and §9 routes
    treasury transfers "BNK-09 → BNK-06/07 → ACC".
  - A same-entity transfer goes SUBMITTED → COMPLETED inside the same
    Execute request, as soon as BNK-06 reports SUBMITTED, and emits
    `treasury-transfer.settled`.
  - A cross-entity transfer posts the GL journal and pairs intercompany at
    SUBMITTED, also before settlement. §8 allows in-transit recognition at
    Submitted only "where approved accounting policy and evidence support
    it".
  - Nothing consumes BNK-07 status. Returns are recorded only by the manual
    `mark-returned`.
  - A transfer returned after LEDGER_POSTED/INTERCOMPANY_PAIRED keeps its
    journal and intercompany entry, and no reversal is raised (§8:
    "Returned … create return/reversal business event and accounting
    correction").

  *Why not fixed:* this needs a BNK-07 status consumer, a real Settled state,
  and a reversal path through general-ledger-svc and
  intercompany-accounting-svc. It is a design change across services.
- **A24. A returned transfer that is resubmitted reuses its original payment
  attempt.**
  - `SubmitTreasuryPayment` always uses `IdempotencyKey = transferID`.
    payment-initiation-adapter-svc answers a reused key with the existing
    attempt (200).
  - After RETURNED → RESUBMIT → re-approval → Execute, BNK-06 hands back the
    original attempt with its own last status. That is SUBMITTED unless BNK-06
    learned of the return, and nothing tells it.
  - The transfer is then marked SUBMITTED against the old attempt, and a
    same-entity one is marked COMPLETED. No new payment is ever sent, yet the
    transfer completes.

  The timeout case is safe. A retry after a lost response reuses the key,
  gets the same attempt back, and a PENDING_UNKNOWN attempt surfaces as 503.
  No duplicate is created, which is the right half of the "Provider timeout
  causes duplicate transfer" negative path. *Why not fixed:* a resubmission
  needs a new attempt identity (for example `transferID:generation`), and §2
  invariant 16 forbids a new attempt while the original is unresolved. Wiring
  that safely depends on A23's BNK-07 resolution, so it is not a key-format
  tweak.
- **A25. Inactive accounts were not blocked — closed in this pass
  (e9cce85b).** Create checked ownership verification but not account
  status, so a SUSPENDED or CLOSED account could be a transfer's source or
  target. Execute re-checked neither fact before sending the payment. The
  §7 failure lines say "inactive account … blocks completion" and
  "unverified or suspended account blocks protected outbound use". Create
  and Amend now require both accounts ACTIVE and verified. Execute re-checks
  both immediately before the payment attempt, but not after submission,
  when the ledger steps must still record money that already moved.
  Legal-entity ownership cannot change in place (BNK-01 trigger), so
  "account ownership changed after approval" reduces to these status and
  verification re-checks.
- **A26. The transfer subject is incomplete.**
  - The spec's required inputs include value date, purpose, requested FX/rate
    basis and transfer type. None exist. `ExecutionDate` sent to BNK-06 is
    `time.Now()`.
  - The transfer currency is not checked against either account's currency,
    so cross-currency movement is neither supported nor refused.
  - No cut-off, provider capability or fee quote.
  - The protected-field hash and the BNK-06 fingerprint cover amount,
    currency and accounts only. That is all there is to cover, but §2
    invariant 21 names value date and purpose too.
- **A27. Liquidity is checked against a placeholder balance.** Create checks
  only `balance − amount < threshold`, and only when a threshold is
  configured. It never checks `balance ≥ amount`, and the balance is A9's
  0.00 placeholder. With a threshold set, every transfer is refused; without
  one, there is no liquidity check at all. "Insufficient/uncertain liquidity
  … blocks completion" is not really met.
- **A28. Thin events and evidence.** Recorded here rather than numbered
  separately:
  - Transfer events carry an empty `legal_entity_id`.
  - Approved, authorized, rejected, cancelled and returned carry no
    correlation ID.
  - `submitted` passes the payment-attempt ID as the correlation ID.
  - No event for ledger-posted or intercompany-paired.
  - The `returned` payload labels `authorizer_principal_id` as the actor.
  - Fiscal period is the calendar month (no fiscal-calendar dependency).
  - The same principal may both approve and authorize a cross-entity transfer
    (B9).

### BNK-10 — FX Exposure

- **A29. Every RecordFXRate failed — closed in this pass (aaaab68c).**
  `RecordFXRate` authorized with `legal_entity_id: ""`. authorization-svc
  refuses an empty legal entity with 400. The treasury authz client treats
  any non-200 as the authorization plane being down, so every call returned
  503 `authz_unavailable` and no rate could be recorded through the API.
  The handler tests use a mock authz and never saw it. It now asks for the
  `PLATFORM` scope: authorization-svc's `PlatformScopeSentinel`, which
  financial-control-svc and governance-decision-log-svc already send.
- **A30. A scenario with no recorded rate silently used parity — closed in
  this pass (aaaab68c).** `RunFXScenario` ignored "no rate recorded" and
  built the "current" side at a rate of 1.0, with a zero as-of time and no
  stale flag. It then reported the swing against parity as the change from
  current. This is the "Stale FX rate used without warning" negative path in
  its starkest form: no rate at all. It now refuses with 422
  `fx_rate_not_recorded`, as `GetFXExposure` does.
- **A31. Exposure population and lineage are thin.**
  - The required inputs are "foreign-currency bank balances; AR/AP/open
    financial items; intercompany/loan positions; approved forecast cash
    flows". Only AR/AP/obligations flows are composed (via
    `GetLiquidityForecastData`). Foreign-currency bank balances,
    intercompany and loan positions, and FIN forecasts are absent.
  - Functional currency is caller-supplied, not server-resolved per entity.
  - No monetary/non-monetary classification, no forward rates.
  - Buckets are hard-coded.
  - Rates are manually recorded with no source/type field: only pair, rate
    and effective time. The rate's ID stands in as `rate_version`.
  - The snapshot keeps no source-population hash ("Source population
    hashes").
  - Bucket order follows Go map iteration, so the stored JSON is not
    byte-reproducible.

  *Why not fixed:* each source is a new integration, and the REF FX source
  is a pre-production decision (§16).
- **A32. Staleness frozen at calculation; refresh not atomic or scoped;
  lineage read unauthenticated — closed in this pass (aaaab68c).** These are
  the same defects as A10/A13. Staleness is now derived on every read from
  the recorded rate's age (`domain.FXExposureStale`; a same-currency identity
  rate never ages). Refresh is one transaction, and refresh and supersede
  require a different, live snapshot for the same legal entity and currency
  pair. `GetSourceLineage` needed only a tenant header; it now needs view
  permission in the snapshot's legal entity.
- **A33. `RunFXScenario` uses the view permission, not
  `fx.scenario.run`.** It checks `TREASURY_POSITIONS_VIEW`. A distinct action
  needs authorization-svc configuration, so it is recorded here, not
  invented.

These BNK-10 rules are met and verified in the code:
- **"Assumptions/rates cannot be altered retroactively in a published
  snapshot."** Migration 000009's trigger rejects any change to rate,
  rate as-of, rate version, netting scope, buckets, totals,
  `has_stale_component` or calculator on every snapshot. Only status,
  publish fields and a one-time `superseded_by` may change. `fx_rates` is
  fully append-only.
- **"Analyst cannot convert scenario into payment/trade execution."** The
  scenario path imports no payment, ledger or intercompany client, and
  scenario output is never persisted.
- **"No silent netting across entities/currencies."** Every snapshot is one
  legal entity and one currency pair, with `netting_scope` recorded as
  `SINGLE_ENTITY:<id>`. The net figure nets receivables against payables
  within that scope only.

### Cross-cutting

- **A34. Money is `float64` end to end.** Transfer amounts, balances,
  thresholds, snapshot figures and FX rates are `float64` in Go, over
  `NUMERIC(20,4)` and `NUMERIC(20,8)` columns. The transfer fingerprint and
  protected-field hash format the float with `%.4f`, and the amount goes to
  BNK-06 as a JSON float. This is the same ZS-DATA-001 D06 class closed in
  payment-initiation-adapter-svc (A6 there). BNK-06 now refuses an amount its
  column would round, but this service can still lose digits before sending.
  *Why not fixed:* it touches every BNK-09 path, the hash and fingerprint
  formats (which BNK-06 re-derives), and every snapshot. It deserves its own
  pass with the `Decimal` type.
- **A35. Efficiency and transactionality, as asked.**
  - N+1: the cash views read balances one query and one transaction per
    account. Fixed: one `DISTINCT ON` query (`GetLatestCashBalances`).
  - BNK-09 commands now make a fixed number of reads per request: one
    transfer, two accounts, up to two authz calls and up to two verification
    reads. No per-row loops remain in the aggregation paths.
  - Calculation is one transaction (insert plus outbox events). Publish is
    one CAS update plus event. Refresh is now one transaction (A13/A32). A
    partially published snapshot is not possible.
  - Publish's stale check reads in a separate transaction from the CAS
    update. That is benign, because a snapshot can only become more stale,
    never less.
  - Every store method opens its own transaction, reads included.
  - AP/AR/obligations are read live from other services outside any
    transaction, so a snapshot's components are not a single consistent
    read. That is acceptable for an analytical view; the freshness record
    says so per component.

## B. Interpretations and additions (decisions a reviewer may want to reverse)

- **B1. Accounts stay ACTIVE at creation (A2).** Kept as the migration chose
  it; ownership verification, not status, is what BNK-09 gates on.
- **B2. The account list and transfer list require `legal_entity_id`.**
  Permission is granted per legal entity, so an unscoped list has nothing to
  be authorized against. A tenant-wide list would need its own permission.
- **B3. Amend is partial.** An empty field keeps its value, which also means
  amend cannot clear `branch_ref`, `country` or `account_type` to empty. A
  clear would need an explicit field.
- **B4. Account history excludes suspend/close reasons.** Left as built
  (A6).
- **B5. A missing balance marks a position stale, not 503.** A store error
  is an outage and fails closed. An ACTIVE account with no balance is
  missing data, so the position is still shown, flagged partial, and cannot
  be published. That matches "must show stale/partial confidence".
- **B6. `GetCashPosition` returns the latest snapshot whatever its status.**
  It may return CALCULATED, unpublished figures. A caller that wants only
  published positions has to filter. Left as built, since that is the store
  comment's stated intent, but it is a reasonable thing to tighten.
- **B7. Read-time staleness uses the thresholds that already existed.**
  Bank balances use 24h `BankBalanceStalenessThreshold`, and FX rates use 24h
  `FXRateStalenessThreshold`. Both are still constants; §16 makes them a
  certified configuration decision. AP/AR/obligations are treated as live at
  calculation, as the code already did. A snapshot's figures still age, but
  only the bank balance and the rate are judged stale.
- **B8. `has_stale_component` in a response is the derived value.** The
  stored column keeps the calculation's own verdict (immutable by trigger).
  Responses report the stored flag or the aged sources.
- **B9. A cross-entity transfer needs permission in both legal entities, and
  the same principal may approve and authorize it.** The SoD line says
  "cross-entity transfers may require dual-entity/controller approval". This
  pass checks every command's permission in both entities, which Create
  already did. It does not require the authorizer to differ from the
  checker; only the maker is excluded from both. Whether those must be
  different people is policy.
- **B10. Transfer reads use `TREASURY_POSITIONS_VIEW`.** The spec names
  `treasury.transfer.read`. There is no such action in this service, and
  every other treasury read uses the positions-view action, so reads follow
  that convention. A dedicated read action is an authorization-svc follow-up.
- **B11. `/transfers/{id}/fingerprint` stays principal-free.** It is the
  service-to-service read payment-initiation-adapter-svc calls to verify a
  treasury payment. It is tenant-isolated by RLS and returns only the status
  and an opaque hash of a transfer whose ID the caller already holds.
- **B12. There is no `GetScenario`.** Scenario output is never persisted:
  "scenario outputs remain analytical and separate from approved treasury
  actions". `RunFXScenario` returns the full result synchronously. A
  persisted scenario would be a new entity, so the query is absent by design.
- **B13. `RecordFXRate` is a platform-scope decision.** An FX rate is a
  market observation shared by every entity reporting in that pair, so it is
  authorized at `PLATFORM`, not per entity. Rates are still stored per
  tenant.
- **B14. Verification in this pass.** Everything was run on a fresh,
  disposable `postgres:16-alpine` container on port 55443 (not the shared dev
  database), using `TEST_DATABASE_URL=… go test -tags=integration ./... -count=1
  -p 1 -v`. The container held the tests' rows afterwards, which confirms the
  suite used it rather than embedded Postgres.
  - Baseline at HEAD: 113 passed, 0 failed, 0 skipped.
  - After all four fix commits: 141 passed, 0 failed, 0 skipped.
  - The 28 new tests are the reproductions and the proofs of each closed
    item.
  - The three BNK-09 store tests for A18 were written first and failed at
    HEAD.

  Commits:
  - e9cce85b (BNK-09: A18–A22, A25)
  - 8c90c0be (BNK-01: A1, A4)
  - 29baaaaf (BNK-08: A10–A13, A35)
  - aaaab68c (BNK-10: A29, A30, A32)
