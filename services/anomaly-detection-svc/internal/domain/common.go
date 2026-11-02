package domain

import (
	"encoding/base64"
	"regexp"
	"strings"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────────
// Identifiers
// ─────────────────────────────────────────────────────────────────────────────

// Every resource id is "<prefix><lowercase uuid>". The prefix tells a reader
// (and a log search) what kind of resource an id names.
const (
	PrefixAnomalyRule   = "arule-" // rule-engine detection rule
	PrefixAnomalyRecord = "anom-"  // rule-engine anomaly record
	PrefixAnomalyModel  = "amd_"   // governed AnomalyModel
	PrefixDetectionRun  = "dtr_"   // governed DetectionRun
	PrefixAnomalySignal = "ans_"   // governed AnomalySignal
	PrefixDisposition   = "rvd_"   // governed ReviewDisposition
)

var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// ValidateID checks that id is prefix followed by a lowercase UUID. A
// malformed id is a 400 (the client sent garbage), not a 404.
func ValidateID(prefix, id string) error {
	if !strings.HasPrefix(id, prefix) {
		return ErrInvalidID.WithDetail("identifier must start with " + prefix + " followed by a UUID")
	}
	uuidPart := strings.TrimPrefix(id, prefix)
	if !uuidPattern.MatchString(uuidPart) {
		return ErrInvalidID.WithDetail("identifier must be " + prefix + "<lowercase uuid>")
	}
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Request body scope
// ─────────────────────────────────────────────────────────────────────────────

// BodyScope is embedded in every request body. Tenant and legal entity are
// taken from the verified headers only; a body may repeat them, but only to
// agree. The handler refuses (403) a body whose value disagrees with the
// header. They are never used as a source of truth.
type BodyScope struct {
	TenantID string `json:"tenant_id,omitempty"`
}

// ─────────────────────────────────────────────────────────────────────────────
// Severity (shared by both surfaces)
// ─────────────────────────────────────────────────────────────────────────────

type Severity string

const (
	SeverityLow      Severity = "LOW"
	SeverityMedium   Severity = "MEDIUM"
	SeverityHigh     Severity = "HIGH"
	SeverityCritical Severity = "CRITICAL"
)

// IsValid reports whether s is one of the four severities.
func (s Severity) IsValid() bool {
	switch s {
	case SeverityLow, SeverityMedium, SeverityHigh, SeverityCritical:
		return true
	default:
		return false
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Advisory boundary (DA-CTRL-030 / N-19)
// ─────────────────────────────────────────────────────────────────────────────

// Requested-action values a caller may attach to a signal or disposition.
// Only REVIEW (or nothing) is accepted: this service is advisory and owns no
// protected action. Anything else — FREEZE_PAYMENT, WRITE_OFF, BLOCK_PAYMENT,
// ... — is refused with advisory_boundary_violation, because "anomaly score
// alone cannot freeze payment / write off balance". The owning domain
// (Banking/AP/GOV workflow) must run its own governed command.
const (
	RequestedActionNone   = ""
	RequestedActionReview = "REVIEW"
)

// CheckAdvisoryBoundary returns ErrAdvisoryBoundary for any requested action
// other than none or REVIEW.
func CheckAdvisoryBoundary(field, requestedAction string) error {
	normalized := strings.ToUpper(strings.TrimSpace(requestedAction))
	if normalized == RequestedActionNone || normalized == RequestedActionReview {
		return nil
	}
	refusal := ErrAdvisoryBoundary.WithDetail("requested_action " + requestedAction +
		" is a protected action; anomaly detection is advisory only and cannot trigger it — " +
		"submit a governed command to the owning domain instead")
	refusal.Field = field
	return refusal
}

// AssuranceStateAdvisory is declared on every signal and anomaly record
// returned by this service ("every analytical/AI output declares ... assurance
// state"). It is a derived read-model field, not lifecycle state.
const AssuranceStateAdvisory = "advisory"

// ─────────────────────────────────────────────────────────────────────────────
// Cursor pagination
// ─────────────────────────────────────────────────────────────────────────────

const (
	DefaultPageLimit = 50
	MaxPageLimit     = 200
)

// PageCursor is the keyset position after the last row of a page: lists are
// ordered newest first by (created_at DESC, id DESC).
type PageCursor struct {
	CreatedAt time.Time
	ID        string
}

// PageRequest is a validated page size plus an optional starting cursor.
type PageRequest struct {
	Limit  int
	Cursor *PageCursor
}

// EncodeCursor turns a keyset position into an opaque string for clients.
func EncodeCursor(createdAt time.Time, id string) string {
	raw := createdAt.UTC().Format(time.RFC3339Nano) + "|" + id
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// DecodeCursor parses a cursor produced by EncodeCursor. Anything else is a
// 400 invalid_cursor.
func DecodeCursor(cursor string) (*PageCursor, error) {
	invalid := NewBadRequestError("invalid_cursor", "cursor", "cursor is not a value returned by a previous page")

	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return nil, invalid
	}
	parts := strings.SplitN(string(raw), "|", 2)
	if len(parts) != 2 || parts[1] == "" {
		return nil, invalid
	}
	createdAt, err := time.Parse(time.RFC3339Nano, parts[0])
	if err != nil {
		return nil, invalid
	}
	return &PageCursor{CreatedAt: createdAt, ID: parts[1]}, nil
}
