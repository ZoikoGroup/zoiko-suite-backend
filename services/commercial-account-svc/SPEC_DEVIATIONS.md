# Spec deviations — commercial-account-svc (ZS-SVC-Q-001)

Governing documents: ZS-SVC-Q-001, *Commercial Platform Service
Specification* (COM-01 through COM-05), and ZS-COM-PFE-001, *Commercial Plan,
Feature & Entitlement Allocation Specification*. Extracted working copies are
in `scratch/commercial_platform_spec.txt` and `scratch/commercial_pfe_spec.txt`.
This is the first deviation register for this service; there was no earlier
`SPEC_DEVIATIONS.md`.

Scope of this survey: all five COM sub-specs against the real handlers, stores
and migrations 000001–000020, including the command/outbox/idempotency matrix
and the §3 shared context/error contract and §6 orthogonal state model. The
service is a seller-plane commercial service; tenant AR/GL, tenant tax and
tenant billing configuration are outside it by design.

Verification: `go build ./...`, `go vet ./...` and `go test ./... -count=1` were
run against a fresh disposable `postgres:16-alpine` container (migrations
000001–000020 applied from empty, `TEST_DATABASE_URL` pointed at that
container). All packages passed, including the store and RLS suites. No shared
development database was used for verification.

## Verified controls

These are implemented and were checked against the code and schema, not just
against comments:

- **COM-01 lifecycle and immutability.** Draft → Review → Approved → Published
  → Retired transitions exist; published price versions are protected by the
  lifecycle trigger; retirement blocks new sales without cascading to existing
  subscriptions or their bound versions. Publisher/approver separation is now
  also enforced (see B1).
- **COM-02 plan-change atomicity.** `RequestChange` writes the new version,
  term, items and proration evidence in one transaction; there is no partial
  update route. Immediate changes recompute entitlement state in that
  transaction; changes effective at a boundary are recomputed by the boundary
  worker. Lifecycle transitions now recompute as well (see B2). Add-on and
  quantity changes are first-class change kinds, not only plan swaps.
- **COM-03 canonical decision path.** The five outcomes are present
  (`ALLOW`, `ALLOW_WITH_LIMIT`, `READ_ONLY`, `RESTRICTED`, `DENY`); decisions
  are computed fresh from the effective subscription, price version,
  restrictions and policy; restrictions can only worsen an outcome; a missing
  subscription or policy fails closed. There is no cached grant that can
  outlive state. The canonical `/v1/commercial/entitlements:*` surface does not
  accept a client-supplied entitlement claim.
- **COM-04 certification and replay.** Certification freezes the statement
  population and totals; duplicate usage is constrained by
  `(meter_key, usage_event_id)`, so a replayed event cannot increase billed
  quantity; late events create `usage_adjustments` when a later open target
  statement exists; `ReopenWindow` is independent-actor guarded and
  supersedes rather than overwrites the certified population.
- **COM-05 payment and correction doctrine.** `PENDING_UNKNOWN` is a real
  payment-attempt state; an unresolved attempt is reused instead of blindly
  retried; issued invoices are immutable under a database trigger; credits,
  write-offs and refunds are separate correction documents; the service emits
  seller accounting events to the outbox and never writes tenant AR/GL tables.
- **Per-command replay.** The canonical COM-01/02/03/04/05 commands that carry
  an idempotency claim now have a replay path, including the seller-plane
  readers added in this pass (B5, B6, B7). The remaining exceptions are listed
  in A4 and A6.

## A. Not implemented, weaker than the spec, or still open

### A1. COM-01 has no materiality classifier and no Finance/Legal/Tax role gate

§4.1 requires:

> “Product/Commercial may propose; Finance approves monetary terms; Legal/Tax
> review where required; publisher cannot self-approve material price changes.”

and the control catalogue names:

> “COM-CTRL-004 | Material price-change maker-checker”

