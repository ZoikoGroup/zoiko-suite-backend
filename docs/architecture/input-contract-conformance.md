# Canonical Input Contract — conformance and gaps

What ZS-ARCH-SVC-001 v2.0 (*Global Service Requirements & Input Contract
Catalogue*) requires as service inputs, what the platform now implements, and
what cannot be implemented yet with the reason.

Source document: `zoiko-suite-frontend-platform/docs/ZoikoSuite_Global_Service_Requirements_Input_Contract_Catalogue.docx`
Assessed against: `services/` at 87 Go modules.

---

## 1. What was implemented

§4 defines a common request envelope that "every service-specific input contract
inherits". That is the one input specification in the document that applies to
every service already built, so it is what was implemented.

| Artefact | Location |
| --- | --- |
| Authoritative package | `services/_contract/envelope/` |
| Vendoring script | `services/_contract/rollout.sh` |
| Vendored copy | `services/<svc>/internal/envelope/` (86 services) |
| Per-service policy | `services/<svc>/internal/envelope/contract.go` (generated) |
| Console side | `zoiko-suite-frontend-platform/lib/api/envelope.ts` |

Services are separate Go modules with no shared module, so the package is
vendored rather than imported — the pattern the repo already uses for
`internal/middleware/tenant.go`. Edit `services/_contract/envelope/` and re-run
`rollout.sh`; never edit a vendored copy.

### Enforcement

`ZS_ENVELOPE_ENFORCEMENT` selects the mode, defaulting to `write-strict`.

| Mode | Behaviour |
| --- | --- |
| `write-strict` (default) | Refuse material state changes missing a mandatory field. Admit reads, marked `X-Envelope-Contract: violated`. |
| `strict` | Refuse reads too. The doctrinal end state, per service, once its callers are migrated. |
| `observe` | Never refuse; parse, propagate, log. A migration state, not a resting state. |

An unrecognised value falls back to `write-strict`, so a typo cannot silently
disable the control.

Refusals name every unmet field at once, structurally:

```json
{
  "error": "envelope_incomplete",
  "detail": "canonical input contract violated: idempotency_key, request_id, source_channel",
  "service": "general-ledger-svc",
  "violations": [
    { "field": "request_id", "header": "X-Request-Id", "reason": "mandatory: request tracing" }
  ]
}
```

Missing `tenant_id` or `actor_subject_id` answers **401**, not 400 — those two
headers are set by gateway-auth-svc after it verifies the signed identity
envelope, so their absence means the request never passed authentication.
Everything else is **400**.

---

## 2. §4 field-by-field status

`U`ser / `X`ternal / `S`erver / `D`erived / `A`pproval / `E`vidence are the §5
provenance classes.

| § 4 field | Class | Header | Status | Notes |
| --- | --- | --- | --- | --- |
| `tenant_id` | S | `X-Tenant-Id` | **Enforced** | Was present in 75/87 services, defaulting to `"default"` on absence in most. Now mandatory everywhere; no default. |
| `actor_subject_id` / `workload_id` | S | `X-Principal-Id` / `X-Workload-Id` | **Enforced** | Either satisfies the slot. |
| `legal_entity_id` | S | `X-Legal-Entity-Id` | **Enforced** on the 69 entity-scoped services | Validated against tenant-entity-registry-svc, incl. cross-tenant refusal. |
| `book_id` / `reporting_basis` | S | `X-Book-Id` / `X-Reporting-Basis` | **Carried, not enforced** | See gap G-1. |
| `operation` | U | `X-Operation` | **Enforced** | Falls back to `METHOD /path`, which *is* the requested command in REST. |
| `request_id` | S | `X-Request-Id` | **Enforced** | Echoed on the response, including on refusals. |
| `correlation_id` | U | `X-Correlation-ID` | **Enforced** | Was present in 40/87. |
| `causation_id` | S | `X-Causation-Id` | **Carried** | Conditional in §4. |
| `idempotency_key` | U | `Idempotency-Key` | **Enforced on writes** | INV-08. A service cannot opt out — leaving the policy field unset still yields `RequiredOnWrite`. |
| `source_channel` | S | `X-Source-Channel` | **Enforced** | Allow-listed to the seven §4 values; an unknown channel is refused, not coerced. |
| `source_system` | X | `X-Source-System` | **Enforced conditionally** | Mandatory when `source_channel` is `import` or `integration`, derived from the channel rather than declared per service. |
| `external_reference` | X | `X-External-Reference` | **Carried** | Conditional in §4. |
| `occurred_at` | X | `X-Occurred-At` | **Carried, format-enforced** | RFC3339 only. A malformed value is refused, never silently dropped to "absent". |
| `effective_at` | X | `X-Effective-At` | **Carried, format-enforced** | As above. |
| `timezone` | S | `X-Timezone` | **Carried + resolvable** | Resolved from `Tenant.primary_timezone`; a caller's own zone is a legitimate narrowing and is preserved. |
| `jurisdiction_context` | S | `X-Jurisdiction-Context` | **Resolvable + enforced** | Resolved from `LegalEntity.primary_jurisdiction_id`. A caller claim that disagrees is overridden and the conflict recorded. |
| `purpose_context` | U | `X-Purpose-Context` | **Enforced on 28 sensitive services** | Personal, bank, tax, payroll, privileged content. |
| `expected_version` | U | `X-Expected-Version` | **Carried** | Optimistic concurrency; no service consumes it yet. |
| `workflow_instance_id` | S | `X-Workflow-Instance-Id` | **Carried** | workflow-svc owns the value. |
| `approval_reference` | A | `X-Approval-Reference` | **Carried** | |
| `evidence_refs` | E | `X-Evidence-Refs` | **Carried** | Comma-separated; empty members dropped. |

