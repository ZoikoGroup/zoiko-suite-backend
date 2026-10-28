# Spec deviations — payment-initiation-adapter-svc (BNK-06)

Governing document: `ZoikoSuite_Banking_Cash_Treasury_Detailed_Service_Specifications_v1_0.docx`,
§7 "BNK-06 — Payment Initiation Adapter", its statement of purpose
("Transmit an already approved payment instruction to a bank/PSP through a
durable, idempotent external attempt while preserving exact authorization
subject and provider evidence"), the §2 invariants 13–16, the §4 shared
contract (context, concurrency, idempotency, errors, evidence), the §10
provider-adapter rules, negative tests 21–25 of §12, and the §3 authority row
("BNK-06 | Durable approved payment submission attempt and provider receipt |
Business payment approval, external finality, duplicate retry inference").
First survey of this service against the spec (2026-10-22).

Citation check: every BNK-06/BNK-07 line cited in the review request was
compared with the docx text. The commands, queries, state model, permissions,
SoD, events, failure semantics, evidence and negative-path lines match the
document exactly; no mismatch was found. The only difference is formatting:
the docx tables are flattened into pipe-delimited text in the extracted copy,
and the section number is not printed on the BNK-06 page itself (§7 is the
"Detailed Service Specifications" section that contains it).

Overall: the command surface is complete. `InitiatePayment` is split into a
durable `PrepareAttempt` followed by `SubmitAttempt` (B1), and
`RetrySamePaymentAttempt`, `CancelBeforeSubmission`,
`ResolveAmbiguousSubmission` and `QuarantinePaymentAttempt` all exist. Four of
the five queries exist; `ListPaymentAttempts` does not (A1). The attempt row
is committed before any provider call, is immutable in its protected fields,
reuses one idempotency key for every retry, and a timeout becomes
`PENDING_UNKNOWN`, never `REJECTED`. The material gaps are that the
authorized subject is never bound to the request's payer/payee/amount/
currency/date (A2), the default send path has no durable in-flight CAS (A3),
and the stored evidence is far thinner than the contract (A4). Roughly 75% of
BNK-06.

The INT-07 provider engine (`PROVIDER_ADAPTERS_ENABLED=true`) is materially
stronger than the legacy stub path: it pins the certified profile, rail and
connector, makes the attempt `SUBMITTING` by compare-and-set before the call,
sweeps a stale in-flight attempt to `PENDING_UNKNOWN`, reconciles a
`PENDING_UNKNOWN` attempt before any resend, and records hash-only transport
evidence. With the engine disabled — the default — every attempt uses the
legacy path described in A3.

A later pass closed A6.

## A. Not implemented / weaker than the spec

- **A1. `ListPaymentAttempts` is not implemented.** The §7 query line names
  `GetPaymentAttempt; GetProviderReceipt; GetSubmissionState;
  GetAttemptEvidence; ListPaymentAttempts`. There is no route, store method or
  SQL for the list query; the only reads are by attempt id. Operators
  therefore cannot answer "which attempts are `PENDING_UNKNOWN`/`SUBMITTING`
  for this entity?" from this service, which is also the input BNK-07 and
  reconciliation need. Adding it is not a small change: it needs a
  tenant/entity-scoped filter, keyset pagination, a supporting index, and a
  decision on which sensitive fields (`payer_account_ref`, `payee_ref`) may
  appear in a list response under invariant 2. Documented, not added.
- **A2. The authorized subject is not bound to the attempt's protected
  fields.** This is the largest gap. `PrepareAttempt` re-fetches the upstream
  authorization and compares only the opaque `ProposalFingerprint` string
  (`clients/authorization.go`, `clients/treasury.go`). It never recomputes a
  fingerprint over the request, and it never compares the request's
  `payer_account_ref`, `payee_ref`, `amount`, `currency` or `execution_date`
  with the upstream authorization's own fields. A caller holding any live
  APPROVED/CONSUMED authorization fingerprint can therefore prepare an
  attempt for the same legal entity with a different payee or amount; the
  fingerprint still matches. `treasury.VerifyPayerAccount` checks that the
  payer account is ACTIVE and ownership-verified in the tenant, but not that
  it is the account named by the authorization, nor that it belongs to
  `legal_entity_id`.

  The database trigger only makes those fields immutable **after** the
  `PREPARED` row exists; it cannot detect that the first value was never the
  authorized one. That leaves invariant 13 ("BNK-06 accepts only an exact,
  still-valid authorized payment instruction; it cannot mutate
  payer/payee/amount/currency/date/purpose"), invariant 14 ("A durable
  payment-attempt record and idempotency identity exist before the external
  network side effect") and §12 test 21 ("Payment subject changed after
  authorization") only partly covered. The §4 concurrency rule is explicit:
  "Approval/finality/certification commands bind a protected-field
  fingerprint." *Why it was not fixed here:* the fingerprint algorithms and
  the authoritative field sets live in payment-authorization-svc,
  treasury-svc and payroll-run-svc. The fix is to have each verifier return
  the protected subject (or a canonical subject hash) and to compare it with
  the request before insert; that is a cross-service contract change, not a
  local patch. Until then, `PrepareAttempt` should not be treated as proof
  that the stored subject is the approved one.
- **A3. The legacy (default) send path has no durable in-flight state and no
  reconcile-before-resend.** `SubmitAttempt`/`RetrySameAttempt` read the
  attempt, check `PREPARED`/`PENDING_UNKNOWN`, and call
  `provideradapter.Client.Submit`; only after the call returns does
  `MarkSubmitted`/`MarkPendingUnknown`/`MarkRejected` write the outcome. Two
  concurrent submits can both read `PREPARED` and both reach the provider.
  A process crash after the provider accepted the payment but before
  `MarkSubmitted` leaves the row `PREPARED`, with no evidence that a call was
  in flight; the next caller may send again. A retry from `PENDING_UNKNOWN`
  resends immediately, without first asking the provider what it already has.
  The unique idempotency key is passed to the provider, so a correct provider
  can suppress the duplicate, but the service itself provides no fail-safe
  duplicate protection and no durable "the request is leaving" fact. That is
  weaker than invariant 14, the §10 failure rule ("Provider outage degrades
  safely to pending/queued/blocked mode depending side-effect risk; never
  duplicate irreversible payments"), §12 tests 23 ("External network call
  occurs before durable attempt commit") and 24 ("Provider timeout creates a
  new payment instead of resolving original"), and the NFR line "durable
  attempt before network call; strict idempotency; high availability with
  fail-safe duplicate protection."

  The INT-07 engine path does this correctly (`BeginSend` compare-and-set to
  `SUBMITTING`, detached outcome recording, `reconcile` before resend,
  stale-in-flight sweeper). The gap is that the engine is off by default and
  the legacy `provideradapter.Client` boundary is the documented place to
  plug in a real bank/PSP. *Why it was not fixed here:* making the legacy
  path safe means either routing every attempt through the engine (which
  needs a provider profile, connector and vault for every deployment) or
  adding the same `SUBMITTING` CAS, detached-write and query-first retry to
  the legacy store methods. The latter is a real state-machine change with
  its own crash tests; it should not be bolted on as part of a survey.
- **A4. Evidence is thinner than the contract.** The §7 evidence line
  requires "Authorization subject hash, attempt ID/idempotency key, request
  message/version/hash, transport/security evidence, provider ID, raw
  response reference and timestamps", and §4 says every externally material
  command links "request subject, approval/authorization fingerprint,
  attempt/result evidence, source hash/reference, actor/workload, versions
  and timestamps". `GetAttemptEvidence` returns only the authorization
  fingerprint, the idempotency key and the internal `attempt_events` list.
  `GetProviderReceipt` returns only `provider_request_id` and
  `provider_response_ref`. The legacy row stores no request message, message
  family/version, request hash, transport/security evidence or raw response
  (only the opaque ref). The engine path adds the pinned provider context
  columns on the attempt and hash-only `provider_transport_log` rows with
  timestamps, but those are not part of `GetAttemptEvidence`, and
  `GetProviderReceipt` still does not read them. Adding a normalized evidence
  view is a contract design decision
  (which hashes, which message version, how much raw response may be
  retained under ZS-SEC-001), so it is documented rather than improvised.
- **A5. `ResolveAmbiguousSubmission` can assert an outcome without provider
  evidence, and emits no domain event.** The command accepts
  `resolved_status` (`SUBMITTED` or `REJECTED_BEFORE_SUBMISSION`) plus a free
  text note, and writes it directly. There is no requirement to attach the
  result of `QueryProvider`/BNK-07 evidence, no verifier that the provider
  actually has the payment, and no second actor. The §7 evidence line and
  §12 tests 24–25 expect the original attempt to be resolved from
  authoritative evidence, not from an operator's assertion; the §7 event list
  also expects `PaymentSubmitted`/`PaymentSubmissionRejected` when the
  outcome becomes known. Today the store writes only the internal
  `PAYMENT_SUBMISSION_AMBIGUITY_RESOLVED` event, not an outbox event, so
  downstream consumers never learn that the ambiguous attempt was resolved.
  The engine's `QueryProvider` endpoint gives an operator real evidence, but
  the resolve endpoint does not require it. *Not fixed here:* requiring an
  evidence reference and choosing which domain event to emit changes the
  command's request contract and downstream event flow; it needs a policy
  decision (and, for a manual finality assertion, the BNK-07 exceptional
  workflow).
- **A6. Money is `float64` in the API and the legacy path can persist a
  rounded amount.** — Closed (B11). `PrepareAttemptRequest.Amount` and
  `PaymentInitiationAttempt.Amount` were `float64`; the column is
  `NUMERIC(18,2)` with `CHECK (amount > 0)`. The provider-bound path called
  `exactAmount`, which checked the float's printed form for more than two
  decimal places after the JSON number had already been decoded into a
  float. The legacy path only checked `Amount > 0`, so `10.005` was sent to
  the adapter as a float and stored as `10.01`: the stored amount and the
  sent amount differed. That is a change to the payment subject (invariant
  13) and a C0 money-precision defect (ZS-DATA-001 D06). Both fields are now
  `domain.Decimal`, decimal text backed by `big.Rat`, the pattern
  intercompany-accounting-svc uses. It is read from a JSON number or a
  quoted string and is never held in a float. `PrepareAttempt` refuses with
  400, on both paths, an amount that is not positive, has more than two
  decimal places or more than 16 integer digits, and the store refuses the
  same before the insert. A valid amount is then held in the column's own
  form (`10.50`). The row is written from that text and read back as
  `amount::text`; the provider check, the legacy adapter's `SubmitRequest`
  and the engine's pinned payload hash all use the same digits.
  `exactAmount` is removed. On the wire `Amount` is still a JSON number, so
  no caller needed changing. payment-run-svc, payroll-run-svc, treasury-svc
  and accounts-receivable-svc all send a `float64`, and a test sends that
  exact shape and checks it is stored and sent unchanged.
  *Still open:* those callers still hold the amount as a `float64` before
  they send it, so a value they have already rounded arrives rounded. This
  service now refuses what it cannot hold, but cannot recover digits a
  caller lost.
- **A7. A replay re-runs all external verification before returning the
  original attempt.** `PrepareAttempt` performs the authorization
  fingerprint check, the treasury payer-account lookup and the provider
  profile check **before** `store.PrepareAttempt`, which is where a duplicate
  idempotency key is detected and the existing row is returned. A legitimate
  retry of an already-created attempt therefore repeats those calls, and if
  treasury-svc, payment-authorization-svc or payroll-run-svc is down, the
  retry returns 503 instead of the stable original result. §4 requires
  "Duplicate calls return the original stable attempt/result rather than
  create a second effect." The lookup itself is indexed
  (`uq_payment_initiation_attempts_idempotency_key`), so the cost is the
  external calls, not the query. *Not fixed here:* an early short-circuit
  must authorize the caller against the existing attempt's legal entity and
  decide what to do when the body differs from the original; doing that
  correctly is a behavior change worth its own review.
- **A8. Read endpoints had no authorization — closed in this pass.**
  `GetPaymentAttempt`, `GetProviderReceipt`, `GetSubmissionState` and
  `GetAttemptEvidence` previously returned any attempt visible under RLS to
  anyone who knew its id; they did not require `X-Principal-Id` or check
  `payment.attempt.read`. All four now go through `fetchReadableAttempt`,
  which requires a principal and authorizes `PAYMENT_ATTEMPT_READ` against
  the attempt's own legal entity before returning data. Handler test
  `TestReadEndpoints_RequirePrincipalAndReadPermission` proves 401 without a
  principal and 403 when the action is denied for all four routes.
- **A9. The source-reference invariant was racy — closed in this pass.**
  `PrepareAttempt` checked for an existing `PREPARED/SUBMITTING/
  PENDING_UNKNOWN/SUBMITTED` row with the same `source_reference` and then
  inserted, but the check and insert were not atomic: two concurrent callers
  with different idempotency keys could both pass the check and create two
  live payments for one instruction. Migration
  `000007_unresolved_source_reference` adds the partial unique index
  `uq_payment_initiation_attempts_unresolved_source_reference` on
  `(COALESCE(tenant_id::text, ''), source_reference)` for exactly those
  statuses (and `source_reference <> ''`); the losing insert is mapped back
  to the winner and returns the existing attempt with the same 200 answer a
  sequential retry gets. Integration test
  `TestPgStore_PrepareAttempt_ConcurrentSameSourceReference_OneWins` runs
  eight concurrent prepares and proves one winner and seven duplicate
  answers, all the same attempt id; the empty-reference case is covered by
  `TestPgStore_PrepareAttempt_EmptySourceReference_NeverDeduped`.
- **A10. The legacy path emitted no outbox events — closed in this pass.**
  Only the INT-07 `FinishSend` wrote the outbox; `MarkSubmitted`,
  `MarkPendingUnknown`, `MarkRejected` and `QuarantineAttempt` wrote just the
  internal `attempt_events` row. A submission through the default path was
  therefore invisible to every downstream consumer, despite the §7 event
  list. Each of those four commands now enqueues the corresponding
  `PaymentSubmitted` / `PaymentSubmissionAmbiguous` /
  `PaymentSubmissionRejected` / `PaymentAttemptQuarantined` event in the
  same transaction as the state change, carrying the caller's
  `X-Correlation-ID`. Integration test
  `TestPgStore_LegacyStub_OutcomeEventsWithCorrelation` proves one event per
  outcome and the correlation id in the payload.

## B. Interpretations and additions (decisions a reviewer may want to reverse)

- **B1. `InitiatePayment` is two REST calls, not one.** `POST
  /bnk06/attempts/` (`PrepareAttempt`) creates the durable `PREPARED` row;
  `POST /bnk06/attempts/{id}/submit` (`SubmitAttempt`) performs the provider
  call. This is the only way to satisfy "durable attempt before network
  call" without a distributed transaction, and the domain comment says so.
  A caller that wants one logical command calls both; a retry of the first
  returns the same row. Routes are REST under `/bnk06/...`, and action names
  are SCREAMING_SNAKE_CASE (`PAYMENT_INITIATE`, `PAYMENT_ATTEMPT_READ`,
  `PAYMENT_ATTEMPT_RESOLVE`, `PAYMENT_CANCEL_PRESUBMIT`), following the
  platform convention rather than the spec's lower-case dotted names.
- **B2. The implemented state set is a superset of the spec's.** The spec's
  BNK-06 line is "Prepared → Submitted / PendingUnknown /
  RejectedBeforeSubmission / Cancelled". The code adds `SUBMITTING` (the
  INT-07 durable in-flight state, never terminal, swept to `PENDING_UNKNOWN`
  if stale) and `QUARANTINED` (the state produced by the spec's own
  `QuarantinePaymentAttempt` command and `PaymentAttemptQuarantined` event).
  Both are justified by the same contract; neither is an extra operator
  choice.
- **B3. Quarantine uses `PAYMENT_ATTEMPT_RESOLVE`.** The spec's permission
  line has no separate quarantine permission, and quarantine is an
  operator-exception action like ambiguous resolution. Read, initiate, retry
  and cancel map to the four named permissions; retry reuses
  `PAYMENT_INITIATE` because it is another send.
- **B4. Idempotency keys are unique platform-wide, not per tenant.** The
  original index is `UNIQUE (idempotency_key)`. A caller in tenant B that
  presents a key already used in tenant A cannot see A's row under RLS, so
  the duplicate was previously reported as a 503. It is now a 409 ("already
  in use in another tenant"): the alternative — returning the other
  tenant's attempt — would leak it, and silently creating a second row
  would defeat the key. The trade-off is that the 409 reveals that some
  other tenant used the key. A per-tenant unique index would remove both the
  leak and the collision, but changes the existing identity guarantee and
  needs a migration decision.
- **B5. `source_reference` deduplication is tenant-scoped and status-scoped
  (changed in this pass).** The partial index uses
  `COALESCE(tenant_id::text, '')` so legacy NULL-tenant rows still collide
  with each other, and the indexed statuses are exactly `PREPARED`,
  `SUBMITTING`, `PENDING_UNKNOWN`, `SUBMITTED`. `REJECTED_BEFORE_SUBMISSION`,
  `CANCELLED` and `QUARANTINED` are deliberately outside it: once an attempt
  is resolved that way, a genuinely new instruction may reuse the source
  reference. An empty `source_reference` is never deduplicated (nothing to
  compare), which is why the index has `source_reference <> ''`.
- **B6. Events use canonical `com.zoikosuite.banking.*` types with the
  spec's names as the legacy event type.** Each event is written to
  `eventing_outbox` in the state-change transaction, with the attempt as
  aggregate, the tenant and legal entity in the envelope, the actor, the
  caller's correlation id and a payload containing the attempt id, status,
  idempotency key and currency (plus provider ref or reason where relevant).
  The five spec events exist. `CancelBeforeSubmission` and
  `ResolveAmbiguousSubmission` still write only internal `attempt_events`
  because the spec's BNK-06 event list has no `PaymentCancelled` or
  ambiguity-resolved event; A5 argues that resolution should nevertheless
  produce `PaymentSubmitted`/`PaymentSubmissionRejected`, which is a
  downstream contract decision.
- **B7. Read authorization requires `X-Principal-Id` and fails closed.**
  The service already required a principal for every write. Reads now do the
  same: missing header is 401, authorization-service denial is 403, and an
  authorization-service outage is 503. The attempt is fetched before the
  authorization check because the check is scoped to the attempt's legal
  entity; a caller therefore gets 404 for an unknown id and 403 for an
  existing id it cannot read. That existence signal is limited to ids
  already visible under RLS and is the same shape the write endpoints
  already had.
- **B8. Payer-account verification adds a hard treasury-svc dependency.**
  `PayerAccountVerified=false` is rejected 400 before any external call.
  When it is true, `PrepareAttempt` re-fetches the account from treasury-svc
  and requires `account_status == ACTIVE` and
  `is_ownership_verified == true` in the same tenant; a non-200, transport
  error or decode failure is 503, never "assume good". The account's
  legal-entity ownership is not compared with `legal_entity_id`; that is
  part of A2.
- **B9. The INT-07 engine is an optional, stronger path.** With
  `PROVIDER_ADAPTERS_ENABLED=true`, `PrepareAttempt` can bind a provider
  key, rail and connector; the engine refuses uncertified/mismatched
  profiles, pins the profile version and payload hash, uses a circuit
  breaker and rate-limit not-before, and records transport evidence. This is
  beyond the BNK-06 contract but is the intended production path for a real
  bank/PSP; A3 explains why the default path still matters.
- **B10. Verification in this pass.** `go test ./...` passes for the service.
  The full integration suite (`go test -tags=integration -count=1
  -timeout=400s ./...`) was run against a fresh, disposable
  `postgres:16-alpine` container through `TEST_DATABASE_URL` and passed; the
  container was removed afterwards. New coverage: read authz and cross-tenant
  idempotency (handler), concurrent same-source-reference and empty-reference
  (store), legacy outcome events with correlation (store). The pre-existing
  immutability, CAS, terminal-state, retry-identity, timeout-to-
  `PENDING_UNKNOWN` and suspended-account tests still pass.
- **B11. A6's amount is a JSON number on the wire, and exactness is
  enforced at the boundary, not trusted.**
  1. *Wire shape.* `Decimal` writes a bare JSON number of exactly its
     digits, as `float64` did, and accepts a number or a quoted string.
     Moving responses to a string would break any caller that decodes
     `Amount` into a number. Accepting a string lets callers move to the
     string form D06 prefers without a coordinated release.
  2. *Refuse, never round.* An amount the `NUMERIC(18,2)` column would round
     (`10.005`, `0.001`) or overflow (`10000000000000000`) is refused, never
     rounded to fit. Rounding would change the authorized amount, which
     invariant 13 forbids. The same check runs in the handler (400) and at
     the top of `PgStore.PrepareAttempt`, so no in-process caller can reach
     the column with an amount it would round.
  3. *One text for every consumer.* After validation the amount is held as
     `ColumnText` (`r.FloatString(2)`). That is exactly what `amount::text`
     returns, so `CheckBinding` at prepare time, the stored row,
     `LoadDispatch`'s `AmountText` (the engine's canonical and payload hash)
     and the legacy adapter's `SubmitRequest.Amount` agree digit for digit.
  4. *Verification.* `go test -tags=integration -count=1 -p 1 ./...` ran
     against a fresh `postgres:16-alpine` container through
     `TEST_DATABASE_URL`: 73 passed, 0 failed, 0 skipped. New coverage:
     decoding and encoding `Decimal` (domain); a caller's `float64` body
     stored and sent unchanged and read back as a `float64`, and refused
     amounts storing nothing (handler); `9999999999999999.99`, which has no
     `float64`, round-tripping through the column, the read and the
     prepared event, and the store's own refusal (store).
