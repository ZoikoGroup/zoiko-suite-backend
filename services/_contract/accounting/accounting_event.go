package accounting

import (
	"encoding/json"
	"errors"
	"time"

	"zoiko.io/contract/types"
)

// AccountingEventStatus represents the lifecycle state of an accounting event in the posting engine.
type AccountingEventStatus string

const (
	AccountingEventStatusPending  AccountingEventStatus = "PENDING"
	AccountingEventStatusPosted   AccountingEventStatus = "POSTED"
	AccountingEventStatusRejected AccountingEventStatus = "REJECTED"
	AccountingEventStatusReversed AccountingEventStatus = "REVERSED"
)

// AccountingEvent represents an approved, deterministic accounting trigger produced by a source domain (ACC-EVENT).
// It acts as the canonical bridge between operational business transactions (AR/AP/Payroll/Assets/Tax)
// and the General Ledger posting engine (ZS-DATA-001 §11 & §23 Stage 3).
type AccountingEvent struct {
	AccountingEventID    types.UUID            `json:"accounting_event_id"`
	TenantID             types.UUID            `json:"tenant_id"`
	LegalEntityID        types.UUID            `json:"legal_entity_id"`
	SourceDomain         string                `json:"source_domain"` // "AR", "AP", "PAYROLL", "ASSETS", "TAX", "TREASURY"
	SourceObjectTable    string                `json:"source_object_table"` // e.g. "sales_invoices", "supplier_invoices"
	SourceObjectID       types.UUID            `json:"source_object_id"`
	EventType            string                `json:"event_type"` // e.g. "INVOICE_ISSUED", "PAYMENT_APPLIED"
	OccurredAt           time.Time             `json:"occurred_at"` // Business event occurrence time
	EffectiveDate        time.Time             `json:"effective_date"` // Accounting-effective date before period resolution
	AmountBasis          json.RawMessage       `json:"amount_basis"` // Structured source monetary/tax facts
	PostingPolicyVersion string                `json:"posting_policy_version"` // Version of posting policy to derive journals
	Status               AccountingEventStatus `json:"status"`
	CreatedAt            time.Time             `json:"created_at"`
}

// Validate checks mandatory accounting event properties.
func (e AccountingEvent) Validate() error {
	if e.AccountingEventID.IsNil() || e.TenantID.IsNil() || e.LegalEntityID.IsNil() {
		return errors.New("accounting_event requires accounting_event_id, tenant_id and legal_entity_id")
	}
	if e.SourceDomain == "" || e.SourceObjectID.IsNil() || e.EventType == "" {
		return errors.New("accounting_event requires source_domain, source_object_id and event_type")
	}
	if e.OccurredAt.IsZero() || e.EffectiveDate.IsZero() {
		return errors.New("accounting_event requires occurred_at and effective_date")
	}
	if len(e.AmountBasis) == 0 {
		return errors.New("accounting_event requires structured amount_basis")
	}
	if e.Status == "" {
		return errors.New("accounting_event requires status")
	}
	return nil
}
