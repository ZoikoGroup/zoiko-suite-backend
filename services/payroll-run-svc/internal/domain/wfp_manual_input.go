package domain

// WFP-03 manual payroll inputs (WFP-NP-009). A manual input is proposed by one principal and decided by a
// different one; only an APPROVED one is pulled into an input batch, where it is classified by the governed
// mapping of source kind MANUAL like every other source. It is never pay on its own.

import (
	"fmt"
	"strings"
	"time"
)

const (
	ManualProposed  = "PROPOSED"
	ManualApproved  = "APPROVED"
	ManualRejected  = "REJECTED"
	ManualWithdrawn = "WITHDRAWN"
)

type ManualInput struct {
	ManualID        string     `json:"manual_id"`
	TenantID        string     `json:"tenant_id"`
	LegalEntityID   string     `json:"legal_entity_id"`
	PayrollPeriodID string     `json:"payroll_period_id"`
	WorkerID        string     `json:"worker_id"`
	InputCode       string     `json:"input_code"`
	Category        string     `json:"category"`
	Amount          string     `json:"amount"`
	Currency        string     `json:"currency"`
	EffectiveDate   string     `json:"effective_date"`
	Reason          string     `json:"reason"`
	EvidenceRef     string     `json:"evidence_ref"`
	State           string     `json:"state"`
	ProposedBy      string     `json:"proposed_by"`
	DecidedBy       *string    `json:"decided_by,omitempty"`
	DecidedAt       *time.Time `json:"decided_at,omitempty"`
	DecisionNote    *string    `json:"decision_note,omitempty"`
	CorrelationID   string     `json:"correlation_id"`
	CreatedAt       time.Time  `json:"created_at"`
}

// ProposeManualInputRequest names the worker and the period it is for. category states the intent (EARNING or
// DEDUCTION): the mapping of the code must agree, or the line is quarantined rather than guessed.
type ProposeManualInputRequest struct {
	PayrollPeriodID string `json:"payroll_period_id"`
	WorkerID        string `json:"worker_id"`
	InputCode       string `json:"input_code"`
	Category        string `json:"category"`
	Amount          string `json:"amount"`
	Currency        string `json:"currency"`
	EffectiveDate   string `json:"effective_date"`
	Reason          string `json:"reason"`
	EvidenceRef     string `json:"evidence_ref"`
}

func (r ProposeManualInputRequest) Validate() error {
	switch {
	case strings.TrimSpace(r.PayrollPeriodID) == "":
		return fmt.Errorf("payroll_period_id is required")
	case !isUUID(r.WorkerID):
		return fmt.Errorf("worker_id must be a UUID")
	case strings.TrimSpace(r.InputCode) == "":
		return fmt.Errorf("input_code is required")
	case r.Category != CategoryEarning && r.Category != CategoryDeduction:
		return fmt.Errorf("category must be EARNING or DEDUCTION")
	case !currencyShape(r.Currency):
		return fmt.Errorf("currency must be a 3-letter upper-case code")
	case strings.TrimSpace(r.Reason) == "":
		return fmt.Errorf("reason is required")
	case strings.TrimSpace(r.EvidenceRef) == "":
		return fmt.Errorf("evidence_ref is required: a manual input without evidence is not payable")
	}
	if _, err := time.Parse("2006-01-02", r.EffectiveDate); err != nil {
		return fmt.Errorf("effective_date must be YYYY-MM-DD")
	}
	amt, err := ParseDecimal(r.Amount)
	if err != nil {
		return fmt.Errorf("amount must be an exact decimal string")
	}
	if amt.Sign() <= 0 {
		return fmt.Errorf("amount must be greater than zero; the sign is carried by the category")
	}
	return nil
}

func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		switch {
		case i == 8 || i == 13 || i == 18 || i == 23:
			if c != '-' {
				return false
			}
		case !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'):
			return false
		}
	}
	return true
}

// DecideManualInputRequest approves, rejects or withdraws a manual input.
type DecideManualInputRequest struct {
	Note string `json:"note,omitempty"`
}

var (
	ErrManualInputNotFound      = errorString("manual input not found")
	ErrManualInputNotPending    = errorString("the manual input is not PROPOSED; a decision is final")
	ErrManualFourEyes           = errorString("a manual input cannot be approved or rejected by the principal who proposed it")
	ErrManualOnlyMakerWithdraws = errorString("only the principal who proposed a manual input can withdraw it")
)
