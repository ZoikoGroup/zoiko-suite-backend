# Spec deviations — payee-banking-identity-svc (ORG-10)

Governing document: `ZoikoSuite_Organization_Legal_Entity_Global_Reference_Data_Detailed_Service_Specifications.docx`,
§4.10 "ORG-10 — Banking Identity / Payee Master", including its table row
"Purpose — Maintain controlled external beneficiary/payee banking identity and
verified destination versions." and the four mandatory engineering controls
under it. First full survey of this service against §4.10 (2026-10-23).

Citation note: the docx section is a table followed by four control paragraphs.
Quotations below are the exact cell/paragraph text; where a row contains
semicolon-separated requirements, the relevant clause is quoted in full.

Overall: the six named commands, their state transitions, account masking, the
transactional outbox, tenant RLS and the real ORG-07 party lookup exist. The
authoritative four-object model, most server-resolved context, independent
verification, read authorization/purpose limitation, the verified event,
event-driven AP invalidation and at-rest tokenisation are not present. Command
idempotency and the activation auto-supersede event are closed in this pass.

## A. Not implemented / weaker than the spec

1. **A1 — No `Idempotency-Key` on any of the six named commands. Closed (B1).**
   §4.10 names the command set: "Named commands | ProposePayeeDestination;
   VerifyPayeeDestination; ApprovePayeeDestination; ActivateDestination;
   SuspendDestination; SupersedeDestination". Every one is a material write, and
   the platform command contract requires a retry identity. Before this pass the
   service had no `command_idempotency` table and no key handling at all: a
   lost-response retry either failed the current-state guard with `409` or, for
   `ProposePayeeDestination`, got `409` "a destination with this
   institution/account/currency already exists" instead of the original
   candidate. None of the six returned the original success. Closed by migration
   `deployments/migrations/000005_add_command_idempotency.{up,down}.sql` and the
   claim/replay logic in `internal/store/pg_store.go`; see B1.

2. **A2 — The four authoritative write objects are collapsed into one table.**
   §4.10 assigns "Authoritative write ownership | PayeeBankingIdentity,
   PayeeDestinationVersion, VerificationEvidence, ChangeRiskRecord." The schema
   has only `payee_destinations` (plus its audit-event table). There is no
   separate `PayeeBankingIdentity` (party-level bank identity), no versioned
   `PayeeDestinationVersion` object with effective dating, no
   `VerificationEvidence` object carrying the evidence itself, and no
   `ChangeRiskRecord` at all. Verification evidence is a method plus a free-text
   reference stored on the destination row
   (`verification_method`, `verification_evidence_ref`); risk classification is
   absent. This is the largest structural gap in the service: the lifecycle is
   implemented on one row whose identity fields are protected by an
   immutability trigger, not on the authoritative object set the spec names.

3. **A3 — Most "Server-resolved context" is not resolved.** §4.10 requires
   "Server-resolved context | Format validation; account-country rules;
   sanctions/fraud hooks; party status; change-risk classification; prior
   version." The implementation only checks that required fields are non-empty
   and that `source_type` is one of four enum values. It does not validate
   account/IBAN formats, apply account-country rules, call any sanctions/fraud
   hook, classify change risk, or consult the prior version before proposing a
   candidate. The ORG-07 lookup does retrieve `Status` and `ComplianceStatus`,
   but `ProposePayeeDestination` discards the result: it only checks that the
   party exists and belongs to the legal entity. A blocked/inactive or
   non-compliant party can therefore still get a destination candidate.

4. **A4 — `VERIFICATION_PENDING` is unreachable; verification skips it.**
   §4.10's state model is "Candidate → VerificationPending → Verified →
   ApprovalPending → Active → Suspended/Superseded." `VerifyDestination` updates
   `status` directly from `CANDIDATE` (or the never-entered
   `VERIFICATION_PENDING`) to `VERIFIED`. No command or store path ever writes
   `VERIFICATION_PENDING`. The constant and the `CanProposeVerification` guard
   exist, but the state is dead. This also means there is no place to represent
   "verification requested but independent evidence not yet received", which is
   exactly the state the spec's mandatory control implies.