`ApprovePriceVersion` rejects an approver who is the version's creator or
submitter, and `PublishPriceVersion` now rejects a publisher who is the
approver. Migration 000020 adds the corresponding database constraint. That
is a universal maker-checker rule, which is stronger than the minimum for
non-material changes. It is not a materiality model: there is no `material`
classification, no protected-field/material-delta comparison, and no distinct
Finance, Legal or Tax authority check. Every price version is treated as
material for separation purposes, while the role-specific approval described
by §4.1 is still absent. A materiality classifier and the associated role
policy remain open.

Deployment note: 000020 is a validating constraint. A deployment that already
contains historical versions where `published_by_principal_id =
approved_by_principal_id` will fail the migration and must remediate those
rows first; the application check alone would not catch historical rows.

### A2. COM-02 has no `DRAFT` lifecycle state

§4.2 requires:

> “State | Lifecycle: Draft/Trialing/Pending/Active/CancelPending/Canceled/Expired.
> Billing health and provisioning health are separate dimensions.”

and §6 repeats:

> “Subscription lifecycle | Draft, Trialing, Pending, Active, CancelPending,
> Canceled, Expired | Commercial agreement state only.”

`LifecycleStatus` implements `TRIALING`, `PENDING`, `ACTIVE`,
`CANCEL_PENDING`, `CANCELED` and `EXPIRED`; the database check constraint has
the same set. There is no `DRAFT` state and no command that creates one.
`PlanStart` goes directly to `PENDING` (no trial) or `TRIALING` (trial).
Adding `DRAFT` without a draft command would create an unreachable state, so
this is recorded rather than papered over.

### A3. Active legacy routes remain an alternate commercial path

The non-bypassable invariants say:

> “Runtime access decisions come from COM-03 using server-resolved
> tenant/product context.”

> “Usage is billable only under an active MeterDefinition and accepted source
> contract.”

> “COM-03 is the sole runtime entitlement decision service for commercial plan
> access/limits. Clients may display entitlements but cannot self-assert them.”

The following legacy routes are still mounted and tested, alongside the
canonical COM-02/03/04 surfaces:

- `/v1/subscriptions` — `CreateSubscription`, `ResolveEntitlement`,
  `RecordUsageEvent`, `SetSubscriptionStatus`, `CreateEvaluationProgram`.
  `ResolveEntitlement` reads legacy plan/overlay limits directly, so it is a
  second runtime decision path. `RecordUsageEvent` writes
  `commercial_usage_meter_events` without COM-04 meter registration,
  statement freeze or adjustment lineage. `SetSubscriptionStatus` uses the
  legacy status enum (including `PAST_DUE` and `RESTRICTED`), which mixes
  lifecycle, billing health and entitlement state that §6 keeps orthogonal.
- `/v1/subscription-change-requests` — legacy preview/confirm change path.
- `/v1/contract-entitlement-overlays` — creates an overlay that the legacy
  resolver reads; it is not a COM-03 restriction and does not go through the
  canonical recompute.
- `/v1/billing-source-transfers` — legacy transfer path.
- `/v1/commercial-accounts` and `/v1/memberships` — legacy account/membership
  lifecycle commands.

The `/v1/price-catalogs` and `/v1/plans` write routes are deliberately retired
through `retiredCatalogWrite`; they are not active COM-01 bypasses. The
remaining routes were not retired in this pass because existing tests and
callers depend on them. Retiring them requires a consumer migration plan and
is the main structural follow-up from this survey.

### A4. Command idempotency and outbox coverage is incomplete

§3 requires:

> “Idempotency | Required for subscription changes, usage ingestion, invoice
> issue, payment collection, credits/refunds and external callbacks.”

and §10 requires:

> “All command endpoints inherit ZS-API-001 requirements for idempotency,
> concurrency and RFC 9457-compatible problem responses.”

The canonical COM-01/02/03/04/05 command handlers mostly use `commandFor` and
the stores call `claimIdempotency`. The following command paths do not, and
therefore cannot return the original result for a repeated idempotency key:

