# ZS-JUR-001 Wave 6: payroll parameters, retention rules, accounting and reporting mappings

Builds on Waves [0](jurisdiction-pack-wave0.md) to [4](jurisdiction-pack-wave4.md) and [7](jurisdiction-pack-wave7.md). Implemented in
`jurisdiction-rules-svc` (migrations `000012`, `000013`). Follows ZS-JUR-001 s17, s18, s19, s37 and JUR-NEG-12.

## Decision: three more closed parameter families, not three new registries
The tax half of Wave 4 put typed `parameters` on the governed rule module ([decision](jurisdiction-pack-decision-rule-parameters.md)). Wave 6
extends the same envelope with `AMOUNT`, `RETENTION` and `MAPPING` instead of creating retention and mapping tables. Provenance, independent
publication, immutability after DRAFT, compile checks, signing, certification, rollout and rollback are inherited unchanged, and no new module type
travels in the manifest. **Departure from the document:** s18 and s19 describe their own registries; the rule-module form was chosen because it gets the
same guarantees for less surface. Revisit if a registry with its own lifecycle is required.

## 6A Payroll statutory parameters (s17)
* Rules in the `PAYROLL` domain carry a class (`INCOME_WITHHOLDING`, `EMPLOYEE_CONTRIBUTION`, `EMPLOYER_CONTRIBUTION`, `ALLOWANCE_OR_THRESHOLD`, `STATUTORY_PAY_LEAVE`;
  compiler `JUR-C091`) and a family: `TAX_RATE`, `TAX_BANDS` or the new `AMOUNT` (a stated value with a unit: per day/week/month/year/pay period or lump sum).
* `POST /v1/payroll-statutory-parameters:resolve` returns every payroll parameter in force on a pay date as one set (typed parameters, class, sources, content digest,
  pack release), ordered by class then code, with a digest over the set. Optional class filter and pinned pack replay.
* **Ambiguity returns no partial set.** If any rule code is ambiguous, the answer is `AMBIGUOUS` with no items: a payroll run must never apply some of the parameters.
* Evidence kind `PARAMETER_SET`, anchored on the set digest (and on the pack release when one pack supplies the whole set).
* Payroll **calculation** stays with the payroll product. An `AMOUNT` rule offered to the calculation endpoint is refused as `PARAMETER_ONLY`.
* Certification: a golden case must pin every parameter-only rule's value (`JUR-T030`).

## 6B Retention rules (s18)
* Domain `RECORDS_RETENTION`, rule code = the record class. Family `RETENTION`: `record_class`, `trigger` (CREATION, PERIOD_END, FILING_DATE, CONTRACT_END,
  EMPLOYMENT_END, EVENT), `minimum` (years 0-100, months 0-11, days 0-365), optional `format_requirement`, `legal_hold_override` (must be true), `destruction_rule`
  (must be `ELIGIBILITY_ONLY`). The validator refuses anything else.
* `POST /v1/retention-rules:resolve`: the rule in force on `trigger_date` applies; the answer is the statutory minimum (ISO 8601), `retain_until` (months clamp to the end of
  the month, then days), format requirement and an explanation. A caller-stated `trigger` that differs from the rule's is `TRIGGER_MISMATCH`.
* **A tenant policy may extend, never shorten** (JUR-NEG-12): `tenant_minimum` shorter than the statutory minimum is `TENANT_POLICY_TOO_SHORT`; longer is reported as the effective retention.
* **This service deletes nothing.** It decides eligibility; a legal hold always overrides.
* Evidence kind `RETENTION`; a resolved answer must be anchored on pack release, artifact digest and rule (database check).

## 6C Accounting and reporting mappings (s19)
* Domain `ACCOUNTING_MAPPING`, rule code = the mapping name. Family `MAPPING`: `mapping_type` (TAX_CODE_TO_ACCOUNT_CLASS, STATUTORY_LINE_TO_METRIC, REPORTING_TAXONOMY,
  DISCLOSURE_REQUIREMENT, CONTROL_ACCOUNT_CLASS) and up to 500 entries `{from, to, note?}`; a `from` may appear once.
* `POST /v1/accounting-mappings:resolve` returns one mapping (`from`) or the whole set. An unmapped code is `MAPPING_ENTRY_NOT_FOUND`: nothing is guessed.
* **Never posts.** The Accounting Kernel stays the only posting authority. Evidence kind `MAPPING`.

## Compiler and harness
* `JUR-C091` payroll rule needs a class. `JUR-C092` RETENTION only in `RECORDS_RETENTION` and MAPPING only in `ACCOUNTING_MAPPING`. `JUR-C093` a records-domain rule needs parameters.
* `JUR-T030` (parameter-only rules): a GOLDEN rule case resolves the rule and states `expect.parameter_amount`: the amount, the ISO retention duration (`P6Y`) or the number of mapping entries.
* Parameter-only families are exempt from the calculation coverage rules (T020-T023).

## Authorization (demo seed bundle `JURISDICTION_RESOLVER_CALLER`)
`PAYROLL_PARAMETERS_RESOLVE`, `RETENTION_RULE_RESOLVE`, `ACCOUNTING_MAPPING_RESOLVE`. Authoring uses the existing rule actions.

## Assumptions and what is not built
* **Retention across the jurisdiction chain:** the most specific rule in force is used; the document's "strictest applicable minimum across the chain" is **not** implemented.
* **Mapping coverage** is pinned by entry count plus the rule content digest, not by one test case per entry. A per-entry lookup case kind would be a harness change.
* No retention schedule per tenant, no destruction workflow, no legal-hold registry (the Data Governance standard owns those); no ledger posting; no disclosure generation.
* Payroll periodization (turning an annual allowance into a pay-period figure) is the payroll engine's job; the unit states what the number means.
* **Migrations:** apply `000012` and `000013` before the new binary. Their down scripts re-add the narrower evidence-kind check as `NOT VALID` because evidence is append-only and written rows cannot be removed.
* Still open for ZS-JUR-001: Wave 8 (Waves 5 and 8 are in jurisdiction-pack-wave5.md and jurisdiction-pack-wave8.md).

## Verification
Unit tests: duration arithmetic (month-end clamping, leap years), parameter validation tables, tenant-extension rule, mapping uniqueness, AMOUNT and class validation, JUR-C091, JUR-T030.
Integration (embedded Postgres 16, `internal/registryit`): payroll set through the full pipeline (tax-year boundary instant, class filter, evidence, idempotency, pinned replay),
`PARAMETER_ONLY` refusal, retention answers for two rule versions, tenant too short, trigger mismatch, idempotency, mapping lookup and not-found, API and compiler refusals (C091, C092),
and the 000005-000013 down/up round trip.
