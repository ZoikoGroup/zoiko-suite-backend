# Spec deviations — general-ledger-svc (ACC-01 Chart of Accounts)

Governing document: `docs/architecture/original_doc/ACC_spec.txt`, §4 "ACC-01 —
Chart of Accounts" (contract fields ~L257–324, wireframe L326–363, negative
paths L365–397), the §23 event table (ACC-01: AccountCreated;
AccountActivated; AccountDeactivated; AccountHierarchyChanged) and the §24
canonical data model (AccountMaster / AccountVersion: entity, book, code,
class, parent, effective interval, posting/control restrictions).

Scope note: this is the first deviations file for this service and covers the
ACC-01 surface completed in the command-per-commit pass — `AmendAccountVersion`,
`ActivateAccount`, `ReparentAccount`, `ResolvePostingEligibility`,
`GetAccountAsOf` (on top of the existing `CreateAccount`, `GetAccount`,
`ListAccounts`, `DeactivateAccount`). Later ACC-0x surfaces will extend it.
The 2026-10-26 ACC-14 close-review extension is recorded in A14 and B13.
The 2026-10-26 ACC-05 `QueryLedger` date-window extension (for
reconciliation-intelligence-svc's A2a candidate search) is recorded in A15 and
B14.

Overall: all nine named commands/queries now exist with real guards (cycles,
optimistic version checks, append-only version history, half-open effective
intervals) and tests. Lifecycle events (A5), maker-checker (A4), mandatory
reason for material control-account/hierarchy changes, and an optional
source_policy_ref (A8) are closed; what remains missing is structural: the
account has no legal-entity/book dimension (A1), no Draft→Approved state (A3)
outside the gated path, and evidence_refs is still optional everywhere.
Those and the rest are A1–A15, recorded rather than guessed. The deliberate design
decisions a reviewer may want to reverse are B1–B18.

## A. Not implemented / weaker than the spec

- **A1. The account has no legal-entity/book dimension.** The spec's purpose
  ("Maintain the versioned posting-account master and hierarchy **for each
  legal entity and accounting book**"), its required inputs (`entity_id;
  book_id`) and §24's AccountMaster/AccountVersion ("entity, book, code…") all
  make the entity/book pair part of an account's identity. This service keys
  accounts by `tenant_id + account_code` only; no `entity_id`/`book_id` column
  exists anywhere in ACC-01, and nothing in posting/eligibility evaluates an
  entity/book constraint. Adding them would re-key every existing account and
  the FKs that reference account codes (journals, ledger entries, mappings), so
  it is a migration + identity decision for the owning product, not a silent
  addition here. Consequence: two entities in one tenant cannot hold different
  versions of the same account code, and `GetAccountAsOf` answers for the
  tenant, not a book.
- **A2. `normal_balance` is not implemented. — Closed (B11).** Every
  account response now carries a read-only `normal_balance`, derived from
  `account_type` by `AccountType.NormalBalance()`: ASSET and EXPENSE are
  `DEBIT`; LIABILITY, EQUITY and REVENUE are `CREDIT`. Nothing is stored.
  There is no column and no migration, so it can never contradict the type.
- **A3. No Draft → Approved state and no approver on the record.** The spec's
  state model is "Draft → Approved → Active → Inactive/Superseded", and
  "approver … retained with every material change". Accounts here are created
  ACTIVE immediately; every version row records the acting principal, reason
  (optional) and evidence (optional), but no approver, because there is no
  approval step. The journal lifecycle (ACC-03) has a real prepare/approve
  flow; account-config approval would need the same treatment across
  Create/Amend/Activate/Reparent, which is a control-design decision.
- **A4. No maker-checker / segregation of duties on high-risk changes. —
  Closed 2026-11-07 for the two changes the spec's own SoD line names.**
  §"Segregation of duties": "Creator cannot approve material control-account or
  hierarchy change where maker-checker is configured". `ReparentAccount` and
  `AmendAccountVersion` no longer apply a change in one call when a control
  account is on either side of it (the account itself, or — for a reparent —
  its new parent): they record a pending `AccountChangeApproval` instead (the
  account is left untouched) and a new command, `ApproveAccountChange`, is the
  only way to apply it — refusing an approver who is the same principal who
  proposed it, enforced both in the application layer and by a database CHECK
  constraint (`sod_account_change_approver_independent`, migration 000031),
  same pattern as financial-close-svc's own `sod_*` constraints. An ordinary,
  non-control account's reparent or amendment is completely unaffected: still
  one call, applied immediately, exactly as before. See B16.
  Still open: the broader "Authorization" line
  (`accounting.config.manage` / `accounting.config.approve for
  activation/high-risk changes`) remains a single platform action per command
  with no distinct approve-vs-manage permission split, and `CreateAccount`/
  `ActivateAccount`/`DeactivateAccount` have no maker-checker at all — the spec
  names only "material control-account or hierarchy change" as requiring it,
  which these three are not (creating an account is not yet a change to one;
  activate/deactivate are status flips the spec's own SoD line does not name).
- **A5. No events are produced. — Closed 2026-11-07.** §23 expects
  AccountCreated, AccountActivated, AccountDeactivated and
  AccountHierarchyChanged. `CreateAccount`, `ActivateAccount`,
  `DeactivateAccount` and `ReparentAccount` now each enqueue their named
  event (`com.zoikosuite.accounting.account.created` /`.activated` /
  `.deactivated` / `.hierarchy-changed`) to the transactional outbox in the
  same transaction as the write, the same `envelope`/`outbox` mechanism
  ACC-02's `enqueueMappingEvent` already uses — no new cross-service
  contract was invented, this reuses the existing one. Accounts are
  tenant-scoped, not legal-entity-scoped (A1), so `legal_entity_id` is
  always "" on these events, the same posture ACC-02's mapping events take.
  `AmendAccountVersion` names no event in the spec's list and emits none —
  it is not a hierarchy change or a status change, and inventing an event
  the spec doesn't name was judged worse than leaving a non-hierarchy
  amendment silent.
  - *Manual check:* `internal/store/acc01_account_versioning_integration_test.go`
    — `TestPgStore_AccountLifecycle_EmitsNamedEvents` (real Postgres: one
    outbox row per command, keyed by the account's own `aggregate_id`;
    explicitly asserts `AmendAccountVersion` emits none).
- **A6. No `Idempotency-Key` handling on ACC-01 commands. — Closed for
  `AmendAccountVersion` and `ReparentAccount` (B12); the other three commands
  remain naturally idempotent as described here.** The wireframe lists it as a
  mandatory control on all five commands. `CreateAccount` relies on the unique
  account code (a retry answers 409, not a replay);
  `ActivateAccount`/`DeactivateAccount` retries are naturally idempotent by
  status; `AmendAccountVersion` and `ReparentAccount` retries with the key
  omitted can append a second identical version — the gap B12 closes.
  `expected_version` narrows the window but is not an idempotency key. See also
  B4.
- **A7. The backdated-hierarchy guard is an approximation with no period
  linkage.** Negative path 3 requires blocking a hierarchy change backdated
  "across closed period". This service has no period or close client (accounts
  carry no entity/book to ask a close service about, see A1), so `ReparentAccount`
  refuses **any** effective date before today's UTC date (`hierarchy_backdated`).
  That is stricter than the spec in one direction (a same-day-but-earlier
  instant or an open-period backdate is also refused) and weaker in another
  (it never consults a close state, and a reparent dated today while the
  period was already hard-closed would pass). Revisit when entity/book and a
  period service exist.
- **A8. Reason and evidence are optional, and no source policy is retained. —
  `reason` closed for material changes (B17); `source_policy_ref` now
  captured, optional, everywhere (B18); `evidence_refs` optionality remains
  open (2026-11-07).** §"Evidence / lineage": "Exact account version,
  approver, reason, effective dates and source policy retained with every
  material change." Every version row retains version number, effective
  interval, actor and timestamp. `reason` is now **required**, refused with
  `ErrAccountChangeReasonRequired`, on exactly the same gate A4/B16 already
  defines as "material" — a control account on either side of a reparent or
  amendment — the one place this codebase has a formal definition of
  "material change"; it stays optional everywhere else (an ordinary account's
  change, or Create/Activate/Deactivate, none of which the spec's SoD/evidence
  lines name as material). `source_policy_ref` is a new optional, free-text
  column on `account_versions`/`account_change_approvals` (migration 000032):
  the spec names "source policy" but gives it no shape, so this is a
  reference string a caller supplies, the same posture `evidence_refs`
  already takes, not a foreign key into a policy table this service has no
  model for; approver is covered for the gated path by A4's
  `account_change_approvals.approved_by_principal_id`. `evidence_refs`
  remains optional everywhere, including the gated path — making it
  mandatory too would need the same gated-path treatment `reason` just got,
  not a platform-wide requirement; recorded as still open rather than
  bundled in without its own review.
- **A9. Unregistered account codes remain postable.** Failure semantics say
  "Inactive/non-posting/control-restricted account blocks posting", but ACC-04
  deliberately lets codes the chart has never seen post (the bootstrap gap —
  every historic caller posts codes nothing ever registered).
  `ResolvePostingEligibility` therefore answers HTTP 200 with
  `registered=false, eligible=true` for an unknown code, rather than 404 or a
  block. This is preserved behavior, not an endorsement: closing the gap is a
  flag-day decision across all posting callers.
- **A10. Negative path 4 ("Activate duplicate account code") has no literal
  counterpart.** Account codes are uniquely constrained per tenant at
  creation, so two accounts cannot share a code and "activating a duplicate"
  cannot occur. The nearest behaviors: creating a duplicate → 409
  `account_already_exists`; activating an already-active account → idempotent
  200 that appends no version (B3). Recorded so the mapping is explicit.
- **A11. Negative path 2 ("Delete account referenced by posted journal") is
  vacuous: there is no delete.** No command or route deletes an account or a
  version; the append-only history makes version deletion unreachable by
  design. Posted history therefore cannot be orphaned by a delete. If a
  lifecycle end-state is wanted it should be Deactivate/Superseded, not a
  DELETE.
- **A12. Parent-level balance aggregation is not defined.** The spec's figure
  (L399–402) shows a chart hierarchy, and ReparentAccount maintains the tree,
  but no contract in this service defines what a parent's balance is. ACC-05
  balances are keyed by `account_code`; reparenting moves no balances and
  changes no ledger entry (asserted by an integration test). Any roll-up
  (include descendants? which sign?) is a reporting decision for a consumer,
  not implemented here.
- **A13. Routes follow the service's house style, not the spec's illustrative
  surface.** The wireframe's `/internal/v1/acc01/...` paths are labeled
  illustrative; the implemented routes are
  `POST /v1/chart-of-accounts/{account_code}/amend|activate|deactivate|reparent`,
  `GET|POST /v1/chart-of-accounts/{account_code}/posting-eligibility`,
  `GET|POST /v1/chart-of-accounts/{account_code}/as-of` and static
  `/resolve-posting-eligibility` and `/as-of` variants that read the code from
  body/query. An internal alias surface was not added; if a gateway contract
  needs one it should be a routing change, not a second implementation.
- **A14. The period-state guard is still API-layer only; no database guard.**
  This service decides HardClosed / CLOSE_REVIEW / SOFT_CLOSE by calling
  financial-close-svc before create, validate-commit, post, approved-post,
  adopt and reprocess. There is no database constraint or trigger that refuses
  a direct INSERT/UPDATE of a journal row into a closed period; a future
  DB-level guard is financial-close-svc's A5 and is deliberately not fixed
  here. The new `journal_headers.close_journal_designation` column is part of
  the row such a guard must consult: a guard that knows only the period state
  would either refuse a legitimate designated close journal or be too
  permissive. Migration 000027's down migration refuses while any row still
  carries a designation, so the evidence cannot be erased while a journal
  depends on it.
- **A15. `QueryLedger` had no cursor and returned float amounts — both closed
  (2026-10-30, 2026-11-01).** ACC-05's read contract is a bounded list.
  - *No cursor — closed 2026-10-30.* `GET /v1/ledger/entries` now takes
    `after_entry_seq` and its response is `{"entries": [...],
    "next_cursor": <entry_seq>}` (was a bare array): `next_cursor` is the
    last returned entry's own `entry_seq`, present exactly when more rows
    exist past the page, same keyset-pagination shape used elsewhere in
    this codebase (e.g. banking-connector-svc's `ListTransactions`). The
    store always fetches `limit+1` rows and trims the extra one, so
    truncation is never inferred from `len(entries) == limit` alone.
    `GET /v1/ledger/as-of` (`QueryLedgerAsOf`) is unchanged — still a bare
    array capped at `limit` — because a point-in-time snapshot read has no
    pending continuation to cursor through; it shares `QueryLedgerFilter`
    and `PgStore.QueryLedger` but ignores the new `hasMore` return.
    `limit > MaxListLimit` (1000) is still silently replaced by the
    default (200) rather than clamped or refused — unrelated to this
    close, left as found.
    - *Only known caller.* `reconciliation-intelligence-svc/internal/ledger`
      was the sole consumer of the old bare-array shape (checked by
      grepping every service for `v1/ledger/entries`); it is updated in the
      same pass (A2b part 4) to decode the new envelope and actually follow
      `next_cursor` instead of refusing a full page.
    - *Tests:* `internal/handler/acc05_ledger_test.go`
      (`TestQueryLedger_CursorSignalsTruncationAndPagesForward`,
      `TestQueryLedger_MalformedCursor_Returns400`, and the pre-existing
      scope test updated for the new envelope);
      `internal/store/ledger_window_pg_smoke_test.go`
      (`TestPgStore_QueryLedger_CursorPagesThroughEveryEntryExactlyOnce`,
      real Postgres: every entry returned exactly once, in increasing
      `entry_seq` order, across a page boundary that falls inside one
      journal's own two lines).
  - *Float amounts — closed 2026-11-01.* `LedgerEntry.DebitAmount`/
    `CreditAmount` are now exact decimal strings on the wire (`type string`,
    same `json:"debit_amount"`/`"credit_amount"` tags), the same change
    `LedgerBalance` already had. `ledger_entries.debit_amount`/
    `credit_amount` are NUMERIC(18,2); pgx already scans NUMERIC into a Go
    `string` exactly (proven in production by `QueryAccountBalance`'s
    `LedgerBalance.DebitTotal` et al.), so `QueryLedger` and
    `QuerySourceEntries` needed no SQL change — only the struct field type,
    in `internal/domain/types.go`. NUMERIC(18,2)'s own range reaches above
    2^53 minor units (e.g. $90,071,992,547,409.93 is 9,007,199,254,740,993
    cents, one more than 2^53), the exact magnitude at which a float64
    wire value stops round-tripping exactly — this closes that gap for any
    amount the column can hold, not just the sub-2^53 range the old
    json-number-text workaround covered.
    - *Consumer.* reconciliation-intelligence-svc's `internal/ledger`
      client already decodes these fields as `json.Number` and parses them
      exactly via `exactAmount`/`big.Rat`. `encoding/json` accepts a quoted
      numeric string into a `json.Number` field exactly as it accepts a
      bare number (verified: `TestListCashAccountEntries_OutOfScopeRowFailsClosed`'s
      pre-existing `"unparseable money"` case already fed a quoted JSON
      string through this same field), so the client needed no code
      change — only a locked-down regression test pinning that behavior
      down.
    - *Scope.* `JournalLine.DebitAmount`/`CreditAmount` (the create-journal
      request/response shape) and `appendLedgerEntries`'s own internal
      float64 hop from `journal_lines` into `ledger_entries` are unchanged
      and out of scope here — A15 names `LedgerEntry`'s wire shape
      specifically, and `journal_lines.debit_amount`/`credit_amount` are
      NUMERIC(18,2) same as `ledger_entries`, but every write path into
      them still carries a float64 in Go first. Fixing that would be a
      second, separate, larger change (every `CreateJournalRequest`
      caller), not attempted here.
    - *Tests:* `internal/handler/acc05_ledger_test.go`
      (`TestQueryLedger_AmountsAreExactDecimalStringsOnTheWire` — decodes
      the response as raw JSON and asserts `debit_amount` is the quoted
      string token, not a bare number); `internal/store/
      ledger_entry_exact_amount_pg_smoke_test.go` (real Postgres: a row
      inserted directly at $90,071,992,547,409.93 — one cent past 2^53 —
      round-trips through `QueryLedger` as the exact same string);
      reconciliation-intelligence-svc's `internal/ledger/client_test.go`
      (`TestListCashAccountEntries_AcceptsQuotedDecimalStringAmount`).

## B. Interpretations and additions (decisions a reviewer may want to reverse)

- **B1. The design is "head row + append-only history".** The existing
  `chart_of_accounts` row remains the current head (so every existing reader
  and FK that references the account keeps working unchanged), and migration
  `000016_add_account_versioning` adds `account_versions`, the append-only
  history. Existing rows are backfilled as version 1 with `effective_from =
  created_at`. Every command writes history + head in one transaction under a
  `SELECT … FOR UPDATE` row lock.
- **B2. Version intervals are half-open `[effective_from, effective_to)` and
  `GetAccountAsOf` requires an explicit `as_of`.** A version that starts
  exactly at the queried instant is the one in force; the queried instant never
  belongs to two versions. `as_of` is refused when missing rather than
  defaulted to "now", because a historical read with an accidental current
  default returns a plausible wrong answer. A read before the first version is
  a 404. `EffectiveDate` wire values: a date-only string means the end of that
  UTC day (the inclusive convention callers mean by "effective from 1 July"),
  RFC3339 is honored exactly. The spec names effective dates as inputs but does
  not prescribe the wire form or interval closure; both are recorded here.
- **B3. Activate/Deactivate/Reparent append versions too.** The spec lists only
  `AmendAccountVersion` as a versioned command, but its state model says
  "historical versions queryable" and "Superseded", so every material change
  (name/type/parent/control flags/status) closes the prior interval and
  appends a version. Re-issuing the status the account is already in is an
  idempotent no-op that writes no version; the head advances in lockstep.
- **B4. `expected_version` is optional when omitted but real when supplied.**
  Existing `DeactivateAccount` callers send no version, so requiring it would
  be a flag day; when a caller supplies `expected_version` it is enforced under
  the row lock and a mismatch answers 409 `account_version_conflict` with no
  version appended. The spec's "expected_version where stateful" is read as
  this opt-in form; A6's idempotency key is the missing half.
- **B5. Reparent refuses self/descendant cycles and unnamed parents.**
  A recursive ancestor walk (recursive CTE in the store; mirrored in the test
  stub) refuses a cycle with 422 `hierarchy_cycle`; an unknown parent is 400.
  A request that omits `parent_account_id` entirely is refused, while an
  explicit `null` makes the account a root — the two cases must not be
  conflated. Re-parenting to the parent it already has is an idempotent no-op.
- **B6. Posting eligibility is served as a stable read shape preserving
  ACC-04's exact rules.** `ResolvePostingEligibility` returns
  `registered/eligible/status/version/requires_override/override_applied/
  reason`; unregistered → eligible (A9), INACTIVE → not eligible, control +
  restricted → eligible only with both `override_control_account_restriction`
  and the separate `COA_CONTROL_ACCOUNT_POSTING_OVERRIDE` authorization.
  ACC-04's inline `checkAccountRestrictions` in `PostAccountingEvent` is
  deliberately left untouched — the read surface formalizes it without
  regressing the engine. Wiring the engine onto the resolver is a follow-up
  that should change both together.
- **B7. Activate and Deactivate share one transition path with an optional
  body.** `decodeOptionalJSON` keeps body-less `DeactivateAccount` requests
  working while allowing `expected_version`, `effective_from`, `reason` and
  `evidence_refs` on either command; `ActivateAccount` is the exact inverse of
  the existing Deactivate, which is the property that matters (a reactivated
  account must be postable again — covered by a test).
- **B8. Error mapping is explicit and stable.** 404 `account_not_found`;
  409 `account_version_conflict`; 400 for unknown parent or missing fields;
  422 `account_effective_date` (date before the current interval),
  `hierarchy_cycle`, `hierarchy_backdated`. This is house-style stable codes
  rather than the spec's unspecified error envelope.
- **B9. Authz scope is the platform CoA scope.** ACC-01 is tenant-wide with no
  entity to scope on (A1), so commands authorize against
  `coaPlatformScopeID` with the `COA_*` actions (A4); the posting override is
  additionally checked at the caller-declared `legal_entity_id` when supplied,
  falling back to the platform scope when not. When entity/book land (A1),
  authorization should move to the account's entity.
- **B10. Verification of B1–B9.** Handler tests (`internal/handler/handler_test.go`):
  amend head/version, 409 stale version, explicit-null parent, activate
  inverse + idempotence, reparent move/cycle/self/backdate/missing-parent,
  eligibility matrix including the denied override, and as-of v1/v2 at
  interval boundaries, missing `as_of` → 400, before-first-version → 404.
  Integration tests (`internal/store/acc01_account_versioning_integration_test.go`,
  Postgres 16; embedded server when `TEST_DATABASE_URL` is unset): version
  appends and prior-interval closure read directly from `account_versions`,
  activate/deactivate round trip, reparent leaves posted `ledger_entries`
  untouched, and `GetAccountAsOf` reconstructs v1 (closed at the amendment),
  v2 at the half-open boundary and 404 before creation. `go build ./...`,
  `go test ./...` and the integration suite are green.
- **B11. `normal_balance` is derived on output, not stored (A2).** It is
  computed in `MarshalJSON` on `domain.Account` and `domain.AccountVersion`,
  the two shapes every account endpoint returns: CreateAccount, GetAccount,
  ListAccounts, AmendAccountVersion, Activate/Deactivate/ReparentAccount
  (Account) and GetAccountAsOf (AccountVersion, using the type in force in
  that version). No handler builds the field itself, so a future account
  endpoint gets it automatically, and no command's request, validation or
  write path changed. A `normal_balance` sent in a request is ignored; it
  is not an input. The spec says that for control account classes "the
  server is expected to resolve" the normal balance. Type is the only input
  today, and a control account takes its type's side. If a contra or
  control class that reverses the side is ever modelled, the derivation
  belongs in `NormalBalance()`, still not in a column. An unrecognised type
  yields no `normal_balance` rather than a guessed one. Covered by
  `internal/handler/acc01_normal_balance_test.go`: the mapping for all five
  types, the field on Create/Get/List/as-of responses, and a request
  carrying `normal_balance` being unable to set it.
- **B12. `AmendAccountVersion` and `ReparentAccount` require and honor an
  `Idempotency-Key` (closes A6).** Migration
  `000017_add_command_idempotency` adds `command_idempotency (tenant_id,
  idempotency_key, command, account_code, created_at, PRIMARY KEY (tenant_id,
  idempotency_key))`, RLS-scoped like the other tables and shaped after
  financial-control-svc's table of the same name. The lookup-then-record order
  is copied from that service's `BeginExecution`
  (`internal/store/execution.go`): both store methods look the key up first
  inside the write transaction; a key recorded for the same command and account
  replays the account as it now stands with `Idempotent-Replay: true` and
  writes no version; a key recorded for a different command or account fails
  409 `idempotency_key_conflict`; otherwise the command executes and the key is
  recorded only after the write has succeeded, in the same transaction, so a
  refused command (stale `expected_version`, cycle, backdate) never consumes
  its key. The handler reads the header, answers 400 `missing_idempotency_key`
  when it is absent, and passes the key straight to the store — request,
  validation and write path only, no response shape changed. A replay returns
  the head as it stands now, not a snapshot of the first response: the same
  semantics as the reference service's run replay, where a later command may
  have advanced the record but the retry still never re-executes. Consequences
  a reviewer should know: keys are tenant-scoped and never expire (the
  reference table does not expire them either); `CreateAccount`,
  `ActivateAccount` and `DeactivateAccount` do not participate because A6's
  rationale still stands (unique code / status make them naturally idempotent);
  the same key with a different payload for the same command and account
  replays the original result rather than comparing bodies, as the reference
  service does. Covered by `internal/handler/handler_test.go` (missing key 400
  for both commands, replay at version 2 with `Idempotent-Replay: true` and no
  second version, cross-command reuse 409) and by
  `internal/store/acc01_account_versioning_integration_test.go` (amend and
  reparent repeated under one key with `account_versions` and
  `command_idempotency` row counts read directly, cross-command conflict, and a
  stale-version refusal that does not consume the key so the corrected retry
  runs).
- **B13. A1 is answered with entity- and book-scoped account versions, not a re-keyed chart.** The chart stays tenant-wide for what an
  account *is*: its code, type and control nature mean the same thing in every entity of a tenant, and every journal line, ledger entry,
  mapping and join keeps pointing at that one row. Re-keying the identity itself would change every one of those references and
  every service that sends an `account_code`, for no behaviour a scoped version does not already give. What can legitimately differ by
  entity or book is now versioned per scope (migration 000026, `account_scope_versions`, append-only, one open version per scope):
  - *What a scoped version sets.* The account name, its parent in the hierarchy and whether it is open for use (ACTIVE or INACTIVE).
    A field left unset inherits from the less specific version. The most specific version in force on a date wins, field by field:
    book, then entity, then the tenant version. Each scope numbers its own versions; a newer one closes the one before it, and an
    earlier date than the open one is refused (`effective_date_invalid`).
  - *API.* `POST` and `GET /v1/chart-of-accounts/{code}/scoped-versions` (action `COA_ACCOUNT_AMEND` / `COA_ACCOUNT_VIEW`, checked on
    the legal entity named). `GET .../as-of` and `.../posting-eligibility` take `legal_entity_id` (and `book_id`) and answer for that
    scope, saying for each of name, parent and status whether it came from the BOOK, the ENTITY or the TENANT. Without them the answer is
    the tenant version, exactly as before.
  - *Where the scoped status is enforced.* The kernel posting engine (an account closed for a book is refused for that book, dated from
    the version's effective date, so history is unchanged) and the legacy journal path (`account_inactive`). A closed account in one
    entity stays open in another.
  - *Scoped hierarchy.* A scoped parent is checked against the hierarchy as that scope sees it, so a loop is refused
    (`hierarchy_cycle`); another entity can hold the opposite arrangement. It is for reading and reporting: whether an account may be
    posted to (a header account has children) is still decided by the tenant hierarchy.
  - *Still tenant-level.* Account type, control type, direct-posting restriction and book applicability (`PUT /v1/account-applicability`).
    A1's consequence "two entities cannot hold different versions of the same account code" no longer holds for the facts above; it
    still does for the type.
- **B14. CLOSE_REVIEW admits a journal-level designated close journal, not a
  permission-only bypass (closes financial-close-svc A4).** ACC-14's posting
  table says CLOSE_REVIEW permits "Only designated close journals/workflows"
  but defines neither the designation nor the mechanism. This service now
  persists a nullable `journal_headers.close_journal_designation` (migration
  `000027_add_close_journal_designation`) from a closed allowlist:
  `ACCRUAL_RECOGNITION` (ACC-07), `PREPAYMENT_RECOGNITION` and
  `PREPAYMENT_TERMINATION` (ACC-08). Every value must agree with the journal's
  ACC-03 type — all three are `ACCRUAL` — and the database CHECK repeats that
  rule. A caller cannot invent a designation; an unrecognised value or a
  type mismatch is 422 `invalid_close_journal_designation` even in an OPEN
  period. The designation is not the SOFT_CLOSE override: `SOFT_CLOSE`
  (`RESTRICTED`) still requires `override_soft_close=true`, a non-empty
  `soft_close_override_reason`, and `GL_SOFT_CLOSE_POSTING_OVERRIDE`; that
  override is not a CLOSE_REVIEW mechanism and an ordinary journal carrying it
  is still refused in CLOSE_REVIEW. During `CLOSE_JOURNALS_ONLY`, a valid
  designation is honored only when the poster also holds the separate
  `GL_CLOSE_REVIEW_POSTING` action (registered by access-control-svc migration
  `000019`), checked at creation, validation-commit, posting, approved posting,
  adoption and reprocessing. Registration only makes the action authorable; a
  deployment must still grant it to the principal that posts close journals
  (financial-close-svc uses the human request principal). A `CLOSED` period
  returns `ErrPeriodHardClosed` and is never turned into an allowance by a
  designation.
  The external refusal for an ordinary non-designated journal in CLOSE_REVIEW
  remains HTTP 412 `period_locked`, exactly as before; internally the close
  client now distinguishes `ErrCloseJournalsOnly` from
  `ErrPeriodHardClosed` so the handler can make the designated-only decision.
  The created outbox event carries the designation as audit evidence.
  Verification: handler tests in `internal/handler/close_review_posting_test.go`
  (ordinary CLOSE_REVIEW refusal, designated create/post, missing action,
  invalid and type-mismatched designations, hard-closed refusal),
  `internal/handler/posting_engine_test.go` (ordinary ACC-04 event refusal),
  `internal/close/client_test.go` (policy mapping), store integration tests in
  `internal/store/close_journal_designation_store_test.go` (round trip, outbox,
  DB CHECK, and the guarded down migration refusing while a row depends), and
  financial-close-svc's `internal/handler/close_journal_meta_test.go` plus
  `internal/clients/gl_journal_contract_test.go` (request shape for ACC-07/08
  recognition and termination, SOFT_CLOSE override only in SOFT_CLOSE).
- **B15. `QueryLedger` gains a transaction-date window and a journal-status
  filter (2026-10-26).** Added for reconciliation-intelligence-svc's A2a: it
  proposes the book side of an UNMATCHED bank statement line from one cash
  account's entries near the statement date. ACC-05 names `QueryLedger` as
  its read contract, so this extends that route in its existing `?field=`
  style rather than adding a search route.
  - *Parameters:* `transaction_date_from` / `transaction_date_to`
    (YYYY-MM-DD, inclusive, either end optional) and `journal_status`. They
    are accepted on `/v1/ledger/entries` and on `/v1/ledger/as-of`, which
    builds the same filter.
  - *Validation (400):* a malformed date, `from` after `to`, or a status
    other than FINALIZED or REVERSED.
  - *Why a status filter:* `ledger_entries` is append-only, so a REVERSED
    journal's entries stay listed. bank-reconciliation-svc refuses to match
    a journal that is not FINALIZED, so a reader looking for live postings
    asks for `journal_status=FINALIZED`. The filter is one EXISTS probe on
    `journal_headers`' primary key per candidate row, not a second query.
  - *transaction_date, not posting_date:* the business date of the cash
    movement is what a bank statement date reflects. posting_date can be
    the period end of an accrual.
  - *Index:* migration 000028 adds `idx_ledger_entries_account_txn_date`
    `(tenant_id, legal_entity_id, account_code, transaction_date)`. The
    existing scope index puts `book_id` before `account_code` and carries no
    date.
  - *Why the down migration has no refusal guard:* the up adds only an
    index, which holds no data. Dropping it loses nothing a re-run cannot
    rebuild; the read stays correct, only slower.
  - *Verification:*
    - handler tests in `internal/handler/acc05_ledger_test.go`: the filter
      reaches the store from both routes, and malformed input gets 400;
    - `internal/store/ledger_window_pg_smoke_test.go` on real Postgres: the
      window is inclusive at both ends, and FINALIZED drops a reversed
      journal and keeps its reversal;
    - 000001–000028 applied from empty with `psql --single-transaction -v
      ON_ERROR_STOP=1`, and 000028 taken down and up again by hand.
- **B16. ACC-01 maker-checker is scoped to control accounts, not every
  hierarchy change or every account (2026-11-07).** A4 closes the spec's SoD
  line for exactly what it names — "material control-account or hierarchy
  change" — not every `ReparentAccount`/`AmendAccountVersion` call.
  - *Why control-account-scoped, not every reparent:* a control account
    (`is_control_account = true`) is the fraud-relevant and rollup-relevant
    case this control exists for — an account other parts of the platform (or
    a bank/intercompany reconciliation) trust without re-checking, and the one
    place a single insider moving or redefining it unilaterally is the actual
    risk. An ordinary account's reparent (e.g. reorganising expense
    sub-accounts) changes no control account's standing and is not gated,
    matching the spec's own word "material" rather than reading it as every
    hierarchy edit.
  - *Mechanism — a side table, not a new state on `account_versions`:*
    `account_change_approvals` (migration 000031) records a pending change;
    `account_versions`/`chart_of_accounts` are untouched until
    `ApproveAccountChange` applies it. This keeps the existing "at most one
    open version per account" invariant and the ordinary, ungated path
    exactly as they were — no existing caller or test changed behaviour.
  - *Re-validated at approval time, not just at proposal time:* `ApproveAccountChange`
    re-checks `expected_version` against the account's current version under
    a fresh row lock, so a change the account has moved past in the meantime
    is refused (`ErrAccountVersionConflict`), not silently replayed against
    stale facts.
  - *Events:* the A5 lifecycle event (`hierarchy-changed` / `amended`) fires
    at approval time, when the change is real, not at proposal time, when it
    may never be approved.
- **B17. `reason` becomes mandatory on the same gate B16 defines as
  "material", not on every command (2026-11-07).** A8's evidence/lineage line
  wants reason retained with "every material change"; this codebase has no
  general notion of "material" to hang that on except the one B16 already
  built. Reusing it, rather than inventing a second materiality test, means
  `reason` is required exactly where a human will actually read it back
  (reviewing a control-account change) and exactly nowhere an existing caller
  — test or real — was already passing an empty one, since every existing
  account in this codebase is non-control and so never reaches the gate.
  - *Why not make it mandatory everywhere:* every one of the five commands'
    existing callers may omit `reason` today; requiring it platform-wide would
    refuse live traffic with no deprecation window, not just fail new tests —
    the same "not a silent addition" reasoning A1 and A9 are recorded under.
  - *Verification:* `internal/store/acc01_maker_checker_integration_test.go`
    — `TestPgStore_MaterialChange_RequiresReason` (real Postgres: a blank or
    whitespace-only reason is refused on both `ReparentAccount` and
    `AmendAccountVersion` for a control account; an ordinary account's
    amendment with no reason still applies immediately, unaffected).
- **B18. `source_policy_ref` is a free-text reference, captured on every
  reparent/amendment, not only the gated path (2026-11-07).** Unlike `reason`
  (B17), nothing here gates whether `source_policy_ref` may be supplied —
  it's accepted and stored on `ReparentAccount`/`AmendAccountVersion`
  regardless of materiality, landing on the pending `AccountChangeApproval`
  for a gated change or directly on the new `account_versions` row otherwise.
  It is never required; `Create`/`Activate`/`Deactivate` do not accept it,
  matching reason/evidence's existing scope to the four commands that produce
  a version with a story behind it.
  - *Why free text, not a typed reference:* the spec's wireframe gives
    "source policy" no type and no referenced entity — this service has no
    policy/workflow table to point a foreign key at, and inventing one here
    would be designing a different capability's data model, not filling
    ACC-01's own gap.
  - *Verification:* `internal/store/acc01_maker_checker_integration_test.go`
    — `TestPgStore_SourcePolicyRef_RoundTrips` (real Postgres: a
    `source_policy_ref` supplied on `ReparentAccount` appears on the pending
    `AccountChangeApproval` immediately, and on the `account_versions` row
    once `ApproveAccountChange` applies it).