5. **A5 — `VerifyAuthorizationFingerprintInput` does not exist.** §4.10 lists
   "Read/query surfaces | GetActivePayeeDestination; ListPayeeVersions;
   GetPayeeChangeHistory; VerifyAuthorizationFingerprintInput." The first three
   have handlers; there is no route, handler, request type, or store method for
   `VerifyAuthorizationFingerprintInput`. AP-10 instead calls
   `GetActivePayeeDestination` and compares `DestinationID` (see A10); there is
   no purpose-built surface that takes an authorization's captured fingerprint
   input and answers whether the active destination still matches it.

6. **A6 — Segregation of duties is only the maker/approver half; break-glass is
   absent.** §4.10 requires "Maker/approver separation; supplier-profile editor
   cannot alone activate changed beneficiary details; break-glass separately
   controlled." `ApprovePayeeDestination` does enforce that the destination's
   proposer cannot also approve it (own-object SoD via authorization-svc), which
   is real. But `VerifyPayeeDestination` and `ActivateDestination` have no
   own-object check, so the proposer may verify the destination and may activate
   it once someone else has approved it; there is no distinct verifier or
   activator separation. There is also no break-glass mechanism, no separate
   break-glass principal/action, and no audit path for one. The minimum
   "cannot alone activate" is therefore met narrowly (a second principal must
   approve), but the full SoD line is not.

7. **A7 — Read paths do not gate access on an authorization decision, and have
   no purpose limitation.** §4.10 requires "Payee-master admin; masked access;
   read of full destination strictly purpose-limited." Masking is real:
   `AccountIdentifier` is cleared unless the caller holds
   `PAYEE_MASTER_PRIVILEGED_READ`. But that privileged check is only a masking
   decision: for `GetPayeeDestination`, `ListPayeeVersions` and
   `GetActivePayeeDestination`, a denial clears the account and still returns
   `200`; it never denies the read. `GetPayeeChangeHistory` calls no
   authorization at all. Thus any authenticated caller in the tenant can
   enumerate destinations by party or fetch one by guessed ID, with masking as
   the only control. The privileged read is a bare action check with no
   purpose/ticket/context binding, so "strictly purpose-limited" is not
   implemented. Tenant RLS limits the blast radius but does not replace
   payee-master read authorization.

8. **A8 — The event set is incomplete, and one extra event is emitted.**
   §4.10's "Events produced | PayeeDestinationProposed;
   PayeeDestinationVerified; PayeeDestinationActivated;
   PayeeDestinationSuspended; PayeeDestinationSuperseded" includes
   `PayeeDestinationVerified`; no `com.zoikosuite.banking.payee-destination.verified`
   outbox row is ever written. Conversely, the service emits
   `com.zoikosuite.banking.payee-destination.approved` and an internal
   `PAYEE_DESTINATION_APPROVED` event, although the spec's event list has no
   `PayeeDestinationApproved`. The activated auto-supersede path originally had
   the same problem for the old active row; that part is closed in B2. The
   verified event remains an A gap: a subscriber cannot learn from the outbox
   that a destination became `VERIFIED`.

