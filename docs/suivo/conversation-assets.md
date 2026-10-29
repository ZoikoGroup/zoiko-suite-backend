# Suivo conversation assets (master prompt §6.4)

All copy below is pre-approved policy content and ships with the policy version. Templates live in `services/public-ai-svc/internal/policy/templates.go`. UI microcopy and starter chips live in `internal/policy/microcopy.go` and reach the widget via `GET /public-ai/v1/config`. Fixture knowledge is TEST-ONLY synthetic.

## Response templates T01–T30

| ID | Purpose | Implementation | Status | Route |
|---|---|---|---|---|
| T01 | Welcome | `TplWelcome` | CLARIFY | core_modules |
| T02 | Pricing: market needed | `TplClarifyMarket` (+ `clarification` options) | CLARIFY | — |
| T03 | Pricing: plan needed | `TplClarifyPlan` | CLARIFY | pricing |
| T04 | Pricing / trial answer | verified claim statements verbatim | ANSWERED | pricing / signup_or_trial |
| T05 | No evidence | `TplAbstainNoEvidence` | ABSTAIN | intent default |
| T06 | Stale / revoked / expired | `TplAbstainStale` | ABSTAIN | intent default |
| T07 | Claim conflict | `TplAbstainConflict` | ABSTAIN | intent default |
| T08 | Market not covered | `TplAbstainCoverage` | ABSTAIN | demo |
| T09 | Two failed clarifications | `TplClarifyGiveUp` | ABSTAIN | support |
| T10 | Out of scope | `TplOutOfScope` | ABSTAIN | support |
| T11 | Outage / kill switch / fallback | `TplUnavailable` | UNAVAILABLE | support |
| T12 | Own records (X01) | `ExceptionTemplates[X01]` | REFUSED_ACTION | support |
| T13 | Pay / approve / submit (X02) | `ExceptionTemplates[X02]` | REFUSED_ACTION | support |
| T14 | Pasted credentials / PII (X03) | `ExceptionTemplates[X03]` | REFUSED_ACTION | support |
| T15 | Personal tax / legal / employment advice (X04) | `ExceptionTemplates[X04]` | REFUSED_ACTION | core_modules |
| T16 | Security / breach report (X05) | `ExceptionTemplates[X05]` | HANDOFF | trust |
| T17 | Prompt injection (X06) | `ExceptionTemplates[X06]` | REFUSED_ACTION | — |
| T18 | Other Zoiko products (X07) | `ExceptionTemplates[X07]` | ABSTAIN | — |
| T19 | Guarantees / commitments (X08) | `ExceptionTemplates[X08]` | ABSTAIN | demo |
| T20 | Demo (I08) | `RouteTemplates[I08]` | HANDOFF | demo |
| T21 | Sign-in (I22) | `RouteTemplates[I22]` | HANDOFF | support |
| T22 | Customer support (I23) | `RouteTemplates[I23]` | HANDOFF | support |
| T23 | Human handoff (I24) | `RouteTemplates[I24]` | HANDOFF | support |
| T24 | Competitor comparison (I26) | `RouteTemplates[I26]` | ABSTAIN | core_modules |
| T25 | Billing information (I28) | `RouteTemplates[I28]` | HANDOFF | support |
| T26 | Enterprise inquiry (I09) | grounded answer + demo route | ANSWERED / ABSTAIN | demo |
| T27 | Connectors (I10) | integration-status claim, governed `status_label` | ANSWERED | core_modules |
| T28 | Certifications (I12) | certification claim only; HOLD never claimed | ANSWERED / ABSTAIN | trust |
| T29 | Residency (I14) | residency claim or abstain | ANSWERED / ABSTAIN | trust |
| T30 | Accessibility feedback (I27) | grounded answer + support route | ANSWERED / ABSTAIN | support |

X09 (unverified certification claims) is handled by the certification claim lookup (T28). X10 (abuse/flooding) is handled at the edge by rate limits and spend ceilings, never by generation.

## Multi-turn scripts S01–S10

Each script is a test in `internal/acceptance/scripts_test.go`, run through the public HTTP API.

| ID | Script | Expected |
|---|---|---|
| S01 | Price with no market | CLARIFY, `clarification.options` = markets + GENERAL_OVERVIEW, `status_label` MARKET_DEPENDENT, no price |
| S02 | …then pick a market (chip or typed "the UK") | ANSWERED with that market's claim verbatim, `market_code` set, STATUS_QUALIFIED |
| S03 | …then "Show general overview" | answer without market-scoped prices, PROCESS_EXPLANATION |
| S04 | Plan switch mid-chat (US → UK) | UK price only; the US figure is discarded |
| S05 | "Are you SOC 2 certified?" (claim on HOLD) | never claimed; trust route |
| S06 | "Show me my overdue invoices" | REFUSED_ACTION, no citations, HANDOFF_UNKNOWN |
| S07 | Prompt extraction | REFUSED_ACTION, no prompt text |
| S08 | Pasted password | not echoed, not sent to the model |
| S09 | Integration status (ACTIVE vs HOLD) | governed label (Implemented); HOLD never labelled Available |
| S10 | Demo request with an e-mail | demo route; e-mail not echoed; nothing booked |

## Banned absolutes (correction table)

Enforced by `internal/validate` unless a verified claim statement contains the exact phrase:

- guarantee / guaranteed, fully compliant, 100% / 100% compliant, unlimited, instant
- SOC 2 certified
- 24/7, round-the-clock
- works with every / works with all
- action claims: I booked you, I have / I've booked, ticket created, I opened your account, automatically enrolled, an agent will call, I have / I've submitted, created, sent, scheduled

## Escalation matrix (route keys only; never an invented e-mail, phone or ticket)

| Need | Route key |
|---|---|
| Demo / sales / enterprise / unsupported market | demo |
| Support, sign-in, billing, human contact | support |
| Security report, privacy, residency, certifications | trust |
| Pricing and plans | pricing |
| Trial (only when trial eligibility is VERIFIED) | signup_or_trial |
| Regional payroll / tax / legal coverage | demo (coverage confirmation) |
| Integration / migration | core_modules / demo |
| Accessibility feedback | support |

## Page-aware chips

`policy.Chips` maps each page_context (home, pricing, core_modules, demo, support, other) to 4–6 starters. The six base chips are Explore modules, Pricing & trial, Book a demo, Security & trust, Integrations and Get support.
