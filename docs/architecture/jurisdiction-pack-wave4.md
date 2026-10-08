# ZS-JUR-001 Wave 4: regulatory calendar, obligation due dates and tax parameters

Builds on Waves [0](jurisdiction-pack-wave0.md), [1](jurisdiction-pack-wave1.md), [2](jurisdiction-pack-wave2.md),
[3](jurisdiction-pack-wave3.md) and [7](jurisdiction-pack-wave7.md). Implemented in `jurisdiction-rules-svc` (migration `000010`).
Follows ZS-JUR-001 s15, s16, s25, s26, s27 and JUR-NEG-13, 21, 26.

## What Wave 4 covers
ZS-JUR-001 Wave 4 is "Tax rule modules, obligations, authority/business-day calendars". Both halves are built. The **calendar and obligation half**
is described first. The **tax half (rates, thresholds, rounding, exact calculation)** was blocked on a doctrine conflict; it was resolved by
[jurisdiction-pack-decision-rule-parameters.md](jurisdiction-pack-decision-rule-parameters.md) (ZS-JUR-001 governs; parameters live in the pack
rule modules) and is described in the last section of this document.

## The model
* **Calendars** (`regulatory_calendars`, immutable `regulatory_calendar_versions`, `regulatory_holidays`): per authority, effective-dated
  versions with an IANA time zone, a weekend definition (0 = Sunday .. 6 = Saturday, **required and never defaulted**), an optional
  authority-local cutoff time and a holiday list. An amended calendar is a **new version**, never an edit (JUR-NEG-13).
* **Obligation rules** (`obligation_rules`, immutable once published): period basis (MONTHLY, QUARTERLY, ANNUAL, EVENT_DRIVEN,
  PAYROLL_CYCLE), anchor (PERIOD_END, EVENT_DATE, REGISTRATION_DATE, ANNIVERSARY), offset months and days (optionally "to the end of that month"),
  business-day adjustment (NONE, NEXT, PREVIOUS), the calendar to use, effective window, extension policy (allowed, cap, evidence required),
  cutoff, and an escalation owner and SLA.
* Both follow the same **provenance** invariants as rules (s3): at least one **reviewed, non-superseded source**; obligation rules also need a
  regime and an **APPROVED interpretation** (JUR-NEG-18). They are drafted, sourced, and **published by a different principal** (service and
  database both refuse self-publication). The database makes PUBLISHED rows, their holidays and their sources immutable.

## The calculation (`domain.CalculateDueDate`, a pure function)
Anchor date, plus offset months (clamped to month end; or moved to the end of that month when `offset_to_month_end`), plus offset days, plus a granted
extension, then the **business-day adjustment** using the calendar version in force (weekends and holidays are skipped, each skipped date is
explained), then the **authority-local end of day or cutoff** converted to a UTC instant. **That order is an assumption** the document leaves open
(it only says "offset", "adjustment", "extension" and "cutoff"); it is stated in the API description and in every result's explanation.

* Facts are validated against the period basis (a MONTHLY `period_end` must be a month end; QUARTERLY a calendar quarter end). A 29 February anniversary clamps to
  28 February in a common year.
* **Extensions** are accepted only where the rule permits them (s15: only where legally supported), within the cap, and with an evidence reference when required.
  Otherwise `EXTENSION_NOT_PERMITTED`.
* **Calendar version choice:** among versions of the calendar effective on or before the extended date, the highest. Results record the calendar version used.
* **Outcomes:** `DUE_DATE_CALCULATED`, `NO_OBLIGATION_RULE`, `UNSUPPORTED_JURISDICTION`, `INVALID_FACTS`, `EXTENSION_NOT_PERMITTED`, `NO_CALENDAR`, `AMBIGUOUS`.
  Overlapping obligation rules are never resolved by guess (obligation rules have no precedence); the pack compiler refuses to build such a pack.