9. **A9 — ORG-10 publishes the high-risk event, but AP authorization controls do
   not consume it.** The mandatory control says "Any active-destination change
   emits an immediate high-risk event consumed by AP authorization controls."
   The first half is now real for activation, explicit supersede, suspension,
   propose and approval: they enqueue in the same transaction as the state
   change. The second half is not. `payment-authorization-svc` has no
   Kafka/event consumer in this codebase; it protects itself by synchronously
   calling `GetActiveDestination` at request, approval, consumption and
   validation time and failing closed on a changed/absent pinned destination
   (`internal/handler/handler.go`'s `verifyStillEligible`). That is a real pull
   control, but it means the event is not "consumed by AP authorization
   controls" and a destination change between those checkpoints does not
   proactively invalidate an already-open authorization. The exact missing
   piece is a subscriber that maps `payee-destination.activated`,
   `payee-destination.suspended` and `payee-destination.superseded` to the
   affected party/payee snapshots and invalidates open authorizations.

10. **A10 — AP-10 pins `DestinationID`, not the spec's "active version"
    fingerprint.** §4.10's dependency line names "AP-10 fingerprints active
    version." The payment-authorization client decodes only `DestinationID`,
    `LegalEntityID`, `Status` and the proposer/verifier/approver principals; it
    does not decode ORG-10's `Fingerprint`. It pins `DestinationID` at request
    time and `verifyStillEligible` re-fetches the active destination and compares
    IDs, invalidating on a change or absence. That catches the normal ORG-10
    mutation path, because a changed account creates a new destination row. It
    does not catch an in-place mutation of the same row's institution/account/
    currency, which is the case the fingerprint exists to detect. The schema's
    immutability trigger rejects such an in-place mutation and the store never
    attempts one, so this is a spec-semantics/defence-in-depth gap rather than a
    reachable bypass through supported commands.

11. **A11 — The uniqueness indexes are not tenant-scoped.** §4.10's
    "Idempotency / concurrency" row requires "Destination candidate fingerprint
    detects duplicates; activation only one active version per party/scope
    unless explicit multi-destination policy." The two database invariants are
    `uq_payee_destinations_fingerprint (party_ref, fingerprint) WHERE status <> 'SUPERSEDED'`
    and `uq_payee_destinations_active_scope (party_ref, scope) WHERE status = 'ACTIVE'`
    — neither includes `tenant_id`. RLS confines reads
    and writes, but a PostgreSQL unique index is global, so the database treats
    equal `(party_ref, fingerprint)` or `(party_ref, scope)` values in two
    tenants as a conflict. `party_ref` is an ORG-07 UUID and is very likely
    globally unique in practice, which is why this is not an everyday failure;
    it is still not a structural tenant boundary. A migration should add
    `tenant_id` to both indexes (with the same partial predicates) before this
    service is relied on for cross-tenant isolation.

12. **A12 — Full account identifiers are masked on output but not tokenized or
    encrypted at rest.** The mandatory control says "Full account identifiers
    are tokenized/masked and exposed only under purpose-limited privileged
    access." Responses are masked (last4 always; full value only on the
    privileged action), and event payloads use a safe projection with
    `AccountLast4` and no account identifier. But the
    `payee_destinations.account_identifier` column stores the full value in
    plaintext; there is no
    tokenization, envelope encryption, KMS call, or secret-vault integration.
    A database/backup reader gets the full account. The service's own package
    doc acknowledges the encryption half; the tokenization half is simply not
    built.

13. **A13 — Evidence and lineage are references, not authoritative records.**
    §4.10 requires "Evidence / lineage | Original source, independent
    verification, risk checks, approvers, masked fingerprint, effective
    version." The row stores `source_type`, a free-text
    `verification_evidence_ref`, the reviewer/approver principal IDs, a SHA-256
    fingerprint, and `created_at`/`updated_at`. It does not store the original
    source document/reference, the independent verification evidence itself, any
    risk-check result, an effective-from/effective-to version window, or a
    version chain beyond `superseded_by_destination_id`. The audit-event table
    records that a transition happened, not the evidence object that justified
    it. This is weaker than the spec's lineage requirement and is the same
    object-model gap as A2 seen from the evidence side.

14. **A14 — Two dependency groups are not wired: BNK provider validation and
    GOV-04/06/07/12.** §4.10's dependency line is "ORG-07; BNK provider
    validation; GOV-04/06/07/12; AP-10 fingerprints active version." ORG-07 is
    a real HTTP lookup. AP-10 is a real pull (A9/A10). BNK provider validation
    does not exist: `VerifyPayeeDestination` accepts a caller-supplied method
    and evidence reference, with no bank-account-verification provider call.
    GOV-04/06/07/12 are not called either; the service uses authorization-svc
    directly for its own SoD check. The consequence is that "independent
    verification appropriate to risk/jurisdiction/provider" is attested by the
    caller, not independently established by this service.

## B. Interpretations and additions