"Carried" means parsed, propagated on the request context, echoed and available
to handlers, but not itself a refusal condition.

---

## 2b. Per-service inputs (§9) — implemented

Beyond the §4 envelope, each §9 row lists the service's own required inputs.
This section tracks those, service by service.

### general-ledger-svc — ACC-03 / ACC-04 / ACC-05 ✅

§9.D ACC-03 requires: *"Book; entity; journal type; document/transaction date;
posting date; currency; debit/credit lines; accounts; dimensions; description;
source/evidence."*

| ACC-03 input | Before | Now |
| --- | --- | --- |
| entity, lines, accounts, description | present | unchanged |
| source/evidence | partial (`source_event_id`, `governance_decision_id`) | plus `evidence_refs`, unioned with the §4 envelope's `X-Evidence-Refs` |
| **journal type** | absent | **required** — closed set of 7, refused outside it |
| **document/transaction date** | absent | **required** — ISO calendar date |
| **posting date** | absent | **required** — refused if earlier than transaction date |
| **currency** | absent | **required** — ISO 4217 shape check |
| **dimensions** | absent | **carried** per line as JSONB (REF-08 missing, so unvalidated) |
| **book** | absent | **carried** (REF-06 missing — gap G-1) |

Migration `000006_acc03_journal_inputs`. Pre-existing rows are backfilled with
`journal_type = 'UNSPECIFIED'` and `currency_code = 'XXX'`, and the column
defaults are dropped immediately afterwards so no future insert can silently
acquire a value nobody chose. `UNSPECIFIED` is refused on a new journal.

Two decisions worth stating:

- **Transaction and posting dates are separate fields.** A supplier invoice
  dated the 28th, received and posted on the 3rd of the next month, is one
  document with two dates. Collapsing them moves it into the wrong period in one
  direction or misstates the document in the other.
- **They are dates, not timestamps.** Two postings made on the same business day
  in London and Auckland serialise to timestamps either side of midnight UTC,
  and any period boundary computed from them puts the same day's work in two
  periods.

A reversing journal inherits the original's book, currency, transaction date,
evidence and per-line dimensions — a reversal is not an independent business
event, and must net the original out. Its type is `REVERSAL` and its posting
date is today.

Console: journal type, both dates and currency are on the intake form with the
posting-before-transaction check shown beside the field; the register shows type,
currency, posting date, book, and names a pre-contract entry as such rather than
rendering a blank.

Not done for GL, and why: **ACC-04 posting-rule / account-mapping and ACC-05
account indexes** need ACC-01 Chart of Accounts and ACC-02 Account Mapping,
neither of which exists. `account_code` stays an unvalidated string.

### tax-determination-svc — TAX-03 ✅

§9.J TAX-03 requires: *"Seller/buyer; establishments; ship-from/to; supply
location/date; product/service classification; taxable amount; currency;
exemption facts; B2B/B2C facts."* Server-resolved: *"Registrations; jurisdiction
pack; place-of-supply rules; rates; rounding."*

Before this pass the service took a jurisdiction, a category and an amount and
applied a rate — meaning **the caller had already decided the question the
service exists to answer**.