## They travel in signed packs
The manifest gains `calendar_modules` (calendar version ids) and `obligation_modules` (obligation rule ids). The **compiler** (new checks `JUR-C070`..`C088`)
requires every module to be **PUBLISHED**, in the pack's scope, sourced and reviewed, interpreted and approved; an obligation's calendar must be **among the pack's
own calendar modules** (the artifact is self-contained, no live lookup); overlapping windows of the same obligation code are an error. A pack with only calendar and obligation modules
is a valid pack. The artifact embeds the modules with content digests.

The **certification harness** gets obligation cases (`obligation_code` plus facts; expected outcome and `due_date`), and three new coverage rules for each obligation rule:
`JUR-T010` a GOLDEN case that calculates it; `JUR-T011` (when it adjusts to a business day) a BOUNDARY case whose date was **actually moved off a weekend or holiday** (JUR-NEG-26);
`JUR-T012` a NEGATIVE case in which a not-permitted extension is refused. A wrong expected date fails the case (a test author's arithmetic error is caught).

## Runtime: `POST /v1/obligation-calculations:calculate`
Same pack selection, ring and region gating, verification and **fail-closed** behaviour as rule resolution (they share `Resolver.pick`), answering from the artifact alone. Facts only in;
the server chooses the pack, obligation rule version and calendar version. Optional `as_of` adds an `overdue` verdict against the due instant. Every answer except 503 is recorded in the
decision evidence ledger (`decision_kind = OBLIGATION`) with the pack release, artifact digest, rule version and sources; a pinned `pack_ref` + `pack_version` reproduces a past calculation exactly.
**Holiday amendment (JUR-NEG-13):** the amended calendar ships as a new pack release; new calculations use it, earlier evidence is untouched, and a pinned replay returns the original date.
`jurisdiction.calendar.changed` (the event name the service's original specification reserved) is published when a calendar version is published.

## Authorization
Calendars: `REGULATORY_CALENDAR_CREATE`, `REGULATORY_CALENDAR_VERSION_CREATE|SET_SOURCES|PUBLISH`. Obligations: `OBLIGATION_RULE_CREATE|SET_PROVENANCE|PUBLISH`. Callers:
`OBLIGATION_CALCULATION_CALCULATE` (demo seed bundle `JURISDICTION_RESOLVER_CALLER`). Authoring and publication are separate actions **and** the service forbids one principal doing both for the same record.

## Decisions the document leaves open, and what this wave assumed
* **Order of operations** (above). Real authorities differ; the explanation shows every step so a reviewer can check it.
* **Calendar version selection** by effective date, highest version wins. No "supersedes" link between versions.
* **Period alignment** is checked only for MONTHLY and QUARTERLY (calendar months and quarters). Fiscal quarters and non-calendar monthly periods must be modelled as ANNUAL/EVENT_DRIVEN with explicit dates.
* **Cutoff semantics:** the cutoff replaces end of day on the due date; it does not roll a late submission to the next business day.
* **Escalation** is data on the result (`escalation_owner`, `escalation_sla_hours`); nothing escalates.

## Not built
* **Tax rule families beyond flat and marginal-band rates** (place of supply, reverse charge, exemptions, compound taxes, withholding, caps and floors); see the tax section below.
* **Obligation instances and tracking** (s27 RegulatoryObligation per entity; the `created` / `due-date-calculated` / `overdue` events): the service calculates and records, it does not hold a per-entity
  obligation list or detect overdue items. The existing `obligations-svc` and `filing-tracker-svc` are not integrated. `as_of` gives a point-in-time verdict only.
* **Registrations, licences, schemes and threshold monitors** (s16), and **registration-driven applicability** (which obligations apply to which entity).
* Rolling calendars generated from rules (e.g. "last Monday of May"); holidays are listed dates. Half-day closures and authority-specific cutoff exceptions.
* Provenance-driven **drift detection** for calendars, and source-change linkage to calendar versions beyond the existing notice intake.
* Deploy order: apply migration 000010 before the new binary; the registry routes are always mounted.

## Verification
Unit tests of the engine (clamping, month-end offsets, weekends and holidays, Friday-Saturday calendars, version selection, extensions, fact validation, leap-day anniversaries, time zones and DST, cutoff,
validation tables, rule selection and ambiguity), of the compiler checks, artifact round trip and harness obligation cases and coverage, and of the resolver (verified answer, explicit refusals,
cross-pack conflict, fail-closed on a shifted holiday, pinned replay and version selection). Integration suite (`internal/registryit`, embedded Postgres 16): authoring, sourcing and independent publication with the
database guards attacked in SQL; malformed definitions; the full pack pipeline through compile, sign, test, review, certify, release; derived, explained and recorded due dates; the holiday-amendment scenario;
compile refusals; harness coverage gaps blocking certification; the 000005-000010 down/up round trip.

---

# Tax half: typed rule parameters and exact calculation

**Decision** (full record: [jurisdiction-pack-decision-rule-parameters.md](jurisdiction-pack-decision-rule-parameters.md)): ZS-JUR-001 governs. A rule's
rates, band limits and rounding live in a NEW typed field `parameters` (migration `000011`), not in `rule_payload` (which keeps its meaning) and not in editable rows.

* **Schema (closed, no formula language):** `TAX_RATE` (flat) and `TAX_BANDS` (marginal bands, limits inclusive and strictly increasing, last band unbounded), each with one
  `rounding` step (HALF_UP, HALF_EVEN, UP, DOWN; scale 0 to 6). Every number is a **decimal string**; a JSON number is refused (JUR-NEG-25).
* **Exact arithmetic:** `domain.CalculateTax` uses rational arithmetic (`math/big`), never binary floating point, and rounds **once** to the declared scale and mode at the end.
  Amounts are non-negative decimal strings with at most 12 decimal places; `"-5"`, `"1e3"`, `"1."` are refused as `INVALID_FACTS`. Credits are not guessed: they need their own basis.
* **Frozen with the rule:** editable only while DRAFT (API `PUT /v1/admin/rules/{id}/parameters`); a trigger forbids any change afterwards. The compiler re-validates them
  (`JUR-C090`) so a bad value written straight into SQL still cannot be packaged, and snapshots them into the artifact with a content digest.
* **Certification coverage** for any rule that carries parameters: `JUR-T020` a GOLDEN calculation case; `JUR-T021` a BOUNDARY case at exactly each finite band limit;
  `JUR-T022` a BOUNDARY case whose result is actually rounded; `JUR-T023` a NEGATIVE case in which an unsupported amount is refused. A calculation case resolves the rule like a
  rule case (jurisdiction, domain, code, instant) and adds `taxable_amount` and the expected `tax_amount`; a wrong expected tax fails the case.
* **Runtime:** `POST /v1/regulatory-calculations:execute` resolves the rule from the verified released pack (same fail-closed, ring and region rules) and applies its parameters.
  The response carries the resolution (pack release, rule, sources), the tax, the unrounded value, whether rounding changed it, and every step. Each answer is recorded as evidence
  (`decision_kind = CALCULATION`). A rule with no parameters is `NO_PARAMETERS` (no rate is ever guessed). **A rate quietly altered in storage cannot be served:** the artifact
  digest and signature fail and the answer is a 503, not a wrong tax (an integration test changes a stored rate with the guards off and proves it).
* **Not built:** place of supply and situs, reverse charge, exemptions and their evidence hooks, compound or inclusive-of-tax maths, per-line versus per-document rounding, currency precision,
  caps and floors, withholding and payroll families. Each would be a new closed family decided with Tax Engineering. `tax-rules-svc` and `tax-determination-svc` are **not modified**; the
  migration path is in the decision record. Currency is echoed, not validated (no registry exists).
