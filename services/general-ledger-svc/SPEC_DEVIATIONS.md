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

Overall: all nine named commands/queries now exist with real guards (cycles,
optimistic version checks, append-only version history, half-open effective
intervals) and tests. What is missing is structural: the account has no
legal-entity/book dimension, no approval workflow or segregation of duties, no
events, idempotency keys only on the two replay-unsafe commands (B12), and
reason/evidence are optional. Those are A1–A13, recorded rather than guessed.
The deliberate design decisions a reviewer may want to reverse are B1–B12.

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
- **A4. No maker-checker / segregation of duties on high-risk changes.**
  §"Segregation of duties": "Creator cannot approve material control-account or
  hierarchy change where maker-checker is configured"; §"Authorization":
  `accounting.config.manage` / `accounting.config.approve for
  activation/high-risk changes`. The service checks one platform action per
  command (`COA_ACCOUNT_CREATE`, `COA_ACCOUNT_AMEND`, `COA_ACCOUNT_ACTIVATE`,
  `COA_ACCOUNT_DEACTIVATE`, `COA_ACCOUNT_REPARENT`, `COA_ACCOUNT_VIEW`) at the
  platform scope, with no second principal, no materiality notion and no
  distinction between ordinary and high-risk changes. The control-account
  posting override is at least a separate action
  (`COA_CONTROL_ACCOUNT_POSTING_OVERRIDE`). The spec's permission *names* are
  from the accounting domain; this platform's authorization contracts use the
  `COA_*` action codes, which is a naming translation, but the missing second
  signature is a real gap.
- **A5. No events are produced.** §23 expects AccountCreated,
  AccountActivated, AccountDeactivated and AccountHierarchyChanged;
  ACC-01 commands enqueue nothing. ACC-04's posting engine emits journal
  events, so the infrastructure exists, but defining four new cross-service
  event types (and their payload contracts) is a platform contract decision,
  not something to invent per command. Consumers that need to react to chart
  changes today must poll.
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
- **A8. Reason and evidence are optional, and no source policy is retained.**
  §"Evidence / lineage": "Exact account version, approver, reason, effective
  dates and source policy retained with every material change." Every version
  row retains version number, effective interval, actor and timestamp; reason
  and `evidence_refs` are accepted and stored **when supplied but never
  required**, and approver/source policy are absent (A3/A4). If evidence is to
  become mandatory the refusal belongs in the handler/store for all five
  commands at once.
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
  permissive. Migration 000018's down migration refuses while any row still
  carries a designation, so the evidence cannot be erased while a journal
  depends on it.

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
- **B13. CLOSE_REVIEW admits a journal-level designated close journal, not a
  permission-only bypass (closes financial-close-svc A4).** ACC-14's posting
  table says CLOSE_REVIEW permits "Only designated close journals/workflows"
  but defines neither the designation nor the mechanism. This service now
  persists a nullable `journal_headers.close_journal_designation` (migration
  `000018_add_close_journal_designation`) from a closed allowlist:
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
