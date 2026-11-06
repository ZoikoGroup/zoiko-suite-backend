# jurisdiction-rules-svc: spec deviations

Where this service's behaviour differs from a specification, and why (implementation plan §2 R15).

## ZS-API-001 §12: the `validate-action` 409 is a decision, not a problem

`POST /v1/validate-action` answers `409` with a `ValidateActionConflictResponse` (`application/json`) when the
rule set is `conflicted` (equal-precedence rules overlap) or `insufficient` (no rule in the ancestor chain).
That body is the evaluation result callers branch on, not an error report, so it keeps its own schema
rather than becoming an RFC 9457 problem. Every other 4xx/5xx from this service is
`application/problem+json` (the probe body on `/readyz` aside). Recorded 27 Oct 2026 in the API-2 review.

## ZS-JUR-001 s9: jurisdiction decisions from entity facts (JUR-1)

`POST /v1/jurisdiction-decisions:resolve` rests on the entity's authorised assignments plus its establishments (ORG-6) and registrations (ORG-7), projected from
tenant-entity-registry-svc events (`establishment.*`, `registration.*`, `address.created`/`superseded`; migration 000032). Choices where the spec is silent:

- **Facts select, never add.** A named establishment or registration must exist for the entity, be in force on the date and point at a jurisdiction the entity is authorised
  for; it then selects among the authorised ones as a claim does. Nothing is selected automatically because a registration happens to exist.
- **Missing facts are 422, not 409.** Several authorised jurisdictions and nothing selecting is `INSUFFICIENT_FACTS` (422) with `missing_facts` naming what would select.
  This replaces the earlier 409 `AMBIGUOUS_JURISDICTION`, which the engine no longer returns. A named fact that cannot be used is `FACT_NOT_USABLE` (422); facts that disagree
  are `FACTS_CONFLICT` (409).
- **Dates, not instants.** The registry records establishment and registration changes by date, so a decision uses the state on the UTC date of `effective_at`. A registration is
  in force through its expiry date. The registration start is taken from the status event that made it ACTIVE.
- **An establishment's jurisdiction is its address country**, matched to a jurisdiction code exactly (case-insensitive). A subdivision code does not match its country. A
  superseded address version carries no country, so the establishment is unusable until a later version is seen.
- **Counterparty and transaction facts are recorded, not used.** Place-of-supply rules do not exist. No counterparty party id is accepted: no party projection is held, and an
  unverified id would look like evidence.
- **Registrations held by a party** (not a legal entity) are not projected.
- **The ledger holds digests.** The recorded request and the evidence chain carry a digest of each establishment or registration id, never the id.