| Command | Missing claim | Outbox |
|---|---|---|
| `PUT /v1/commercial/accounts/{id}/market` (`SetAccountMarket`) | yes | none |
| `PUT /v1/commercial/currencies/{code}` (`PutCurrency`) | yes | none |
| legacy `POST /v1/commercial-accounts` (`CreateCommercialAccount`) | yes | present |
| legacy `POST /v1/memberships` (`CreateMembership`) | yes | present |
| legacy `DELETE /v1/memberships/{id}` (`DeactivateMembership`) | yes | present |
| legacy `POST /v1/subscriptions` (`CreateSubscription`) | yes | present |
| legacy `POST /v1/subscriptions/{id}/evaluation-program` | yes | none |
| legacy `POST /v1/subscriptions/{id}/usage-events` (`RecordUsageEvent`) | yes | none |
| legacy `POST /v1/subscriptions/{id}/status` (`SetSubscriptionStatus`) | yes | present |
| legacy `POST /v1/subscription-change-requests` (preview) | yes | none |
| legacy `POST /v1/subscription-change-requests/{id}/confirm` | yes | present |
| legacy `POST /v1/contract-entitlement-overlays` (`CreateOverlay`) | yes | none |
| legacy `POST /v1/billing-source-transfers` (`TransferBillingSource`) | yes | present |
| `POST /v1/commercial/plan-transition-rules/{id}` (retire) | yes | present |
| `POST /v1/commercial/meter-definitions/{key}/{version}` (retire) | yes | none |
| `POST /v1/commercial/restrictions/{id}` (remove) | yes | present |
| `POST /v1/commercial/entitlements:recompute` | yes | present |

`CreateUsageAdjustment` already had a claim and now has a replay path (B6).
`CreateSnapshot`, `ApplyRestriction`, `PublishEntitlementPolicy`,
`RegisterMeterDefinition`, `CorrectUsageEvent`, `CloseUsageWindow`,
`StatementAction`, and the COM-05 invoice/payment/dunning/dispute/credit
commands all have claims and replay paths. The table is the remaining gap, not
a claim that every command in the service is unguarded.

### A5. COM-03 policy publication invalidates globally, not per tenant

§4.3 says:

> “Events | EntitlementSnapshotChanged; CapabilityGranted/Restricted/Revoked;
> LimitChanged; GracePeriodStarted/Ended.”

and §9 requires:

> “09 | Downgrade scheduled but high-tier feature remains usable after
> effective time | COM-03 revokes/limits at boundary; stale cache invalidated.”

`PublishEntitlementPolicy` writes one outbox event with aggregate
`entitlement_policy` / `grace-policy`. It does not recompute or emit an
`entitlement_snapshot.changed` event for each affected tenant. The canonical
`Evaluate` path is safe because it computes fresh, but a cache-fronted caller
must invalidate the entire policy cache rather than a tenant's snapshot. There
is no per-capability cache-revocation SLO or freshness metadata in the
service, because there is no cached decision path to measure. This is
fail-safe for the service's own reads but weaker than the per-tenant
invalidation implied by the event contract.

### A6. Replay for evidence commands returns current state, not the original result

§3 requires idempotent commands to return the original result, and §4.3
requires evidence to include “Decision input versions, policy version, reason
code, source subscription version, usage snapshot ID.” Two replay paths do not
honour that exactly:

- `CreateSnapshot` replays by calling `GetEffectiveEntitlements` for the
  organization at `now`, not by returning the snapshot captured by the
  original command. If state changed between the original call and the replay,
  the response differs from the original evidence.
- `PublishEntitlementPolicy` replays through `GetEntitlementPolicy(now)`, so a
  future-dated policy that was published but is not yet effective replays as
  the currently effective policy, not the version created by the original
  command.

The claims prevent duplicate writes, but the replayed response is not a
faithful original-result replay. A snapshot/policy-version lookup by claim
resource id would close this.

### A7. COM-04 corrections after freeze or certification are rejected

§4.4 requires:

> “Failure semantics | At-least-once intake with dedup. Negative/invalid
> quantities quarantine. Late events after certified cutoff create adjustment,
> not mutation.”