- **B1. Command idempotency (closes A1).** The implementation follows the
  platform pattern used by `financial-control-svc`, `general-ledger-svc`,
  `consolidation-svc`, `intercompany-accounting-svc` and
  `reconciliation-engine-svc`, not a new model. `command_idempotency` has
  `PRIMARY KEY (tenant_id, idempotency_key)`, a `command` name and a
  `destination_id` target, RLS by `app.tenant_id`, and is written in the same
  transaction as the state change and outbox event. A command claims the key
  only after its write/event enqueue succeeds; a refused command rolls back
  before the claim, so it does not burn the key. Replay returns the row as it
  now stands, with `Idempotent-Replay: true`; `ProposePayeeDestination` keeps
  its 201 on the first response and returns 200 on a replay, matching the
  platform's command convention. The same key for a different command or
  destination is `409` (`ErrIdempotencyKeyConflict`). A missing key is `400`.
  Propose additionally compares the full request (including normalized scope)
  so the same key with a changed body is a conflict, not a silent replay. For
  lifecycle commands the replay identity is `(command, destination)`, not the
  request bytes; the first committed transition wins, which is why a replay
  after a later state change returns the current state rather than re-running
  the old transition. A concurrent first attempt on a lifecycle command is
  serialized by the destination row lock; the loser sees the winner's claim and
  replays. Concurrent `ProposePayeeDestination` calls are serialized by the
  destination fingerprint/claim uniqueness instead; the loser re-reads the
  winner's claim and replays, and a genuine duplicate with a fresh key still
  returns `409`. One residual edge remains: `ProposePayeeDestination` performs the ORG-07 party
  lookup before it reaches the store, so an ORG-07 outage on a retry returns
  `503` rather than replaying the already-committed candidate. The key is still
  not burned and a duplicate is still prevented; the replay is simply not
  reachable while the dependency is down. Tests:
  `internal/handler/idempotency_test.go` and
  `internal/store/idempotency_integration_test.go` (missing and oversized
  keys, all six commands, refused key, cross-command and cross-destination
  conflicts, changed propose body, concurrent retry, replay after later state
  change).

- **B2. Activation's auto-supersede now emits the old destination's event
  (closes the activation half of A8).** §4.10's mandatory control is "Any
  active-destination change emits an immediate high-risk event." Activating a
  destination for a party/scope that already has an `ACTIVE` row changes two
  rows: the new one becomes `ACTIVE` and the old one becomes `SUPERSEDED`.
  Before this pass only the new row reached the outbox; the old row had only an
  internal audit event. `ActivateDestination` now enqueues
  `com.zoikosuite.banking.payee-destination.superseded` for the old row in the
  same transaction, with the safe projection and `status: "SUPERSEDED"`, as
  well as the new row's `activated` event. This is a deliberate extension of
  the prior dual-write behavior, not a claim that the old behavior was correct.
  Test: `TestPgStore_Outbox_ActivateDestination_EnqueuesBothSidesOfTheChange`.

- **B3. The list-path authz N+1 is fixed; the store layer itself has no N+1.**
  `ListPayeeVersions` previously called `maskIfNotPrivileged` once per returned
  version, and each call made an authorization-svc request. It now caches the
  privileged-read decision per `LegalEntityID` (`maskVersions`), so a normal
  party's version list makes one authz call per distinct entity rather than one
  per version. The store layer was audited separately: every read/command is a
  single set-based query (or a constant number of them); there is no per-row
  query loop, and the control-population path is set-based. Regression test:
  `TestListPayeeVersions_PrivilegedCheckOncePerEntity`. One cross-service N+1
  remains outside this store: payment-authorization's `bankChangerConflict`
  makes one ORG-10 HTTP call per payee ref. It is not part of this service's
  store layer and was left unchanged.

- **B4. AP-10 wiring was re-checked; the stale handler comment is corrected.**
  `payment-authorization-svc` really does re-fetch the active destination: it
  pins `DestinationID` at `RequestPaymentAuthorization`, then
  `verifyStillEligible` re-fetches at `ApprovePayment` and
  `ConsumePaymentAuthorization`, and `ValidateAuthorization` mirrors the check;
  a changed or absent destination invalidates the authorization and fails
  closed. The comment in `ActivateDestination` saying "No real caller does that
  yet" was therefore stale and is corrected. What is still missing is the
  event consumer and the fingerprint comparison, documented as A9/A10; the
  pull path is not a substitute for either.

- **B5. `APPROVAL_PENDING` is the post-approval resting state, and an
  `approved` event is an addition.** §4.10's state line has no separate
  `APPROVED` state and no `ApprovePayeeDestination` state target, while the
  command list has `ApprovePayeeDestination` followed by `ActivateDestination`.
  The service reads `APPROVAL_PENDING` as "approved, awaiting activation." It
  emits `PAYEE_DESTINATION_APPROVED` and
  `com.zoikosuite.banking.payee-destination.approved` in addition to the
  spec's five events. This is additive, carries the same safe projection, and
  is used by the internal change history; it is not claimed to be in the
  spec's event list.