| TAX-03 input | Before | Now |
| --- | --- | --- |
| taxable amount, currency | present | currency now shape-validated |
| **seller / buyer** | absent | **required** |
| **establishments** | absent | **carried** (ORG-08 missing) |
| **ship-from / ship-to** | absent | **carried + validated** against jurisdiction-rules-svc |
| **supply location** | absent | **required + validated**; basis recorded |
| **supply date** (tax point) | absent | **required**, ISO date |
| **product/service classification** | absent | **required** — free-text code plus a closed `supply_kind` (GOODS / SERVICES / DIGITAL_SERVICES) |
| **B2B/B2C facts** | absent | **required** — `supply_type` B2B/B2C/B2G; B2B additionally requires the buyer's registration |
| **exemption facts** | amount only | **reason required** whenever `exempt_amount > 0` (INV-10), plus certificate ref |
| **Registrations** (server-resolved) | absent | **resolved** from tenant-entity-registry-svc tax identity bundles, effective-dated to the supply date |
| jurisdiction pack, rates | present | unchanged (tax-rules-svc) |
| place-of-supply rules | absent | **cannot be done** — see below |
| rounding | absent | **cannot be done** — REF-07 missing |

Migration `000003_tax03_determination_inputs`.

**Two findings worth calling out.**

*Place of supply is asserted, not derived.* §9.J expects place-of-supply rules
to derive it from establishments, supply kind and B2B/B2C facts. Those rules are
jurisdiction-pack data (TAX-02) and no pack carries them. Rather than fake the
derivation, the service takes the jurisdiction as an input and records
`place_of_supply_basis = 'CALLER_ASSERTED'` on every determination — so the
evidence says plainly that no rule engine decided it. When packs carry the rules
the basis becomes `RULE_DERIVED`, and disagreement between the two is auditable.
The console shows "caller-asserted" for the same reason.

*`JURISDICTION_RULES_URL` was configured platform-wide but pointed at the wrong
port.* jurisdiction-rules-svc listens on **8082**; 11 compose entries set 8081.
Nothing read the variable, so the error was latent. This service now reads it,
so the port is corrected in `deployments/docker-compose.yml` and in the service
default.

*Not done, and why:* rounding policy needs **REF-07 Accounting Policy**;
establishment validation needs **ORG-08**; a single party master that both a
selling entity and a customer resolve against does not exist, so `seller_party_id`
and `buyer_party_id` are recorded unvalidated.

*Pre-existing defect, flagged not changed:* `internal/rules/client.go` falls back
to a **0% rate** when tax-rules-svc is unreachable, producing a zero-tax
determination rather than failing closed. `snapshotTaxRule` already refuses to
snapshot it, so the record is honest about being a fallback — but the tax figure
is still zero. That is a fail-open on a governed calculation, sitting next to the
two fail-closed paths added here. Worth a decision.

---

## 3. Server-resolved inputs — what can actually be retrieved

§5 class S says the client "may request context but cannot override result", so
each server-resolved input needs a service that owns the fact. What exists:

| Input | Resolver | Endpoint | Status |
| --- | --- | --- | --- |
| `jurisdiction_context` | tenant-entity-registry-svc | `GET /v1/entities/{id}` → `primary_jurisdiction_id` | **Wired** |
| `timezone` | tenant-entity-registry-svc | `GET /v1/tenants/{id}` → `primary_timezone` | **Wired** |
| Tenant status / lifecycle (INV-01) | tenant-entity-registry-svc | `GET /v1/tenants/{id}` | **Wired**, allow-listed |
| Legal-entity status, cross-tenant check (INV-02) | tenant-entity-registry-svc | `GET /v1/entities/{id}` | **Wired** |
| Residency policy | tenant-entity-registry-svc | entity policy overrides tenant default | **Wired** |
| Default currency | tenant-entity-registry-svc | `default_currency_code` | **Wired** |
| Permissions / SoD | authorization-svc | `POST /v1/authorize` | Already wired per service |
| Policy version | policy-svc | `POST /v1/policies/evaluate` | Already wired per service |
| Rule pack version | jurisdiction-rules-svc | `GET /v1/jurisdictions/{id}/rule-pack` | Already wired per service |
| Retention class / legal hold | retention-registry-svc | `GET /v1/retention/resolve` | Already wired per service |
| Period status | financial-close-svc | `GET /v1/close/periods/status` | Already wired per service |

A resolver 5xx returns `ErrResolverUnavailable`, which callers must treat as
"cannot answer", never as "no" — the contract jurisdiction-rules-svc already set
for the platform.

---

## 4. Gaps — doc inputs that CANNOT be implemented, and why

### G-1 · `book_id` / `reporting_basis` — no issuing service

§4 makes it mandatory for accounting/reporting actions and INV-03 requires every
material posting to identify its book. **REF-06 Accounting Book / Ledger Basis
does not exist in `services/`.** Nothing can issue or validate a `book_id`.