and §9 requires:

> “18 | Late usage arrives after certified invoice cutoff | Create prospective
> adjustment; original invoice population unchanged.”

> “19 | Operator edits certified UsageStatement total manually | Reject;
> correction via UsageAdjustment.”

`CorrectUsageEvent` rejects the command when the original event's statement is
not `OPEN` (`ErrUsageEventNotCorrectable`), and it rejects corrections of an
event already marked `late`. There is a separate `CreateUsageAdjustment`
command, but it requires a source event that already has a statement linkage
and a target statement that is open; it is not a general correction path for
an event in a frozen or certified statement. The result is that the canonical
`CorrectUsageEvent` command cannot produce the adjustment lineage the spec
requires once the window has closed. This was left in place because the
existing tests explicitly assert the rejection; it remains an open A-item.

### A8. COM-04 late events cannot always create an adjustment

The automatic late path in `registerLateEvent`:

1. records the late event with `statement_id = NULL` and `late = TRUE`;
2. resolves the subscription term at `observedAt` (the observation time);
3. calls `findOrCreateOpenStatement` for that term; and
4. inserts a `usage_adjustments` row from the origin statement to the target.

This works when the observation time falls in a later term that has, or can
create, an open statement. It does not work in two real cases:

- **Same-term frozen statement.** If the origin statement for the current term
  is `FROZEN` (not yet certified), `findOrCreateOpenStatement` returns
  `ErrStatementInvalidState` because the one live statement for that window is
  not open. The adjustment also cannot target the origin because the
  `usage_adjustments` schema requires distinct origin and target statements.
  The transaction rolls back, so the late event is not persisted at all.
- **Post-final-term event.** If the subscription has ended and `observedAt` is
  after its last term, `resolveTerm` fails with `ErrNoOpenTermForCorrection`.
  There is no target statement to carry the quantity forward, and again the
  late event is not persisted.

Both cases are weaker than “Late events after certified cutoff create
adjustment, not mutation.” The service currently needs a later open term to
represent the adjustment. A durable late-event inbox with a pending
adjustment that can be attached when a target exists would be the structural
fix.

### A9. COM-04 `ReopenWindow` does not carry adjustments or counts

§4.4 lists `ReopenWindow` as a controlled-exception command, and §6 says:

> “Usage statement | Open, Frozen, Certified, Adjusted, Superseded | Certified
> population is immutable.”

`ReopenWindow` marks the certified statement `SUPERSEDED`, creates a new
`OPEN` statement, and re-homes accepted `usage_event_records` onto the
replacement. It does not re-home or copy existing `usage_adjustments` that
targeted the superseded statement, and the replacement's `event_count` and
`quarantined_count` start at zero. Recertifying the replacement therefore
does not reproduce the original certified total plus its adjustment lineage.
The original is preserved, but the replacement's evidence is incomplete.
This is a structural reopen/adjustment carry-forward gap.

### A10. COM-05 never rates `ONE_TIME` or `DISCOUNT` components

§4.5 requires:

> “Required inputs | Subscription versions, certified usage statements,
> price/rating versions, authorized discounts/credits, merchant tax result,
> billing period/cutoff, customer billing profile.”

§5 requires:

> “Rating | Price component + quantity + discount policy versions |
> Deterministic line amount and rating evidence.”

and §4.1 defines:

> “One-time | amount, trigger, eligibility, expiration | Explicit
> trigger/evidence; no hidden recurring conversion.”

> “Discount/credit | type, value/cap, duration, eligibility, approval | Never
> embedded as mutable invoice-line override.”

`RateInvoiceBasis` rates only `RECURRING_FIXED`, `PER_UNIT` and `METERED`
components. Its own comment says: “ONE_TIME and DISCOUNT components are not
rated here (out of scope for this part of COM-05).” Approved discounts and
one-time charges can be stored on a price version and approved on a
subscription, but they never reach an invoice candidate. This is a material
COM-05 rating gap.

