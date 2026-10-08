# Decision: where tax rates and thresholds live (ZS-JUR-001 vs "Model B")

Status: **decided** (taken by the implementer on the owner's delegation; revisitable). Date: 2026-10-03.
Scope: `jurisdiction-rules-svc`, and the migration path for `tax-rules-svc` / `tax-determination-svc`.

## The conflict
* This service's earlier rule (OQ-1 "Model B", from the 03-microservices note and the migration comments): `rule_payload` holds
  **applicability metadata only**; rates, thresholds and bands belong to the Tax and Payroll services.
* ZS-JUR-001 (the August 2026 baseline) s2, s3, s11, s12: rates, thresholds, formulas and exemptions live in **immutable released rule
  modules with provenance and tests**; "no production regulatory rule exists outside a registered pack artifact"; "tax rates stored as
  editable rows" is listed as a named risk. `tax-rules-svc` today stores editable `tax_rate_percentage` rows (`PUT /{id}`), which that
  standard forbids.

## Decision
**ZS-JUR-001 governs.** Calculation values live in the rule, as a NEW typed field `parameters`, and travel in signed, tested, certified,
released pack artifacts. Specifically:

1. `rule_payload` is **unchanged** and keeps its meaning (applicability metadata). Nothing existing reads or writes differently, so the
   change is additive and reversible. This honours Model B's intent (do not smuggle computation into the free-form payload) while moving
   the *authoritative location* of computation values to the governed, versioned place the newer standard requires.
2. Parameters are a **closed, validated schema**, not a formula language (none has been chosen, s38): `TAX_RATE` (flat) and `TAX_BANDS`
   (marginal), each with one declared rounding step. All numbers are **decimal strings** (a JSON number is refused: JUR-NEG-25), executed
   with exact rational arithmetic, rounded once at the end (HALF_UP, HALF_EVEN, UP, DOWN; scale 0 to 6).
3. Parameters are editable only while the rule is DRAFT and are frozen by a database trigger afterwards; the compiler re-validates them
   (second line of defence) and snapshots them into the artifact with a content digest; a rule that carries parameters cannot be certified
   without calculation test coverage (golden, every band limit, an actually-rounded case, a refused unsupported amount).
4. Runtime calculation is served only from verified, released artifacts (`POST /v1/regulatory-calculations:execute`), fails closed, and
   records inputs, steps, result, rule and pack versions as evidence.

## Why this option
* It is the newest governing standard and states the invariant outright; the alternative would leave a regulated rate in mutable rows
  with no release gate, which the standard names as a defect.
* It is the only option that gives a rate the properties a legal value needs: provenance, independent review, tests, signed immutability,
  reproducible history, controlled rollout and rollback.
* It is additive. Choosing the other direction would have meant re-labelling the standard's central requirement a deviation.

## Consequences and what this change does NOT do
* **`tax-rules-svc` and `tax-determination-svc` are NOT modified.** Rewiring live consumers and migrating production rate data is a
  coordinated cross-service change with its own risk, and was out of scope. Until it happens there are two sources of rates
  (the legacy editable rows and the governed packs); **only the pack path satisfies ZS-JUR-001**, and the legacy path remains a known
  non-conformance.
* **Recommended migration path (not started):** (1) re-author each active `tax_rules` row as a parameterised rule version in a pack, with
  sources and an approved interpretation, and certify and release it; (2) change `tax-determination-svc` to call
  `/v1/regulatory-calculations:execute` (or resolve the rule and apply the parameters) instead of reading `tax-rules-svc`;
  (3) run both in parallel and compare outcomes on real traffic; (4) freeze `tax-rules-svc` read-only, then retire it. Steps 2-4 need
  the owning teams.
* **Limits of the engine:** only flat and marginal-band rates with one rounding step. Not covered: place-of-supply and situs logic,
  reverse charge, exemptions and their evidence hooks, compound taxes, per-line versus per-document rounding, currency-specific
  precision, caps and floors, and withholding families. Each would be a new closed family (with a schema, engine, tests and coverage
  rules) decided with Tax Engineering, not a free-form formula.
* **Currency** is echoed, not validated (no currency registry exists); rounding scale is the rule's, not derived from the currency.

## Reversal conditions
Revisit if Tax Engineering decides the rule DSL / decision-table technology (s38) and wants parameters expressed in it: `parameters`
is a versioned envelope with a `family` discriminator, so a new family can be added without touching existing rules or artifacts.