Requiring one would force every caller to invent an identifier no service can
check — a posting claiming a basis nobody decided, which is precisely the
failure INV-03 exists to prevent. The envelope therefore *carries* `book_id`
today so callers can begin sending it, and the 17 accounting services generate
`BookID: NotRequired` with a pointer to this gap. Flipping them is a one-line
change per service once REF-06 ships.

Affected: general-ledger, accounts-payable, accounts-receivable, consolidation,
intercompany-accounting, financial-close, migration-integrity,
reporting-orchestration, metric-registry, corporate-tax, vat-gst,
withholding-tax, tax-determination, payroll-tax, treasury, bank-reconciliation,
reconciliation-intelligence.

### G-2 · §6.1 money fields — no reference-data services

| Doc field | Needs | Present? |
| --- | --- | --- |
| `functional_currency` | REF-02 Currency Registry | No |
| `exchange_rate`, `exchange_rate_type`, `exchange_rate_date`, `exchange_rate_source` | REF-03 Foreign Exchange Rates | No |
| `rounding_method` | REF-07 Accounting Policy | No |
| `presentation_currency` | REF-06 + REF-02 | No |

Separately, INV-04 requires exact decimal arithmetic. `general-ledger-svc` stores
`DebitAmount`/`CreditAmount` as `float64`, and `tax-determination-svc` stores
every monetary field as `float64`. That is a **data-model** defect rather than an
input-contract one, so it is out of scope here, but it blocks §6.1 conformance
and should be tracked.

### G-3 · Fiscal calendar / period — field exists, resolver does not

`LegalEntity` already carries `fiscal_calendar_id`, and `general-ledger-svc`
carries `fiscal_period` as a plain string with the in-code note *"no Fiscal
Calendar service exists yet"*. **REF-04 Fiscal Calendar** and **REF-05 Accounting
Period** are unimplemented, so a supplied period cannot be validated as open,
closed or even real.

### G-4 · Doc services with no implementation at all — 107 of 200

Their input contracts cannot be implemented because the service does not exist.
93 of the doc's 200 service IDs map onto the 87 built services.

| Domain | Missing | Notably |
| --- | --- | --- |
| REF Reference Data & Accounting Basis | 9 / 10 | REF-02…REF-10 — the entire basis layer; only REF-01 Jurisdiction Registry exists |
| ACC Core Accounting | 10 / 18 | ACC-01 Chart of Accounts, ACC-15 Trial Balance, ACC-16 Signed Financial Snapshot |
| AR Revenue & Receivables | 8 / 10 | AR-05 Electronic Invoice, AR-06 Credit/Debit Note, AR-08 Cash Application |
| AUD Audit & Assurance | 8 / 10 | AUD-01 Engagement through AUD-10 Audit Trail Export |
| BIZ Business Operations | 7 / 10 | BIZ-04 Forms, BIZ-05 Task/Case, BIZ-06 CRM |
| TAX Global Tax | 7 / 15 | TAX-07 Tax Ledger, TAX-11 E-Invoice Clearance, TAX-13 SAF-T |
| AP Procurement & Payables | 6 / 12 | AP-09/10/11 — the whole payment proposal→run chain |
| BNK Banking & Treasury | 6 / 10 | BNK-01 Bank Account, BNK-06 Payment Initiation |
| INV Inventory | 5 / 5 | none of the inventory domain exists |
| OPS Platform Reliability | 5 / 6 | OPS-01 Observability, OPS-04 Backup & Recovery |
| INT Integration Platform | 5 / 10 | INT-02 Event Bus, INT-03 Outbox/Inbox, INT-04 Webhook |
| PRJ Project Accounting | 4 / 4 | none of the project domain exists |
| ORG Identity & Party | 4 / 10 | ORG-04 Group & Ownership, ORG-10 Payee Master |
| FIN Planning & Performance | 4 / 7 | FIN-04 Variance Analysis, FIN-07 Board Pack |
| LEG Legal & Governance | 4 / 12 | LEG-02 Director Register, LEG-09 Legal Matter |
| AST Assets | 3 / 3 | none of the fixed-asset domain exists |
| REP Reporting | 3 / 7 | REP-01 Financial Statements, REP-03 XBRL |
| DATA Data & AI | 3 / 12 | DATA-02 Data Quality, DATA-04 Analytical Platform |
| WFP Workforce Boundary | 2 / 7 | WFP-05 Payroll Journal, WFP-07 Payroll Payment |
| SEC Security | 2 / 11 | SEC-03 Privileged Access, SEC-04 Encryption Policy |
| GOV Governance | 1 / 12 | GOV-02 GTRM — residency design exists in `docs/`, no service |
| COM Commercial | 1 / 5 | COM-04 Usage Metering |