### A11. COM-05 collection health is incomplete

§6 requires:

> “Billing health | Current, PaymentPending, PastDue, Collections, WrittenOff |
> Does not itself determine runtime access.”

`CollectionStateOf` returns only `CURRENT`, `PAYMENT_PENDING`, `PAID` or
`FAILED`, derived from payment attempts. It does not produce `PAST_DUE`,
`COLLECTIONS` or `WRITTEN_OFF`, even though dunning cases, write-offs and
outstanding balances exist elsewhere in the service. The billing-health
dimension is therefore narrower than the orthogonal state model. Dunning and
write-off state are visible through their own queries, but a caller cannot get
the spec's consolidated billing-health state from `CollectionState`.

### A12. COM-05 dunning `SUSPENDED` maps to `RESTRICTED`, not `DENY`

§2 requires:

> “Payment failure and entitlement restriction are related through explicit
> commercial policy. A processor failure does not automatically authorize
> destructive data loss or immediate full tenant shutdown.”

§4.5 requires:

> “Payment delinquency affects entitlement only through versioned
> grace/restriction policy.”

and §6 models dunning as:

> “Dunning | None, Notice1, Notice2, Restricted, Suspended, Closed | Versioned
> commercial policy; does not delete data.”

`AdvanceDunning` correctly applies a restriction through COM-03, but
`RestrictionLevel` has only `READ_ONLY` and `RESTRICTED`. Both the
`RESTRICTED` and `SUSPENDED` dunning stages therefore map to
`RestrictionRestricted`; the most severe dunning stage cannot produce a
`DENY` outcome. This is a deliberate current limitation, not a silent
shutdown, but it leaves the `SUSPENDED` state weaker than the state model
suggests. Adding a `DENY` restriction level would require a migration and a
review of every restriction consumer.

## B. Interpretations and additions (decisions a reviewer may reverse)

- **B1. Publisher/approver separation is unconditional.** Changed in this
  pass. `PublishPriceVersion` now refuses when the publisher is the approver,
  and migration 000020 enforces the same rule in the database. The spec's
  minimum is “publisher cannot self-approve material price changes”; applying
  it to every version is stricter and avoids needing a materiality classifier
  for the SoD edge. A reviewer could replace this with a materiality-scoped
  rule once A1 is modelled.
- **B2. Lifecycle transitions recompute entitlement state in the same
  transaction.** Changed in this pass. `StartSubscription` (when already
  effective), `Renew`, `Activate`, `CancelNow`, `ScheduleCancellation` and
  `Reactivate` now call `recomputeAfterLifecycle`. Future-dated starts and
  boundary-effective changes remain the boundary worker's job; doing them
  early would create a snapshot for a version that is not effective yet.
  `RequestChange` keeps its existing recompute to avoid a duplicate snapshot.
- **B3. Direct restriction apply/remove recomputes and emits
  `entitlement_snapshot.changed`.** Changed in this pass. `ApplyRestriction`
  and `RemoveRestriction` now recompute after the restriction write, so the
  restriction and the decision evidence commit together. Dunning's
  `AdvanceDunning` already recomputed and was left unchanged to avoid a
  duplicate snapshot.
- **B4. Cross-tenant restriction reads and quota reads were corrected/batched.**
  Changed in this pass. `loadEntitlementBasis` and `loadActiveRestrictions`
  are now organization-scoped, closing a cross-tenant restriction leak.
  `currentPeriodConsumptions` replaces the per-capability consumption query
  loop with batched term, statement, meter-definition and accepted-event
  reads. `loadUsageBasis` and `IssueInvoice`'s statement-evidence reads were
  batched the same way, and `loadRegisteredMeters` /
  `loadRegisteredMetersForCapabilities` now share one `unnest`-based lookup
  instead of one query per meter. These are implementation corrections, not
  new spec policy.
