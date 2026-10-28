# jurisdiction-rules-svc: spec deviations

Where this service's behaviour differs from a specification, and why (implementation plan §2 R15).

## ZS-API-001 §12: the `validate-action` 409 is a decision, not a problem

`POST /v1/validate-action` answers `409` with a `ValidateActionConflictResponse` (`application/json`) when the
rule set is `conflicted` (equal-precedence rules overlap) or `insufficient` (no rule in the ancestor chain).
That body is the evaluation result callers branch on, not an error report, so it keeps its own schema
rather than becoming an RFC 9457 problem. Every other 4xx/5xx from this service is
`application/problem+json` (the probe body on `/readyz` aside). Recorded 27 Oct 2026 in the API-2 review.
