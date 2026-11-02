package domain

import "errors"

// Domain errors. The handler maps each one to exactly one HTTP status and a
// stable problem code (internal/handler/problem.go, writeDomainError).

var (
	// 404 — not found, or owned by another tenant (never distinguished).
	ErrDefinitionNotFound = errors.New("report definition not found")
	ErrRunNotFound        = errors.New("report run not found")
	ErrExportNotFound     = errors.New("audit export request not found")

	// 409 — the lifecycle does not allow the requested change.
	ErrIllegalTransition   = errors.New("the requested status transition is not allowed from the current status")
	ErrDefinitionNotActive = errors.New("runs may only be requested against an ACTIVE report definition")
	ErrExportInvalidState  = errors.New("audit export is not in a state that permits this action")
	ErrExportNotRedactable = errors.New("redactions may only be recorded before the package is sealed")
	ErrBuildInProgress     = errors.New("a build of this export is already in progress; retry after it completes or its lease expires")
	ErrManifestRequired    = errors.New("export has no manifest: cannot seal an empty package")
	ErrLegalHoldActive     = errors.New("delivery blocked: a legal hold applies to this export's records")
	ErrIdempotencyMismatch = errors.New("Idempotency-Key was already used for a different request")
	ErrIdempotencyInFlight = errors.New("a request with this Idempotency-Key is still being processed")

	// 412 — If-Match did not match the current version.
	ErrVersionMismatch = errors.New("If-Match does not match the current version of the resource")

	// 403 — segregation of duties.
	ErrExportSelfApproval = errors.New("principal may not approve their own export request")

	// 424 / 503 — upstream dependencies (fail closed).
	ErrArchiveFetchFailed   = errors.New("could not fetch the referenced archive from audit-event-store-svc: export not built")
	ErrArchiveDigestInvalid = errors.New("audit-event-store-svc returned an archive without a valid SHA-256 digest: export not built")
	ErrRetentionServiceDown = errors.New("retention-registry-svc unavailable: delivery blocked (fail closed)")
)