The full 107-row list is in §6 below.

### G-5 · Built services with no doc counterpart — 10

These have no §9 row, so the doc specifies no inputs for them. They received the
§4 envelope like everything else, but nothing beyond it can be aligned.

`benefits-svc`, `capability-registry-svc`, `compensation-svc`,
`compliance-risk-scoring-svc`, `compliance-status-svc`, `decision-support-svc`,
`employment-contracts-svc`, `offboarding-severance-svc`,
`performance-review-svc`, `workforce-compliance-svc`

Most are HR services. §9.O deliberately scopes the platform to a *Workforce &
Payroll Financial Boundary* (WFP-01…07) — interfaces to Zoiko HR / Zoiko Payroll
rather than an HR system of record. Either the doc needs a section for them, or
they need re-framing as WFP interfaces.

### G-6 · `secret-vault-integration-svc` — pre-existing breakage

Does not compile, for reasons unrelated to the input contract. Its working tree
has `internal/domain/types.go`, `internal/config/config.go` and
`internal/store/pg_store.go` gutted relative to HEAD (1,251 deletions), leaving
`domain.SecretPolicy`, `domain.SecretLease` and `domain.CreateSecretPolicyParams`
referenced but undefined.

Its `go.mod` was separately invalid — it required
`github.com/jackc/pgx/v5/pgxpool`, which is a *package inside* the `pgx/v5`
module, not a module. That made the module unparseable, which is why the failure
had gone unnoticed. **Fixed** here; the gutted domain files are not, since
restoring them means reinstating a secret-lease domain well outside this scope.

### G-7 · Two services with no business API

| Service | Reason |
| --- | --- |
| `search-indexer-svc` | Exposes only `/healthz`, `/readyz`, `/metrics`. Nothing to gate; the envelope package exempts probes anyway. Vendored, not wired. |
| `search-client` | Library module, no HTTP server. Not vendored. |

---

## 5. Verification

| Check | Result |
| --- | --- |
| `services/_contract` unit tests | 32 pass, 0 fail |
| `go build ./...` across services | 86 / 87 (G-6) |
| `go vet` + `go test ./...` across services | 86 / 87 (G-6) |
| Services with middleware wired | 85 (76 via `main.go`, 9 via `handler.NewRouter`) |
| Frontend `tsc --noEmit` | clean |
| Frontend `next build` | clean |

Nine handler suites were brought onto the contract with a `withEnvelope` helper
(`internal/handler/envelope_contract_test.go`) that fills only absent fields, so
negative tests asserting on a wrong tenant or a replayed key still say what they
mean.

---

## 6. Appendix — the 107 unimplemented doc services

**GOV** GOV-02 ·
**ORG** ORG-04, ORG-08, ORG-09, ORG-10 ·
**REF** REF-02, REF-03, REF-04, REF-05, REF-06, REF-07, REF-08, REF-09, REF-10 ·
**ACC** ACC-01, ACC-02, ACC-06, ACC-07, ACC-08, ACC-09, ACC-10, ACC-12, ACC-15, ACC-16 ·
**AR** AR-01, AR-02, AR-03, AR-05, AR-06, AR-08, AR-09, AR-10 ·
**AP** AP-04, AP-07, AP-09, AP-10, AP-11, AP-12 ·
**BNK** BNK-01, BNK-03, BNK-04, BNK-06, BNK-07, BNK-10 ·
**AST** AST-01, AST-02, AST-03 ·
**INV** INV-01, INV-02, INV-03, INV-04, INV-05 ·
**PRJ** PRJ-01, PRJ-02, PRJ-03, PRJ-04 ·
**FIN** FIN-03, FIN-04, FIN-06, FIN-07 ·
**TAX** TAX-01, TAX-07, TAX-08, TAX-11, TAX-13, TAX-14, TAX-15 ·
**REP** REP-01, REP-02, REP-03 ·
**AUD** AUD-01, AUD-02, AUD-03, AUD-04, AUD-06, AUD-07, AUD-09, AUD-10 ·
**LEG** LEG-02, LEG-09, LEG-10, LEG-11 ·
**BIZ** BIZ-02, BIZ-04, BIZ-05, BIZ-06, BIZ-07, BIZ-08, BIZ-09 ·
**WFP** WFP-05, WFP-07 ·
**DATA** DATA-02, DATA-04, DATA-05 ·
**INT** INT-02, INT-03, INT-04, INT-06, INT-09 ·
**COM** COM-04 ·
**SEC** SEC-03, SEC-04 ·
**OPS** OPS-01, OPS-02, OPS-04, OPS-05, OPS-06
