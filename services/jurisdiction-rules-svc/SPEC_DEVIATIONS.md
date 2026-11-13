# jurisdiction-rules-svc: spec deviations

Where this service's behaviour differs from a specification, and why (implementation plan §2 R15).

## ZS-API-001 §12: the `validate-action` 409 is a decision, not a problem

`POST /v1/validate-action` answers `409` with a `ValidateActionConflictResponse` (`application/json`) when the
rule set is `conflicted` (equal-precedence rules overlap) or `insufficient` (no rule in the ancestor chain).
That body is the evaluation result callers branch on, not an error report, so it keeps its own schema
rather than becoming an RFC 9457 problem. Every other 4xx/5xx from this service is
`application/problem+json` (the probe body on `/readyz` aside). Recorded 27 Oct 2026 in the API-2 review.

## R6 / API-3: `If-Match` is optional (412, never 428)

Rule R6 asks for 428 when a command on an existing aggregate omits `If-Match`. This service
answers 412 for a stale `If-Match` on every aggregate (000020, 000034) but accepts a command
without one: the platform's tax, payroll, filing and receivables services call these routes
(and the e2e and seed scripts) without it, and a 428 would break them. The console always
sends it. Make it mandatory once those callers send it. Recorded 12 Nov 2026 (D2-1).

## API-4: legacy list shapes kept

A list answers the API-4 shape `{items, next_cursor, page_size}` only when the request names
`page_size`, `cursor` or `filter[...]`. Without them it keeps its original answer (a bare array or
a wrapped list, `limit`/`offset`, bare filters), because out-of-domain services read these
routes. A paged request with a bare legacy filter is 400 `unknown_filter`. Recorded 12 Nov 2026.

## J-01: `IDENTITY_VERIFY=warn` for reads and runtime resolutions

Administrative writes require a verified identity (`IDENTITY_VERIFY_ADMIN=require`). Reads and
the runtime resolution routes (`rule-resolutions:resolve`, `regulatory-calculations:execute`,
determinations, submissions) stay in the gate's warn mode on the local stack until the
out-of-domain services that call them forward the identity envelope (tracker X5 / R-3). The code
default outside local is `require`.

## ZS-JUR-001 JUR-NEG-17: no exemption rule family

"Missing registration evidence for exemption" has no rule to test: exemptions (with place of
supply, reverse charge and compound taxes) are not a rule family yet, by the wave-4 decision that
each new tax family is specified with Tax Engineering. Licence and scheme answers already name
their `evidence_required`.

## ZS-JUR-001 JUR-NEG-29: skew is detected, not enforced

Deployment skew between regions is reported (`/v1/ops/metrics`) and each resolver serves only its
own ring and region. "Blocks regulated action where policy requires consistency" needs a
consistency policy that nobody has defined, and services do not report the pack version they run.

## Doc 03 §8.2 consumed "external regulatory feed changes": producer missing

The consumer of `regulatory.source.changed` is built (JR-6) and documented in `asyncapi.yaml`;
external-data-feed-svc does not publish the event yet, so `KAFKA_REGULATORY_FEED_TOPIC` is unset
and drift is recorded by hand until it does (tracker D2-1.F05 ⛔).

## API-5: only queued operations can be cancelled

A pack compile or test run is one database transaction with no safe midpoint, so cancel stops an
operation only before it starts (409 `operation_not_cancelable` afterwards). An operation whose
process died stays `running`; the RUNBOOK says to re-issue it.

## PDC-02 NP-15 and NP-16 do not apply

NP-15 (branch mistaken for an entity) is decided by tenant-entity-registry's legal-personality
facts, which this service only reads. NP-16 (tenant override of a mandatory rule) has no path:
tenant elections and overrides (s28) are not implemented, so nothing can override a pack rule.