- **B6. Invoice/email data is candidate evidence, not a permanent bar.**
  §4.10 says "Bank details from invoice OCR, email or free text are candidate
  evidence only" and "Activation requires independent verification appropriate
  to risk/jurisdiction/provider." The service interprets this as: a candidate
  sourced from `INVOICE_OCR` or `EMAIL` can never be verified by re-asserting
  its own source (`CanVerify` rejects the same method/`SAME_AS_SOURCE`), but it
  can be verified by a genuinely independent method (for example an outbound
  call to the supplier), and only then can it activate. This is why
  `TestVerifyPayeeDestination_InvoiceSourced_RequiresIndependentMethod` expects
  409 for same-source verification and 200 for an independent method. The
  stricter reading — invoice-sourced data never activates at all, even with
  independent verification — is not what the code implements; if that is the
  intended reading, A14 (no BNK/provider check) means the current service
  cannot satisfy it either.

- **B7. Scope defaults to one active destination per party; multi-destination
  policy is not modeled.** §4.10 permits "only one active version per
  party/scope unless explicit multi-destination policy." The service supports
  a `scope` field (default `DEFAULT`) and enforces one `ACTIVE` row per
  `(party_ref, scope)` in the application plus a partial unique index. There is
  no API for declaring an explicit multi-destination policy, no policy object,
  and no scope registry. The default single-destination behavior is conforming;
  a future policy layer would need to be designed rather than assumed.

- **B8. An unpinned payee is allowed only while ORG-10 has no active
  destination.** AP-10 treats `404` from `GetActivePayeeDestination` as a real
  coverage absence, not an outage: a payee with no ORG-10 active destination
  can be authorized unpinned, but if one appears later, `verifyStillEligible`
  invalidates the authorization. An ORG-10 transport/5xx failure fails closed.
  This is a deliberate interpretation of "unverified/changed destination blocks
  new authorization": absence is not treated as a failed check, but a later
  appearance is treated as a change.

- **B9. Fingerprints are SHA-256 digests, not reversible account values.**
  `Fingerprint(institution, account, currency)` returns a hex SHA-256 digest;
  `AccountLast4` is the display mask. The digest is returned in the HTTP
  representation and in the safe event projection, which makes it a stable
  pseudonymous identifier for duplicate detection and event correlation, not a
  disclosure of the account. It is still linkable across records in the same
  tenant; the spec's "masked fingerprint" is read as this digest plus last4,
  not as a per-reader salted value. A stronger design would scope/salt the
  digest or omit it from broader responses.

- **B10. Event payloads are deliberately narrower than the domain object.**
  `DestinationEventPayload` carries the destination/party/legal-entity IDs,
  scope, status, source type, country, currency, last4, fingerprint, timestamps
  and `superseded_by_destination_id`, but not the full account identifier, payee
  name, or institution. The actor lives on the outbox envelope, not in this
  payload. That is an intentional application of "full account never
  overexposed" to the event bus; a consumer needing more must call this
  service's read path (which itself needs the A7 fix). The cost is that the event
  is not a complete projection of the destination, and a subscriber cannot
  reconstruct the full before/after object from the event alone. This is an
  interpretation, not a spec violation, but it constrains what an AP-10 event
  consumer could do without a follow-up read.

## Verification performed

- `gofmt` on every changed Go file; `go vet ./...` clean.
- `go test ./...` (service module) — all unit tests pass.
- `go test -tags=integration -count=1 -timeout=600s ./...` with
  `TEST_DATABASE_URL` pointed at a fresh disposable `postgres:16-alpine`
  container (`payee_it` database), not the shared development database — all
  unit and store integration tests pass, including the new
  `idempotency_integration_test.go` suite and the updated outbox assertions.
- `payment-authorization-svc`: `go test ./internal/handler -run
  'ORG10|Payee|Destination'` passes, covering the existing pull-based
  fail-closed AP-10 path that B4/A9/A10 describe.
- The integration harness applies every `deployments/migrations/*.up.sql` to a
  clean database and drops `command_idempotency` before `payee_destinations`
  (the foreign-key dependency) between tests.
