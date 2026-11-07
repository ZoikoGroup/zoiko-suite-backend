# workflow-svc: spec deviations

Where this service's behaviour differs from a specification, and why (implementation plan §2 R15).

## ZS-API-001 §12: the `verify-release` 409 is a decision, not a problem

`POST /v1/workflows/{workflow_instance_id}/verify-release` answers `409` with a `ReleaseVerification`
body (`application/json`, `can_release: false`, `status: INVALID` and the reason) when the Critical Release
Rule (ZS-STATE-001 §6.1) fails. That body is the release decision callers branch on, not an error report,
so it keeps its own schema rather than becoming an RFC 9457 problem. Every other 4xx/5xx from this service
is `application/problem+json` (the probe body on `/readyz` aside). Recorded 28 Oct 2026 in the API-2
review.
