# Spec deviations — payment-status-svc (BNK-07)

Governing document: `ZoikoSuite_Banking_Cash_Treasury_Detailed_Service_Specifications_v1_0.docx`,
§7 "BNK-07 — Payment Status & Confirmation", its statement of purpose
("Resolve and preserve canonical provider/network payment execution state and
finality from authenticated callbacks, polls, bank reports and statement
evidence"), the §2 invariants 15, 17, 18, 19 and 25, the §4 shared contract
(context, concurrency, idempotency, errors, external state, evidence), the §8
Payment Execution State & Accounting Consequence Matrix, negative-path tests
"Forged callback accepted; Out-of-order status regresses settled payment;
Duplicate callback creates duplicate accounting effect; Provider says settled
but statement evidence conflicts and is ignored" of §12, and the §3 authority
row ("BNK-07 | Canonical payment execution/finality status and evidence |
Payment subject mutation, ledger write, business approval").
First survey of this service against the spec (2026-10-22); A1 closure pass
(2026-10-26).

Citation check: every BNK-07 line cited in the review request was compared with
the docx text. The commands, queries, state model, permissions, SoD, events,
failure/degradation semantics, evidence and minimum negative-path lines match
the document exactly; no mismatch was found. The only difference is formatting:
the docx tables are flattened into pipe-delimited text in the extracted copy,
and the section number is not printed on the BNK-07 page itself (§7 is the
"Detailed Service Specifications" section that contains it).

Overall: the profile-driven INT-07 callback path is materially stronger than
the command surface suggests. It verifies a per-tenant vault secret, enforces a
provider profile and replay window, maps provider statuses under a versioned
profile, serialises callbacks on the payment row, writes an append-only receipt
for every authenticated callback, and refuses to regress a governed final
state. All five named queries exist. All nine named commands now exist:
`ConfirmSettlement` and `RecordRejection` are the first-class, evidence-bearing
manual finality commands added in this pass (A1). The remaining material gaps
are that two spec states (`SUBMITTED` and `PENDING_UNKNOWN`) are unreachable or
unrepresentable (A4), `PollPaymentStatus` is caller-attested rather than a
provider poll and emits no event (A2/A3), the legacy webhook is tenant-blind
and weaker than the profile path (A6/A7), callback finality evidence is still
thin (A9), and the older manual commands (`ResolveStatusConflict`,
`RecordReturn`, `CancelPaymentWhereSupported`) still rely on assertion rather
than evidence (A10/A11). `RecordPaymentStatus` idempotency (A5) and A1 are
closed in this pass. Roughly 70% of BNK-07.

## A. Not implemented / weaker than the spec

- **A1. `ConfirmSettlement` and `RecordRejection` were not implemented — closed
  in this pass.** The §7 command line names `RecordPaymentStatus;
  ProcessProviderCallback; PollPaymentStatus; LinkStatementConfirmation;
  ConfirmSettlement; RecordReturn; RecordRejection; CancelPaymentWhereSupported;
  ResolveStatusConflict`. Both missing commands now exist as first-class,
  evidence-bearing paths:

  * `POST /bnk07/payments/{paymentID}/confirm-settlement` and
    `POST /bnk07/payments/{paymentID}/record-rejection`. BNK-07 §7 names the
    commands and their shared contract fields (provider/bank refs, confirmation/
    reject evidence, effective timestamps, finality basis) but does not
    prescribe a per-command JSON schema. The request shape is therefore
    `provider_reference`, `bank_reference`, `reason`, `effective_date` (RFC3339
    or `YYYY-MM-DD`), `evidence_reference` and optional `evidence_hash`
    (SHA-256 hex); at least one provider/bank reference, a reason, an effective
    date and an evidence reference are required. The response is
    `{payment, evidence, replayed}`.
  * Both commands require `Idempotency-Key`. The key is claimed in the same
    transaction as the status change, the append-only
    `payment_finality_evidence` row, the `status_events` row and the
    `PaymentSettled`/`PaymentRejected` outbox event (migration `000008`). A
    retry with the same key and request returns the committed result with
    `Idempotent-Replay: true`; the same key for a different command, payment or
    request is a 409 conflict; a refused command never consumes its key. This
    is the platform command-idempotency pattern, not the natural-key dedup the
    older BNK-07 commands use, because a manual finality decision has no
    provider event reference to dedupe on.
  * Both commands require `PAYMENT_FINALITY_CONFIRM`, the same action
    `PollPaymentStatus` uses for a final status, and enforce creator-different
    SoD in the handler and again in the store: the principal who created the
    payment cannot manually assert its final settlement/rejection. An open
    evidence conflict is refused (409); a payment already in a governed-final
    state is refused (409) unless the request is the exact idempotent replay.
  * `GetFinalityEvidence` now returns the stored manual finality evidence rows
    in addition to the state-level source/version. The four BNK-07 actions are
    now registered in access-control-svc migration
    `000020_register_payment_status_actions` (`PAYMENT_FINALITY_CONFIRM` as
    HIGH risk; the other three STANDARD), closing the pre-existing grantability
    gap that would otherwise make these commands permanently 403 in a real
    environment. Deploy access-control `000020` before enabling the new
    payment-status commands; the action cannot be granted until it exists in the
    registry.
  * Tests: `TestPgStore_ConfirmSettlement_EvidenceAndOutbox`,
    `TestPgStore_ConfirmSettlement_DuplicateSameKeyReplaysWithoutSecondEffect`,
    `TestPgStore_ConfirmSettlement_AlreadyTerminalWithNewKeyRefused`,
    `TestPgStore_ConfirmSettlement_SameKeyDifferentRequestConflict`,
    `TestPgStore_ConfirmSettlement_SameCreatorForbidden`,
    `TestPgStore_ConfirmSettlement_OpenConflictRefused`,
    `TestPgStore_ConfirmSettlement_ConcurrentSameKeyOneEffect`,
    `TestPgStore_RecordRejection_EvidenceAndOutbox`,
    `TestPgStore_RecordRejection_MissingEvidenceDoesNotConsumeKey`,
    `TestPgStore_FinalityEvidence_TenantIsolation`, and the migration down-guard
    test; plus the handler success/duplicate/terminal/missing-evidence/SoD/
    permission tests in `internal/handler/finality_handler_test.go`. The
    access-control integration suite also proves the four actions are
    registered and that 000020's down migration refuses while a bundle names
    one.
  * A4 is not a precursor for A1: both commands accept any non-final state
    (`CanConfirmFinality` is `!IsFinal`), so a future `PENDING_UNKNOWN` would be
    accepted without changing them. A4 remains a separate state-model item
    (below).
- **A2. `PollPaymentStatus` is caller-attested, not a provider poll, and has no
  polling policy.** The handler accepts `reported_status` and an optional
  `mapping_version` in the request body and applies them through the same
  store method as a webhook. There is no outbound provider client, no provider
  status query, no scheduled poll, no backoff, no retry budget, no lag/freshness
  measurement and no authenticated raw response. The §7 purpose names "polls"
  as an authoritative input, and the NFR line promises "near-real-time plus
  eventual statement confirmation" and a "provider-lag/finality-freshness SLI";
  this endpoint provides none of those. It is honest about being manual (the
  package doc and handler comment say so), and it now requires
  `payment.finality.confirm` plus the creator-different rule for SETTLED/
  REJECTED, but a permission gate does not turn an assertion into a poll.
  *Why it was not fixed here:* there is no real bank/PSP integration in this
  repository, and a real poll needs an adapter, provider authentication,
  backoff, durable polling state and a decision on how a stale poll interacts
  with a newer callback.
- **A3. `PollPaymentStatus` changes canonical state without emitting a domain
  event, and `ResolveStatusConflict` emits only the conflict-resolved event.**
  `ApplyCallbackStatus(..., emitEvent=false)` deliberately preserves the old
  poll behavior, so a poll that moves a payment to ACCEPTED/PENDING/SETTLED/
  REJECTED writes only the internal `status_events` row, never the outbox.
  `ResolveStatusConflict` writes `PAYMENT_STATUS_CONFLICT_RESOLVED`
  (`...conflict-resolved`), not the `PaymentSettled`/`PaymentRejected`/
  `PaymentCancelled` event for the final status it sets. Accounting, AP/AR and
  reconciliation consumers that react to the §7 event list therefore do not
  learn about a manual settlement or rejection unless they also poll BNK-07.
  That is weaker than §4 ("Every externally material command links ...") and
  the §7 events line. *Why it was not fixed here:* deciding whether a manual
  path emits one final event, both a conflict-resolved and a final event, or a
  distinct manual-override event is a downstream contract decision; emitting
  from the poll would also change the behavior this service has had since the
  dual-write. The new A1 commands do emit `PaymentSettled`/`PaymentRejected`
  directly through the outbox, so the first-class manual command path is no
  longer silent; the compatibility poll and `ResolveStatusConflict` still are.
- **A4. Two spec states are unreachable or unrepresentable: `SUBMITTED` and
  `PENDING_UNKNOWN` — still open; A1 does not depend on it.** The §7 state
  model is "Prepared → Submitted → Accepted/Pending → Settled or
  Rejected/Returned/Cancelled; PendingUnknown may occur". `StatusSubmitted`
  exists in the domain, the table CHECK and
  `CanCancel`, but no command or callback can set it: `RecordPaymentStatus`
  hard-codes `PREPARED`, `ValidCallbackStatus` excludes `SUBMITTED`, and there
  is no BNK-06 event consumer or explicit "mark submitted" transition. A
  normal payment therefore jumps from `PREPARED` straight to
  `ACCEPTED`/`PENDING`/`SETTLED`/`REJECTED`; `SUBMITTED` is observable only by
  writing the database directly. `PENDING_UNKNOWN` is absent from the domain,
  the CHECK constraint and `ValidCallbackStatus`, so an ambiguous outcome can
  only be recorded as `PENDING` or left at `PREPARED`. That contradicts §8
  ("Pending / Unknown | Outcome cannot be determined from available evidence |
  Freeze duplicate execution; no invented failure/success; reconcile original
  attempt") and invariant 15 ("A provider timeout or missing response is
  Pending/Unknown, never automatically Failed"). *Why it was not fixed here:* a
  new state changes the CHECK constraint, the terminal/regression rules,
  `CanCancel`, `ListUnresolvedPayments`, the provider status profile and every
  consumer's status mapping; it needs its own migration and state-machine
  review, not an improvised enum value. A1 did not close A4: the new finality
  commands operate from any non-final state, so they do not need
  `PENDING_UNKNOWN` to exist first, and adding that state without a producer
  for `SUBMITTED` would leave the state model only partly closed. Closing A4
  still needs a BNK-06 event consumer or explicit "mark submitted" transition
  for `SUBMITTED`, a `PENDING_UNKNOWN` transition/consumer for ambiguous
  provider outcomes, and the corresponding terminal/regression, cancellation
  and consumer-mapping review.
- **A5. `RecordPaymentStatus` was not idempotent — closed in this pass.** Every
  call previously generated a new UUID and inserted a new
  `payment_execution_states` row. A lost `201` followed by a retry therefore
  created a second execution state for the same BNK-06 attempt, each able to
  receive its own callbacks and emit its own `PaymentSettled` — exactly the
  duplicate-accounting effect negative-path #3 is meant to prevent. The
  natural key is now `(tenant_id, source_reference)`, **not**
  `provider_request_id` (which the contract allows to be empty):
  `source_reference` is the same field and concept the BNK-06 idempotent-dedup
  fix uses (`uq_payment_initiation_attempts_unresolved_source_reference`,
  migration `000007_unresolved_source_reference`). Migration
  `000007_unresolved_source_reference` adds
  `uq_payment_execution_states_unresolved_source_reference` on
  `(COALESCE(tenant_id::text, ''), source_reference)` for the not-yet-resolved
  statuses `PREPARED`, `SUBMITTED`, `ACCEPTED` and `PENDING`; an empty
  `source_reference` is excluded. `RecordPaymentStatus` now looks up an
  existing unresolved row before inserting and returns it unchanged on a
  sequential retry, and maps a specific unique-constraint loss under a
  concurrent race back to the winning row — the same pattern
  `PrepareAttempt` uses. A duplicate retry creates no second status event or
  outbox event. Test:
  `TestPgStore_RecordPaymentStatus_ConcurrentSameSourceReference_OneRow`
  (plus the sequential-retry, empty-reference and resolved-reuse tests in the
  same file). See B11 for the key and status-set decision.
- **A6. The legacy webhook is tenant-blind and materially weaker than the
  profile-driven endpoint.** `POST /bnk07/webhooks/provider-callback` is
  registered outside the tenant middleware, so its context has an empty tenant
  and the RLS policy (`tenant_id IS NULL OR tenant_id = app.tenant_id`) lets it
  see only NULL-tenant rows. Rows created through the normal
  `X-Tenant-Id` path are invisible to it; conversely, any tenant's signed
  callback can drive a NULL-tenant row. It verifies one deployment-wide
  `WEBHOOK_SHARED_SECRET` over the raw body and has no per-tenant/per-provider
  secret, no timestamp or replay window, no provider key (the payload has no
  provider field), no certified status map or mapping version, no callback
  receipt, and no sequence/event-time ordering. This pass added open-conflict
  blocking (409) and the existing trigger still blocks governed-final
  regressions, but the endpoint is not equivalent to the profile path. The
  profile endpoint
  `POST /bnk07/webhooks/provider-callback/{provider}/{tenantID}` is the strong
  path: per-tenant vault secret, provider binding, replay window, certified
  status map, receipts, row lock, rank and event-time checks. *Why it was not
  removed here:* it is a compatibility surface, and removing a public route is
  a deployment/contract decision. It is now fail-closed unless a secret is
  explicitly configured (A15).
- **A7. Intermediate out-of-order statuses can regress through the legacy and
  poll paths.** `ApplyCallbackStatus` has no `statusRank` and no
  `last_provider_event_at` check. It relies on the database trigger, which only
  protects SETTLED/REJECTED/CANCELLED/RETURNED. A `PENDING` payment can
  therefore be moved back to `ACCEPTED` by an older callback, or by a poll
  whose body says ACCEPTED; the older `mapping_version` overwrites the newer
  one. The profile callback path has `statusRank`, an event-time comparison and
  a `last_provider_event_at` column, so the two paths do not implement the same
  ordering rule. §7 says "Out-of-order callbacks cannot regress final state"
  and invariant 17 says BNK-07 "handles out-of-order statuses without
  regressing governed finality"; governed finality is guarded, but intermediate
  freshness and the "sequence/order decision" evidence are not. *Why it was
  not fixed here:* the callback path can compare event times, but the poll body
  has no event time, and rejecting a manual correction needs a policy decision
  rather than a silent rank check.
- **A8. `ListUnresolvedPayments` is conflict-only and unpaginated.** The store
  query is `WHERE has_open_conflict = TRUE AND legal_entity_id = $1::uuid
  ORDER BY updated_at ASC`. It returns only evidence conflicts; it excludes
  `PREPARED`, `SUBMITTED`, `ACCEPTED`, `PENDING` and (if it existed)
  `PENDING_UNKNOWN` payments that also need resolution or monitoring. The §7
  query name is broader than "open conflict", and the operational need is
  broader still (stuck in-flight, unknown, stale, unmatched). It also has no
  `limit` or cursor, so a large exception population produces an unbounded
  response. This pass added the entity filter and the partial index
  `idx_payment_execution_states_open_conflict (legal_entity_id, updated_at)
  WHERE has_open_conflict`, so the narrow query is efficient; the definition
  and pagination are still missing. *Why it was not fixed here:* "unresolved"
  is a product/control definition (which statuses, age thresholds, conflict vs
  unknown vs stale) and pagination changes the response shape.
- **A9. Finality evidence is much thinner than the contract — partly improved
  for manual commands, still open for callbacks.** §7 requires
  "Authenticated raw response/reference, mapping/version, sequence/order
  decision, provider/bank refs, finality source, timestamps and conflict
  resolution evidence". `GetFinalityEvidence` now returns the append-only
  manual finality evidence rows added by migration `000008` (provider/bank
  refs, reason, effective timestamp, evidence reference/hash, mapping version,
  finality source, actor and idempotency key) in addition to `payment_id`,
  `status`, `finality_source` and `mapping_version`; `GetProviderReference`
  still returns only `provider_request_id`. The `provider_callback_receipts`
  table does store `body_hash`, `scheme`, `event_time`, `outcome`, `received_at`
  and the provider/event refs, but no read endpoint joins a receipt to the
  payment's finality evidence, and `status_events` has no raw hash or external
  reference column. A conflict's reason is written into `status_events.detail`
  and then cleared from `conflict_reason`; it is not returned as resolution
  evidence either. *Why it was not fixed here:* an evidence view needs schema,
  retention and masking decisions (invariant 2 covers bank identifiers) and a
  contract for which raw response may be retained.
- **A10. The older manual commands still require an assertion, not evidence;
  the new settlement/rejection commands close that part but not the rest.**
  `ConfirmSettlement` and `RecordRejection` now require provider/bank refs,
  evidence, an effective date, a reason, `PAYMENT_FINALITY_CONFIRM` and a
  creator-different principal, so the first-class settlement/rejection path is
  no longer assertion-only. `ResolveStatusConflict` still validates that the
  target is SETTLED/REJECTED/CANCELLED and locks the row, but it accepts a
  free-text `reason` only: no statement or provider evidence reference, no
  second approver, and no check that the chosen final status is supported by
  either side's evidence. `RecordReturn` checks SETTLED, requires a reason and a
  different principal, but does not verify that a return actually happened; a
  caller can move SETTLED to RETURNED on their word.
  `CancelPaymentWhereSupported` moves PREPARED/SUBMITTED to CANCELLED without
  asking the provider whether cancellation is supported or succeeded, and emits
  `PaymentCancelled` regardless. §7 says "manual finality override requires
  exceptional controlled workflow and evidence"; permission separation plus a
  non-creator check is necessary but not sufficient for that sentence. *Why it
  was not fixed here:* it needs evidence/workflow contracts (and, for
  cancellation, a provider capability profile and a cancel-requested state)
  rather than a local patch.
- **A11. Conflicts cannot be recorded or resolved on terminal states because
  the immutability trigger rejects every update.** `LinkStatement` updates
  `has_open_conflict`/`conflict_reason` in place. The trigger rejects any
  update when `OLD.status` is REJECTED, CANCELLED or RETURNED, so late
  statement evidence that disagrees with a rejection or cancellation cannot be
  recorded at all (the store returns a generic 503). For SETTLED the trigger
  allows an update only when `NEW.status` is SETTLED or RETURNED, so a conflict
  can be recorded but `ResolveStatusConflict` can only clear it by choosing
  SETTLED; choosing REJECTED/CANCELLED is correctly blocked as a regression,
  but there is no "keep the current status and close the exception" path.
  §7 requires conflicting evidence to "raise an exception rather than silent
  overwrite" and invariant 19 requires an explicit exception/reconciliation
  path; that path is unavailable for the late-evidence cases above. *Why it
  was not fixed here:* the trigger must distinguish conflict-metadata updates
  from status regressions, which is an immutability-policy change with its own
  security and state-machine tests.
- **A12. Read endpoints had no authorization — closed in this pass.**
  `GetPaymentStatus`, `GetStatusHistory`, `GetFinalityEvidence` and
  `GetProviderReference` previously returned any payment visible under RLS to
  anyone who knew its id; they did not require `X-Principal-Id` or check
  `payment.status.read`. All four now require a principal and authorize
  `PAYMENT_STATUS_READ` against the payment's own legal entity.
  `ListUnresolvedPayments` and `ListCallbackReceipts` now do the same against
  the requested entity, and both validate UUID filters. This required
  forwarding the acting principal from BNK-07's callers: `payment-run-svc` and
  `payable-open-item-svc` were updated, while `payroll-run-svc` and
  `accounts-receivable-svc` already forwarded one. Tests:
  `TestReadEndpointsRequireAPrincipal`,
  `TestReadEndpointsRequirePaymentStatusRead`,
  `TestListUnresolvedPaymentsIsEntityScopedAndAuthorized`,
  `TestListCallbackReceiptsValidatesUUIDFilters` and
  `TestGetStatusForwardsTenantAndPrincipal`.
- **A13. Conflict resolution accepted any status and had a check-then-update
  race — closed in this pass.** `ResolveStatusConflict` now rejects anything
  other than SETTLED/REJECTED/CANCELLED with 400 (`ValidResolutionStatus`),
  returns 409 when there is no open conflict, locks the row with
  `SELECT ... FOR UPDATE`, and updates with `has_open_conflict = TRUE` in the
  same statement. `LinkStatement` also locks the row before comparing statuses,
  so a callback cannot race the comparison. Tests:
  `TestResolveStatusConflictRejectsANonFinalStatus` and the existing
  conflict/SoD tests.
- **A14. A callback could overwrite an open conflict or come from a different
  provider — closed in this pass.** The profile path now returns
  `CONFLICT_BLOCKED`/`PROVIDER_MISMATCH` receipts and HTTP 202, changes
  nothing, and does not consume the event's one-decision slot; the receipt
  uniqueness predicate excludes both outcomes. `provider_key` is bound at
  record time or by the first applied callback, and a later callback from a
  different provider is refused. The legacy webhook and `PollPaymentStatus`
  now return 409 `ErrOpenConflictExists` instead of scanning and returning the
  conflicted state. Migration
  `000006_callback_outcomes_and_unresolved_index` adds the receipt outcomes
  and the unresolved index. Tests:
  `TestCallback_OpenConflictBlocksAndDoesNotDecideTheEvent`,
  `TestCallback_ProviderMismatchIsRefused`,
  `TestLegacyCallbackAndPollCannotDecideAnOpenConflict`,
  `TestProcessProviderCallback_OpenConflictIsRefused` and
  `TestPollPaymentStatus_OpenConflictIsRefused`.
- **A15. The legacy webhook trusted a known default secret — closed in this
  pass.** `WEBHOOK_SHARED_SECRET` no longer has a development default. Empty
  means the legacy receiver fails closed with 503 before reading the body; the
  compose file now passes `${WEBHOOK_SHARED_SECRET:-}`. The profile endpoint is
  unaffected because it fetches a per-tenant secret from the vault. Test:
  `TestProcessProviderCallbackWithoutAConfiguredSecretFailsClosed`.
- **A16. A poll could assert finality with only ingest permission — closed in
  this pass.** A poll reporting SETTLED/REJECTED now requires
  `PAYMENT_FINALITY_CONFIRM` (intermediate ACCEPTED/PENDING stays on
  `PAYMENT_STATUS_INGEST`), and the creator cannot assert a final status on
  their own payment. Test:
  `TestPollPaymentStatus_UsesFinalityPermissionForFinalStatus` plus the
  existing SoD tests.
- **A17. Index-defeating casts and an unindexed unresolved query — closed in
  this pass.** `ListReceipts` now compares UUID columns with `$n::uuid`
  instead of casting the column to text; `ApplyProviderCallback` parses the
  provider reference as a UUID before comparing `payment_id`; and
  `ListUnresolved` has the partial index above. No N+1 was found in the list
  or history paths: `ListUnresolved`, `ListEvents` and `ListReceipts` are each
  one query, and the profile callback path is one vault secret fetch followed
  by one store transaction, not a per-payment external call. `GetStatusHistory`
  is also a single indexed query, but it has no limit/cursor either; a payment
  with a long conflict/replay history returns every row.

## B. Interpretations and additions (decisions a reviewer may want to reverse)

- **B1. `RecordPaymentStatus` is a projection/ingest endpoint, not the BNK-06
  submission command.** BNK-06 owns the durable attempt and the external call;
  BNK-07 owns canonical execution/finality state. The endpoint creates a
  `PREPARED` row and lets callbacks or polls move it. That is why
  `RecordPaymentStatus` does not perform an external side effect (and why its
  missing idempotency, A5, is still serious but not the same failure as a
  duplicated bank call). The unreachable `SUBMITTED` transition in A4 is the
  other side of this boundary: a BNK-06 `PaymentSubmitted` projection would
  normally set it.
- **B2. The INT-07 profile callback is the supported production path; the
  legacy shared-secret webhook is a compatibility path.** All examples and
  receipts in this register refer to the profile endpoint unless the legacy
  route is named explicitly. Operationally, the legacy route should stay
  unconfigured (A15) and be removed once no caller depends on it.
- **B3. `CONFLICT_BLOCKED` and `PROVIDER_MISMATCH` are deliberately "not
  decided" and return 202.** A provider that gets a non-2xx retries; retrying a
  blocked event must be able to apply once the conflict is resolved or the
  correct provider calls. Treating these as 4xx and consuming the event would
  lose the provider's evidence or force a manual replay. The receipt unique
  index therefore excludes both outcomes (A14). A reviewer could prefer 409
  with a retry contract instead; 202 was chosen to match the provider-facing
  retry semantics already used for unknown/ambiguous payments.
- **B4. A conflict may only be resolved to a governed final status.**
  `ValidResolutionStatus` accepts SETTLED/REJECTED/CANCELLED. RETURNED is
  excluded because it has its own command and its own SETTLED precondition;
  intermediate statuses are excluded because "resolving" an exception to
  PENDING would leave the exception open in substance. This is a deliberate
  narrowing of the request contract, not a spec permission.
- **B5. `ListUnresolvedPayments` requires `legal_entity_id`.** An unscoped list
  would cross entities and, for a principal entitled in several, leak rows they
  are not entitled to see. The entity scope plus `PAYMENT_STATUS_READ` is the
  authorization boundary; A8 records that the result set is still too narrow
  (conflicts only) and unpaginated.
- **B6. Tenant scoping is optional, and NULL-tenant rows are globally
  visible.** `TenantContext` only sets a tenant when `X-Tenant-Id` is present;
  `RecordPaymentStatus` can therefore insert a NULL-tenant row, and the RLS
  policies on `payment_execution_states` and `status_events` explicitly allow
  `tenant_id IS NULL` for every tenant. This is a compatibility choice that
  makes the legacy webhook work (A6), but it is also a cross-tenant visibility
  risk: a caller that omits `X-Tenant-Id` creates a row every tenant can read,
  so entity-level authorization becomes the only remaining boundary for that
  row. The directional fix is to require a tenant on the
  authenticated routes and reserve NULL-tenant rows for a migration window;
  that is a security-policy change, so it is recorded rather than silently
  changed here.
- **B7. `provider_key` is optional at record time and bound by the first
  applied profile callback.** BNK-06 callers are not forced to know the
  provider key, and a payment created before the provider is known can still
  be updated. Once bound, a callback from a different provider is refused;
  before binding, there is nothing to compare. The legacy webhook cannot bind
  or enforce a provider key because its payload has no provider field (A6).
- **B8. Reads fetch the payment before authorizing it.** The authorization
  check is scoped to the payment's legal entity, so the row must be read first.
  A caller therefore gets 404 for an unknown id and 403 for an existing id it
  cannot read. That existence signal is limited to ids already visible under
  RLS and matches the shape BNK-06's read endpoints use.
- **B9. `LinkStatementConfirmation` trusts the caller's statement status and
  reference.** It does not fetch the BNK-03 statement or BNK-05 reconciliation
  result; it compares the supplied `reported_status` with the current canonical
  status and raises a conflict on disagreement. That is enough to enforce the
  negative path ("provider says settled but statement evidence conflicts and is
  ignored"), but it is not independent evidence. A10 records the related
  evidence gap for manual resolution.
- **B10. Verification.** `go build ./...`, `go vet ./...` (including
  `go vet -tags=integration ./...`) and `go test ./...` pass. The full
  integration suite (`go test -tags=integration -count=1
  -timeout=900s ./...`) was run against a fresh, disposable embedded Postgres
  16 instance with a pid-derived runtime path; no shared dev database was used.
  The access-control-svc integration store suite was run the same way and
  passes, including the 000020 registration and down-guard test. A1 coverage:
  command success/evidence/outbox, duplicate replay with no second effect,
  terminal-state refusal with a new key, key/request conflict, creator-different
  SoD, open-conflict refusal, concurrent same-key one-effect, rejection evidence
  and missing-evidence key preservation, tenant isolation of the new evidence,
  and all three 000008 down guards (evidence, idempotency claim, manual-finality
  state). Previous coverage still passes: read authorization and UUID-filter
  validation (handler), legacy/poll conflict blocking (handler and store),
  profile conflict/provider blocking and non-decision (store), entity-scoped
  unresolved listing (store), invalid resolution status (handler/store),
  empty-secret fail-closed (handler), poll finality permission/SoD (handler),
  and concurrent/sequential `source_reference` idempotency (store). The
  existing forged-callback, duplicate, out-of-order, statement-conflict, return
  and cancellation tests still pass. `payment-run-svc` and
  `payable-open-item-svc` were updated for the principal-forwarding contract;
  their tests pass with the same change.
- **B11. `RecordPaymentStatus` deduplicates on `(tenant_id, source_reference)`
  only while the execution state is unresolved.** The key is BNK-06's
  `source_reference`, not `provider_request_id`: the latter is optional and
  may be empty, so it cannot identify a retry. The index covers `PREPARED`,
  `SUBMITTED`, `ACCEPTED` and `PENDING` — this service's pre-terminal states,
  i.e. the complement of `domain.finalStates`. SETTLED, REJECTED, CANCELLED
  and RETURNED are resolved, so the index deliberately releases the reference
  for a genuine later instruction, mirroring BNK-06's
  `uq_payment_initiation_attempts_unresolved_source_reference` (migration
  `000007`). The pre-insert lookup uses the same tenant/status predicate as the
  index; the index, not the lookup, is the concurrency guarantee. NULL tenant
  ids are coalesced to the empty string so legacy tenant-less rows still
  collide with each other, and an empty `source_reference` is excluded from
  both the lookup and the index. A reviewer could prefer deduplicating after a
  resolution too, but that would make an exhausted source_reference
  permanently unusable, which is why BNK-06's precedent scopes it to
  unresolved attempts.
- **B12. Manual finality commands use header idempotency and a service-owned
  finality basis.** BNK-07 §7 names `ConfirmSettlement`/`RecordRejection` and
  requires provider/bank references, evidence, effective timestamps and a
  finality basis, but it does not define a per-command JSON schema. The chosen
  request fields are the minimum that records that evidence. The
  `Idempotency-Key` is a header rather than a body field so it is handled
  uniformly by middleware and cannot be confused with payment data; it is
  scoped per tenant, and the stored request hash covers command, payment and
  every evidence field. A replay returns the current state plus the stored
  evidence, not a cached HTTP body. The finality source and mapping version are
  service-owned constants, not caller input, so a caller cannot label a manual
  command as a provider callback or statement link. Creator-different SoD is
  enforced in addition to `PAYMENT_FINALITY_CONFIRM`; the spec does not name a
  separate second-approver record, so none is invented here. If a later
  interpretation requires maker-checker, the right change is an approval
  record/state in front of this command, not weakening the existing checks.
  The provider/bank references are recorded as evidence but are not
  independently verified against BNK-06/BNK-03 in this pass (A10).
