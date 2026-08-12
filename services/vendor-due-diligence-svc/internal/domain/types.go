package domain

import "time"

// VendorDDCheck is one due-diligence run against one counterparty.
//
// The pair that matters is (Status, RiskOutcome), and it has four readings
// rather than two — collapsing them into pass/fail is the whole risk in this
// service:
//
//	COMPLETED / CLEAR    screening ran and matched nothing
//	COMPLETED / FLAGGED  screening ran and matched
//	STARTED   / ""       the run was recorded but never concluded
//	FAILED    / ""       the run could not be concluded and said so
//
// STARTED is not "in progress" in any useful sense — the screening is
// synchronous, so a row left in STARTED is one whose conclusion was lost. It
// carries no risk outcome and must never read as a clean result.
type VendorDDCheck struct {
	CheckID        string `json:"check_id"`
	TenantID       string `json:"tenant_id"`
	LegalEntityID  string `json:"legal_entity_id"`
	CounterpartyID string `json:"counterparty_id"`
	VendorName     string `json:"vendor_name"`
	Status         string `json:"status"`                 // STARTED, COMPLETED, FAILED
	RiskOutcome    string `json:"risk_outcome,omitempty"` // CLEAR, FLAGGED
	ScreeningBasis string `json:"screening_basis,omitempty"`
	// ScreeningSource names WHAT did the screening, as a stable machine value
	// rather than a phrase inside ScreeningBasis.
	//
	// This exists because CLEAR from this service is not a sanctions clearance.
	// The only screening implemented is an exact, case-insensitive match against
	// a hardcoded denylist of two names (see handler.screenVendorName) — there is
	// no sanctions feed on this platform to call. A consumer that renders CLEAR as
	// "cleared" reports an unscreened vendor as a screened one that passed, which
	// is the same defect class as reading spend-controls' `no_policy_configured`
	// as an approval. Callers can only avoid that if the wire tells them which
	// screening ran, so it does.
	ScreeningSource        string     `json:"screening_source,omitempty"` // STUB_DENYLIST
	CorrelationID          string     `json:"correlation_id,omitempty"`
	InitiatedByPrincipalID string     `json:"initiated_by_principal_id"`
	StartedAt              time.Time  `json:"started_at"`
	CompletedAt            *time.Time `json:"completed_at,omitempty"`
}

// Concluded reports whether this check reached a terminal state. A STARTED check
// has no conclusion to read, whatever RiskOutcome happens to hold.
func (c VendorDDCheck) Concluded() bool {
	return c.Status == StatusCompleted || c.Status == StatusFailed
}

// Status and outcome values this service writes. Named where the code genuinely
// singles a value out; nothing switches over the whole value space, per the
// doctrine forbidding Go enums over the VARCHAR status columns.
const (
	StatusStarted   = "STARTED"
	StatusCompleted = "COMPLETED"
	StatusFailed    = "FAILED"

	RiskClear   = "CLEAR"
	RiskFlagged = "FLAGGED"

	// ScreeningSourceStubDenylist is the only source implemented. See
	// VendorDDCheck.ScreeningSource for why it is on the wire at all.
	ScreeningSourceStubDenylist = "STUB_DENYLIST"

	// EvidenceTypeSanctionsScreening is the evidence recorded for every
	// screening run, match or no match — the absence of a match is a finding and
	// needs a record as much as a match does.
	EvidenceTypeSanctionsScreening = "SANCTIONS_SCREENING"
)

type VendorDDEvidence struct {
	EvidenceID        string    `json:"evidence_id"`
	CheckID           string    `json:"check_id"`
	TenantID          string    `json:"tenant_id"`
	EvidenceType      string    `json:"evidence_type"`
	Description       string    `json:"description"`
	DocumentReference string    `json:"document_reference,omitempty"`
	RecordedAt        time.Time `json:"recorded_at"`
}

type CreateCheckRequest struct {
	CounterpartyID string `json:"counterparty_id"`
	LegalEntityID  string `json:"legal_entity_id"`
	VendorName     string `json:"vendor_name"`
	CorrelationID  string `json:"correlation_id"`
	// DocumentReference optionally ties the evidence to a document held
	// elsewhere (document-vault-svc). The column existed from 000001 with no code
	// behind it: every evidence row was written with an empty string, so a table
	// designed to reference supporting documents referenced none and could not be
	// made to. Optional — absent is stored as NULL, not as "".
	DocumentReference string `json:"document_reference"`
}

type CheckDetailResponse struct {
	Check    VendorDDCheck      `json:"check"`
	Evidence []VendorDDEvidence `json:"evidence"`
	// Replayed marks a response that resolved an already-processed
	// correlation_id instead of running a new check.
	//
	// The HTTP status carries this too (200 vs 201), but a status code is not
	// enough on its own: a replay can resolve to a check left in STARTED by an
	// earlier attempt that died, and a caller seeing 200 with no risk outcome
	// needs to know it is looking at a stalled prior run rather than at a fresh
	// answer.
	Replayed bool `json:"replayed"`
}

type errorString string

func (e errorString) Error() string { return string(e) }

var (
	ErrCheckNotFound          = errorString("vendor due diligence check not found")
	ErrAuthorizationDenied    = errorString("authorization denied for vendor due diligence action")
	ErrAuthzServiceUnavailabe = errorString("authorization-svc unavailable")
	ErrIdentityMissing        = errorString("caller identity missing")
	ErrStoreUnavailable       = errorString("store unavailable")

	// ErrTenantMissing is returned by every store method when no tenant scope
	// reached it. Distinct from ErrStoreUnavailable because the handler used to
	// report both as 503 `store_unavailable`: a caller who had simply omitted
	// X-Tenant-Id was told this service's database was down.
	ErrTenantMissing = errorString("tenant scope missing")

	// ErrInvalidIdentifier is returned when an id cannot be a UUID at all.
	// Postgres raises SQLSTATE 22P02 from inside the driver before any row is
	// examined, which surfaced as 503 — a typo in a URL reading as an outage.
	ErrInvalidIdentifier = errorString("identifier is not a valid UUID")

	// ErrCheckAlreadyConcluded is returned when a conclusion is written to a
	// check that already has one. The UPDATE previously carried no status guard,
	// so a second completion could overwrite a terminal outcome — including
	// overwriting FLAGGED with CLEAR.
	ErrCheckAlreadyConcluded = errorString("vendor due diligence check has already concluded")
)
