package registry

// kindError is a sentinel that refines a broader one: errors.Is matches both
// it and its parent, but its message is its own.
//
// fmt.Errorf("%w: detail", ErrConflict) cannot do this — the parent's text
// leads every message. That is how a stale expected_version reached callers
// as "conflict: resource already exists: record was modified by someone
// else" (seen live, 28 Sep 2026): a caller told a resource already exists goes
// looking for a duplicate that is not there.
type kindError struct {
	msg    string
	parent error
}

func (e *kindError) Error() string { return e.msg }
func (e *kindError) Unwrap() error { return e.parent }

var (
	// ErrStateConflict — a guarded write matched no row because the record
	// is no longer in the state the command requires (a conflict already
	// resolved, a tenant no longer ONBOARDING). 409, INVALID_TRANSITION.
	ErrStateConflict error = &kindError{"state conflict: the record is no longer in the state this command requires", ErrConflict}

	// ErrSourceUnverified — a command that must be evidence-backed arrived
	// without its evidence. 400, SOURCE_UNVERIFIED (ORG §3).
	ErrSourceUnverified error = &kindError{"source unverified", ErrInvalidInput}

	// ErrReferenceInvalid — a referenced master fact (jurisdiction) is
	// unknown or retired in its owning service. 400, REFERENCE_RETIRED.
	ErrReferenceInvalid error = &kindError{"reference unknown or retired", ErrInvalidInput}

	// ErrOnboardingKeyMismatch — an external_customer_key already used for a
	// DIFFERENT onboarding request. 409, IDEMPOTENCY_MISMATCH.
	ErrOnboardingKeyMismatch error = &kindError{"idempotency mismatch", ErrConflict}
)

// ErrCreateRequestMismatch — a create_request_id already used for a DIFFERENT party request. 409,
// IDEMPOTENCY_MISMATCH. Answering the first request's result would hand the caller a party they did not ask for.
var ErrCreateRequestMismatch error = &kindError{"idempotency mismatch: this create_request_id was used for a different party request", ErrConflict}

// ErrMergeProhibited — a merge MDM-05 forbids: it would collapse two distinct legal entities, it would chain, or a
// party involved is not live. 409, MERGE_PROHIBITED.
var ErrMergeProhibited error = &kindError{"merge prohibited", ErrConflict}

// ErrRateMissing — no approved exchange rate exists for the pair, type, date and set asked for, and none was derived.
// There is no fallback to an older date or to an unapproved observation: an authoritative calculation needing the rate
// is blocked, or routed to an explicit exception. 404, FX_RATE_MISSING.
var ErrRateMissing error = &kindError{"no approved exchange rate for that pair, type and market date", ErrNotFound}

// ErrDimensionMismatch — a conversion between units of different quantity dimensions (length to mass). Dimensionality is
// checked before any arithmetic, and the refusal names both dimensions. 422, DIMENSION_MISMATCH.
var ErrDimensionMismatch error = &kindError{"the units belong to different quantity dimensions", ErrInvalidInput}

// ErrConversionMissing — a unit has no approved conversion in force on the date asked for (never approved, not yet
// effective, or retired), so a quantity cannot be converted. Nothing is guessed. 404, CONVERSION_MISSING.
var ErrConversionMissing error = &kindError{"no approved conversion is in force for that unit on that date", ErrNotFound}

// ErrIdempotencyKeyMismatch — an Idempotency-Key was reused for a different request. 409, IDEMPOTENCY_MISMATCH.
var ErrIdempotencyKeyMismatch error = &kindError{"idempotency mismatch: this Idempotency-Key was already used for a different request", ErrConflict}