- **B5. Replay readers use the seller plane where the claim has no tenant
  context.** Changed in this pass. `GetCreditNoteAsSeller`,
  `GetWriteOffAsSeller`, `GetRefundRequestAsSeller`,
  `GetPaymentAttemptAsSeller`, plus the existing seller-scoped invoice,
  candidate, dunning and dispute readers, are optional concrete-store
  interfaces. Handlers type-assert to them so existing store interfaces and
  test stubs keep compiling. The alternative — threading a tenant id through
  every replay callback — would be a larger interface change.
- **B6. `CreateUsageAdjustment` now has a replay reader.** Changed in this
  pass. `GetUsageAdjustment` is keyed by target statement, meter key and
  source event id, matching the claim's resource id. It is an optional
  interface, like B5, so the `UsageStore` interface and its stubs are
  unchanged.
- **B7. `IssueInvoice` replay resolves the candidate first, then the issued
  invoice under the candidate's tenant scope.** Changed in this pass. The
  claim resource id remains the candidate id; the handler reads the candidate
  as seller, then reads the invoice with the candidate's organization as the
  tenant. This preserves the existing claim semantics and avoids a second
  global invoice reader.
- **B8. COM-04 late-event origin lookup excludes `SUPERSEDED` statements.**
  Changed in this pass. The origin query now selects the latest non-superseded
  statement for the window, so a reopened window's replacement is the origin
  of a subsequent adjustment rather than the superseded predecessor.
- **B9. The legacy `/v1/price-catalogs` and `/v1/plans` writes stay retired.**
  They return `retiredCatalogWrite` rather than being deleted. This keeps the
  retirement visible and testable while preventing a COM-01 bypass. The other
  legacy routes in A3 were not retired because their tests and callers still
  depend on them; retiring them is a separate migration project.
- **B10. `DRAFT` was not added to COM-02.** A2 records the gap. Adding an
  unreachable lifecycle state without a draft command would make the schema
  look more compliant than the service is, and would complicate the existing
  `PlanStart`/transition logic. A draft command and its persistence rules
  should be designed together.
- **B11. COM-04 correction-after-certification remains a rejection.** A7
  records the gap. The existing tests assert `ErrUsageEventNotCorrectable`
  for this case, and the explicit `CreateUsageAdjustment` command is the
  current correction path. Changing the rejection to automatic adjustment
  routing is a behavior change that needs its own test and API review.
- **B12. COM-05 tax computation remains external.** The service accepts a
  merchant tax result (`tax_jurisdiction_code`, `tax_rate_basis_points`,
  `tax_amount`), validates the jurisdiction and registered rate, and freezes
  the result on the candidate/invoice. It does not compute tax and does not
  read tenant tax configuration. This matches §4.5's “merchant tax result”
  input and §5's dedicated seller tax context, but a reviewer could require
  a stronger tax-service integration or an amount-versus-rate check.
- **B13. COM-05 discount/one-time rating was deferred.** A10 records the gap.
  Rating a discount requires a bound discount policy version and an
  eligibility decision; rating a one-time charge requires an explicit trigger
  and evidence. Neither binding exists yet, so adding a line item from the
  component alone would create an unsupported charge. This needs a separate
  COM-05 rating extension.
- **B14. Dunning `SUSPENDED` uses the existing `RESTRICTED` level.** A12
  records the gap. The current `RestrictionLevel` has no `DENY`, and adding
  one requires a schema migration plus a review of every restriction
  consumer. The current mapping is conservative (it restricts rather than
  grants) and does not delete data.
- **B15. Evidence-command replay returns current effective state.**
  A6 records the gap for `CreateSnapshot` and `PublishEntitlementPolicy`. The
  alternative would require snapshot-id and policy-version lookups that the
  current `EntitlementStore` interface does not expose. The claims still
  prevent duplicate rows; only the replayed body can differ.
- **B16. Migration 000020 is validating, not `NOT VALID`.** The strict form
  was chosen so the database proves the new SoD rule for all rows. The
  deployment prerequisite is in A1: remediate historical self-published rows
  before applying it. A `NOT VALID` constraint would deploy more easily but
  would leave the historical population unproven.
