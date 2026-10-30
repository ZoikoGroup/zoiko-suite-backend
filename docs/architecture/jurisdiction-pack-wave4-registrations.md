# ZS-JUR-001 s16: registration types, licences, schemes and threshold monitors

Builds on Waves [0](jurisdiction-pack-wave0.md) to [4](jurisdiction-pack-wave4.md), [6](jurisdiction-pack-wave6.md) and [7](jurisdiction-pack-wave7.md). Implemented in
`jurisdiction-rules-svc` (migration `000021`; four action codes registered in `access-control-svc` migration `000024`). No new service. Follows ZS-JUR-001 s16, s15, s25, s26 and
closes the "Registrations, licences, schemes and threshold monitors" item in the Wave 4 *Not built* list.

## Decision: this service owns the rule content, not the registrations
s16 names five data objects. Two different kinds of thing are mixed in that table:

| s16 object | What it is | Owner |
|---|---|---|
| Registration Type | governed content: identifier format, authority, validation rules | **this service** (new family `REGISTRATION_TYPE`) |
| Scheme/Election (definition) | governed content: eligibility rule, lock-in, cancellation notice | **this service** (`SCHEME`) |
| License/Permit (type) | governed content: authority, term, renewal and grace rules | **this service** (`LICENCE_TYPE`) |
| Threshold Monitor | governed content: metric, period, threshold, breach behaviour | **this service** (`THRESHOLD_MONITOR`) |
| **Entity Registration** (an entity's actual number, dates, status, evidence) | tenant data | **not here**: `tenant-entity-registry-svc` already holds entity tax identities and `tax-determination-svc` already reads them (§9.J) |

Why: ZS-SVC-V-001 says the rules layer returns "rules-based applicability candidate/result from trusted facts. ... PDC does not register or file", and that elections and filing
positions stay in the Tax domain. Storing registrations here would create a second system of record for tenant tax identities. So this service stores **no** registration, licence or
election; callers send the facts and receive a verdict with its explanation, recorded as decision evidence (like Waves 4 to 6).

The four definitions are closed typed parameter families on the governed rule module ([decision](jurisdiction-pack-decision-rule-parameters.md)), so they inherit provenance,
independent publication, immutability after DRAFT, compile checks, signing, certification, rollout and rollback unchanged. Each lives only in its own reserved rule domain
(`REGISTRATION_TYPE`, `LICENCE_TYPE`, `SCHEME_ELECTION`, `THRESHOLD_MONITOR`); the compiler refuses a family in any other domain (`JUR-C092`) and a rule in a reserved domain with no
parameters (`JUR-C093`).

## The four families (all numbers are decimal strings; a JSON number is refused, JUR-NEG-25)
* **`REGISTRATION_TYPE`**: `kind`, `authority`, `identifier {pattern, normalize, examples_valid, examples_invalid}`, `evidence_required`, optional `threshold_monitor` code. The pattern
  must be anchored and uses Go's RE2 engine (linear time, so a pack author cannot ship a pattern that hangs the service). **A pattern is proved by its own examples when the rule is
  validated**: every valid example must match and every invalid one must not, so a pattern cannot be packaged that disagrees with its documentation.
* **`LICENCE_TYPE`**: `authority`, optional `term`, `renewal_notice_days`, `grace_days`, `evidence_required`.
* **`SCHEME`**: `authority`, `eligibility {metric, operator LT|LTE|GT|GTE, threshold, currency?}`, optional `lock_in`, `cancellation_notice_days`, `evidence_required`.
* **`THRESHOLD_MONITOR`**: `metric`, `period` (`STATIC` with a yearly `start_month`/`start_day`, or `ROLLING` with `months`), `operator` (GT or GTE), `threshold`, optional `currency`,
  `breach {effect: REGISTRATION_REQUIRED|REVIEW_REQUIRED|NOTIFY_ONLY, effective: IMMEDIATE|NEXT_PERIOD_START|DAYS_AFTER|END_OF_MONTH_FOLLOWING}`.

## Runtime (each is a read-only evaluation of the rule in force on `as_of`, from verified released packs; fail-closed like the other resolvers)
| Endpoint | Action | Answer |
|---|---|---|
| `POST /v1/registration-identifiers:validate` | `REGISTRATION_IDENTIFIER_VALIDATE` | `IDENTIFIER_VALID` / `IDENTIFIER_INVALID` |
| `POST /v1/licences:evaluate` | `LICENCE_EVALUATE` | validity window and status: `NOT_YET_VALID`, `ACTIVE`, `RENEWAL_WINDOW`, `IN_GRACE`, `EXPIRED` |
| `POST /v1/scheme-elections:evaluate` | `SCHEME_ELECTION_EVALUATE` | `SCHEME_ELIGIBLE` / `SCHEME_NOT_ELIGIBLE`, lock-in end, earliest cancellation |
| `POST /v1/threshold-monitors:evaluate` | `THRESHOLD_MONITOR_EVALUATE` | breached or not, breach date, effective date, headroom |

A verdict is HTTP 200; a refusal to answer (no parameters, unusable facts, unsupported jurisdiction, a licence term exceeded) is 422; `AMBIGUOUS` is 409; an unverifiable pack is 503.
Every answer is recorded as evidence (`decision_kind` `REGISTRATION`, `LICENCE`, `SCHEME`, `THRESHOLD`) and, when it resolves, anchored on the pack release, artifact, rule and content
digest (a database CHECK enforces the anchor). `Idempotency-Key` is supported.

### Behaviour worth knowing
* **The identifier is never stored.** The recorded request holds only a sha256 digest, and the answer carries only the digest of the normalised form. It is a *format* check: whether the number is registered is for the authority to say.
* **Licence dates:** a stated `expiry_date` is the last valid day; a derived one is the day before the term anniversary (issue 15 Jan 2026 + 1 year: last valid day 14 Jan 2027). A stated expiry beyond the type's maximum term is refused, not trusted. Month arithmetic clamps like retention (29 Feb 2028 + 1 year is 28 Feb 2029).
* **Scheme cancellation is deferred, not refused:** it takes effect at the later of `cancel_date + notice` and the lock-in end; `deferred_by_lock_in` tells the caller which applied.
* **Threshold maths is exact** (rational numbers, never floating point): 0.1 + 0.2 meets 0.3 exactly. `GT` needs to exceed the threshold, `GTE` needs to reach it. The rolling window is `(as_of - N months, as_of]` with month-end clamping; a static period starts on its yearly start date.
* **Observations after `as_of` are ignored**, so re-running for an earlier date reproduces the earlier answer. The breach date is the first date the cumulative total met the threshold inside the window; the rule in force on `as_of` decides which threshold applies.
* **Link to the obligation engine (s15):** a `REGISTRATION_REQUIRED` breach returns `registration_effective_date`, the value to pass to the due-date calculation as its `registration_date` fact. Review-only or notify-only breaches offer no registration date.
* **Certification:** the existing golden/boundary coverage applies to every rule; `JUR-T030` additionally requires a golden case that states the headline value (licence term, scheme threshold, monitor threshold). A registration type is covered by its own examples, checked at validation. These families are never calculated with (`PARAMETER_ONLY`).

## Authorization
Actions `REGISTRATION_IDENTIFIER_VALIDATE`, `LICENCE_EVALUATE`, `SCHEME_ELECTION_EVALUATE`, `THRESHOLD_MONITOR_EVALUATE`: registered in `access-control-svc` migration `000024` (no role template
created: who holds them is a role-design decision for Tax and Security leadership) and added to the demo seed caller bundle `JURISDICTION_RESOLVER_CALLER`.

## Not built
* **Entity registrations, licences and elections as stored records**, and any registry of expiring licences: owned by `tenant-entity-registry-svc` and the Tax domain. The licence endpoint could feed a reminder there; nothing here polls or notifies.
* ~~Registration-driven applicability~~: built, see [jurisdiction-pack-wave4-applicability.md](jurisdiction-pack-wave4-applicability.md) (a registration type names the obligations it brings; instances stay in `obligations-svc`).
* **Identifier check digits** (for example a modulus check): only a pattern is supported. Adding one would be a new closed field decided with Tax Engineering.
* ~~Cross-reference checks~~ that a `threshold_monitor` named by a registration type exists in the same pack: now a compiler warning (JUR-W031), see [jurisdiction-pack-wave4-applicability.md](jurisdiction-pack-wave4-applicability.md).
* **Compound eligibility** (several metrics combined) and **multi-rate or tiered thresholds**: each would be a new closed family.
* **Observation sourcing:** the caller supplies the observations; this service does not read ledgers or invoices.
* **Country content:** no real VAT format, licence term or threshold for any country was written (see Wave 8). Real packs need qualified local review.

## Deploy order
Apply `jurisdiction-rules-svc` migration `000021` before the new binary, and `access-control-svc` migration `000024` so the four actions are known. The down scripts re-narrow the evidence-kind check as
`NOT VALID` because evidence is append-only; `000024` down is refused while any bundle or role template still names the actions.

## Verification
Unit tests (`internal/domain/registrations_test.go`): parameter validation tables (cross-family fields, JSON numbers, unanchored patterns, patterns proved wrong by their own examples, 29 February start, rolling versus next-period-start), identifier normalisation and
non-echo, licence status boundaries at every edge, term and leap-day handling, scheme operator edges and lock-in deferral, threshold windows (rolling, static, month-end clamp, GT versus GTE, exact decimals, order independence, ignored later observations, invalid facts, observation cap).
Integration (embedded Postgres 16, `internal/registryit/registrations_integration_test.go`): all four families through the full pack pipeline (author, compile, sign, test, review, certify, release) and answered from the verified artifact; the rule in force by `as_of`; the identifier absent from stored evidence; idempotency; the database refusing a verdict without its basis; API and compiler refusals; and the `000005`-`000021` down/up round trip.
