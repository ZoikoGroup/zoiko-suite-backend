# ZS-JUR-001 s15/s16: registration-driven applicability, and where obligation instances live

Builds on [jurisdiction-pack-wave4.md](jurisdiction-pack-wave4.md) (calendars and due dates) and [jurisdiction-pack-wave4-registrations.md](jurisdiction-pack-wave4-registrations.md) (s16 families). Implemented in `jurisdiction-rules-svc`
(migration `000023`; action `REGISTRATION_OBLIGATION_RESOLVE` registered in `access-control-svc` migration `000026`). No new service, and **no change to `obligations-svc`**.

## Decision: obligation instances stay in obligations-svc
The plan's step 4 said "per-entity obligation instances with `created` and `overdue` events". Checking before building showed that this would duplicate a service that already exists:

* ZS-JUR-001 s27 names the owner: **`RegulatoryObligation | Obligation Service: entity-specific obligation`.**
* `obligations-svc` already owns obligation state (OPEN, IN_PROGRESS, OVERDUE, CLOSED), due dates, obligation-to-entity mappings, filing requirements and applicability decisions, and already publishes `obligation.created`, `obligation.updated`, `obligation.overdue` and `obligation.closed`.

So this service does **not** hold instances and does not emit `regulatory-obligation.created` or `.overdue`: those s26 events are `obligation.created` and `obligation.overdue` from `obligations-svc`. What this service owns, and what was missing, is the *rule side*: which obligations apply
for a registration, and when each is due.

## Registration-driven applicability
The `REGISTRATION_TYPE` family gains an optional `obligations` list (up to 20 distinct UPPER_SNAKE obligation codes): the obligations an entity holding that registration must meet. It is part of the rule, so it is versioned, sourced, independently
published, signed and certified like the rest of the rule; rules written before this change are unaffected.

`POST /v1/registration-obligations:resolve` takes `jurisdiction`, `as_of`, up to 25 `registrations` (`registration_code`, `registration_date`, optional `deregistration_date`) and the period `facts` the obligations anchor on.

* A registration is **in force** on `as_of` when it started on or before it and has not ended; the deregistration date is the first day it no longer applies. One that is not in force brings nothing and says so.
* For a registration in force, the registration type rule in force on `as_of` supplies the obligation codes. An unknown type is `NO_RULE`: nothing is assumed about the obligations it would bring.
* Each obligation's due date is **derived** by the existing engine (the same as `POST /v1/obligation-calculations:calculate`), with `registration_date` taken from the registration. Per obligation the answer is `DUE_DATE_CALCULATED` with the full derivation
  (pack release, rule version, calendar version, steps), or the reason there is none (`NO_OBLIGATION_RULE`, `INVALID_FACTS`, `AMBIGUOUS` ...). When no period fact was supplied and the obligation cannot be derived, a `reason` says so, because the engine's own
  answer (no rule, or invalid facts) depends on whether the pack is in force on the registration date.
* The result is recorded as evidence (`decision_kind APPLICABILITY`). The request holds type codes and dates only: **no registration number, tenant id or entity id**.

This is the link s16 needed to s15: a `THRESHOLD_MONITOR` breach yields `registration_effective_date`; that becomes the registration's `registration_date` here; the registration type then yields the obligations and their due dates.

## Cross-reference check (JUR-W031)
The compiler now warns, without refusing, when a registration type names an obligation or a threshold monitor that the same pack does not define for the jurisdiction. It is a warning because a jurisdiction's rules can legitimately be split across packs; the runtime reports a
missing obligation explicitly. This also closes the "cross-reference" item left open in the s16 write-up.

## The integration that is still open: obligations-svc
`obligations-svc` accepts `due_date` from its caller, whereas s15 says due dates must be derived. The seam is clear: obligation creation could ask this endpoint (or `/v1/obligation-calculations:calculate`) for the date and record the decision id as its basis, and a registration change
could trigger a re-evaluation. **That is not built, deliberately**: it changes `obligations-svc`'s create path (derive versus accept) for every existing caller, and it is another team's service. It needs a decision on whether derivation is required, optional, or
only a warning when the supplied date differs. Until then the two services can already be used together by a caller that sends the derived `due_date` it received here.

## Not built
* **Obligation instances, overdue detection and filing** here (they are `obligations-svc`'s, see above). `GET /v1/obligation-calculations` still gives a point-in-time `overdue` verdict when `as_of` is passed.
* **Calling `obligations-svc` or reacting to its events**, and any automatic re-evaluation when a registration, holiday or rule changes.
* **Obligations that depend on more than a registration** (entity type, size, sector, scheme election): `scheme` and `threshold_monitor` results are separate answers the caller combines; they are not yet conditions on an obligation.
* **Obligation frequency expansion** (generating every period's due date for a year): the caller supplies one set of period facts per call.

## Deploy order
Apply migration `000023` before the new binary, and `access-control-svc` `000026` so the action is known. The `000023` down script re-narrows the evidence-kind check as `NOT VALID` (evidence is append-only); evidence migrations form a chain, so revert and re-apply `000023` before any earlier one.

## Verification
Unit: the `obligations` field (valid, optional, lower case, too short, duplicate, spaces, non-strings, more than 20, rejected next to another family). Integration (embedded Postgres 16): the due-date scenario's calendar and obligation rule released together with a registration type; JUR-W031 for the missing
obligation and monitor and not for the defined one; a registration in force with a derived date (10 Nov, through the weekend and the authority closure) and an undefined obligation reported as such; not started and ended registrations; the exclusive end day; unknown type; missing period facts with
its reason; several registrations; evidence kind and idempotent replay; request validation; and the `000005`-`000023` down/up round trip plus `000023` alone.
