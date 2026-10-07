package domain

import (
	"fmt"
	"math/big"
	"strings"
	"time"
)

type Severity string

const (
	SeverityLow    Severity = "LOW"
	SeverityMedium Severity = "MEDIUM"
	SeverityHigh   Severity = "HIGH"
)

// ControlException is the authoritative record of one control failure (§21).
// Invariant 7: every material exception has an owner, reason code, severity,
// due date, lifecycle and evidence trail. Owner is at minimum the control's
// owner ROLE from creation, so nothing is ever ownerless; a named principal is
// set by the assign command.
type ControlException struct {
	ExceptionID    string    `json:"exception_id"`
	TenantID       string    `json:"tenant_id"`
	RunID          string    `json:"run_id"`
	LegalEntityID  string    `json:"legal_entity_id"`
	Category       string    `json:"category"`
	ReasonCode     string    `json:"reason_code"`
	Assertion      string    `json:"assertion"`
	Severity       Severity  `json:"severity"`
	Side           Side      `json:"side"`
	RecordIDs      []string  `json:"record_ids"`
	Exposure       string    `json:"exposure"`
	Currency       string    `json:"currency"`
	Detail         string    `json:"detail"`
	OwnerRole      string    `json:"owner_role"`
	OwnerPrincipal *string   `json:"owner_principal_id,omitempty"`
	DueAt          time.Time `json:"due_at"`
	// ExpectedClearing is set for reconciling items (timing differences): the date
	// the item is expected to clear. DueAt is that date, not the default SLA.
	ExpectedClearing *string        `json:"expected_clearing,omitempty"`
	State            ExceptionState `json:"state"`
	Version          int            `json:"version"`
	CreatedAt        time.Time      `json:"created_at"`
	UpdatedAt        time.Time      `json:"updated_at"`
	// Derived on read, never stored (§7 attention signals).
	Attention []string `json:"attention,omitempty"`
}

// DeriveAttention computes non-authoritative signals. They never overwrite
// exception state.
func (e ControlException) DeriveAttention(now time.Time) []string {
	var out []string
	open := e.State != ExClosed && e.State != ExWaived && e.State != ExCarriedForward
	if open && now.After(e.DueAt) {
		out = append(out, "OVERDUE", "SLA_BREACH")
	}
	if e.Severity == SeverityHigh {
		out = append(out, "MATERIAL")
	}
	return out
}

// MaterialityView is the pinned materiality a run's severities are derived from.
type MaterialityView struct {
	AmountThreshold string
	Currency        string
}

// DeriveSeverity is policy-derived (§21): from the exposure against the run's
// pinned materiality, with a conservative floor. It is deliberately not
// operator-supplied.
//
//   - exposure >= threshold        => HIGH   (individually material)
//   - exposure >= threshold / 10   => MEDIUM
//   - otherwise                    => LOW
//
// If there is no pinned materiality, or it is in a different currency (so the
// comparison would need an FX basis we do not have), severity is MEDIUM: an
// unassessable exception must not default to the lowest tier. Record-integrity
// findings (missing, duplicate) never fall below MEDIUM.
func DeriveSeverity(category, exposure, currency string, m *MaterialityView) Severity {
	floor := SeverityLow
	if category == CatMissing || category == CatDuplicate {
		floor = SeverityMedium
	}
	sev := SeverityMedium
	if m != nil && m.Currency == currency {
		exp, ok1 := new(big.Rat).SetString(exposure)
		thr, ok2 := new(big.Rat).SetString(m.AmountThreshold)
		if ok1 && ok2 {
			switch {
			case exp.Cmp(thr) >= 0:
				sev = SeverityHigh
			case new(big.Rat).Mul(exp, big.NewRat(10, 1)).Cmp(thr) >= 0:
				sev = SeverityMedium
			default:
				sev = SeverityLow
			}
		}
	}
	if sev == SeverityLow && floor == SeverityMedium {
		sev = SeverityMedium
	}
	return sev
}

// DefaultSLA is the default remediation window by severity. It is a DEFAULT
// pending the §34 controlled decision "Exception SLA and escalation matrix",
// and is applied only at exception creation.
func DefaultSLA(s Severity) time.Duration {
	switch s {
	case SeverityHigh:
		return 2 * 24 * time.Hour
	case SeverityMedium:
		return 5 * 24 * time.Hour
	default:
		return 10 * 24 * time.Hour
	}
}

type AssignExceptionRequest struct {
	OwnerPrincipalID string `json:"owner_principal_id"`
	DueDate          string `json:"due_date"` // optional YYYY-MM-DD; may tighten but never extend the SLA
	Reason           string `json:"reason"`
}

func (r *AssignExceptionRequest) Validate() error {
	if strings.TrimSpace(r.OwnerPrincipalID) == "" {
		return fmt.Errorf("%w: owner_principal_id is required", ErrInvalidArgument)
	}
	if strings.TrimSpace(r.Reason) == "" {
		return fmt.Errorf("%w: reason is required for an ownership change", ErrInvalidArgument)
	}
	if r.DueDate != "" {
		if _, err := time.Parse("2006-01-02", r.DueDate); err != nil {
			return fmt.Errorf("%w: due_date must be YYYY-MM-DD", ErrInvalidArgument)
		}
	}
	return nil
}
