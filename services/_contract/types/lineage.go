package types

import (
	"errors"
	"fmt"
	"time"
)

// TraceStage represents one of the 8 canonical stages in ZS-DATA-001 Section 23.
type TraceStage int

const (
	Stage1SourceRecord           TraceStage = 1 // Supplier invoice, bank statement, sales order, payroll result
	Stage2CanonicalBusinessObject TraceStage = 2 // Validated authoritative domain record with stable ID
	Stage3AccountingTaxEvent      TraceStage = 3 // Deterministic event with rule/policy/version context
	Stage4JournalTaxLedgerLine   TraceStage = 4 // Immutable monetary effect linked to source line and rules
	Stage5LedgerBalanceSnapshot  TraceStage = 5 // Controlled aggregate at posting watermark
	Stage6ReportRunReturn        TraceStage = 6 // Versioned mapping/calculation against frozen snapshot refs
	Stage7RegulatoryFiling       TraceStage = 7 // Approved output package
	Stage8EvidenceEvent          TraceStage = 8 // Approval, submission, authority receipt, hashes
)

func (s TraceStage) String() string {
	switch s {
	case Stage1SourceRecord:
		return "STAGE_1_SOURCE_RECORD"
	case Stage2CanonicalBusinessObject:
		return "STAGE_2_CANONICAL_BUSINESS_OBJECT"
	case Stage3AccountingTaxEvent:
		return "STAGE_3_ACCOUNTING_TAX_EVENT"
	case Stage4JournalTaxLedgerLine:
		return "STAGE_4_JOURNAL_TAX_LEDGER_LINE"
	case Stage5LedgerBalanceSnapshot:
		return "STAGE_5_LEDGER_BALANCE_SNAPSHOT"
	case Stage6ReportRunReturn:
		return "STAGE_6_REPORT_RUN_RETURN"
	case Stage7RegulatoryFiling:
		return "STAGE_7_REGULATORY_FILING"
	case Stage8EvidenceEvent:
		return "STAGE_8_EVIDENCE_EVENT"
	default:
		return fmt.Sprintf("STAGE_UNKNOWN_%d", int(s))
	}
}

// LineageEdge represents an attributable, directional link in the Source-to-Report graph.
type LineageEdge struct {
	EdgeID            UUID       `json:"edge_id"`
	TenantID          UUID       `json:"tenant_id"`
	LegalEntityID     UUID       `json:"legal_entity_id"`
	FromStage         TraceStage `json:"from_stage"`
	FromObjectType    string     `json:"from_object_type"`
	FromObjectID      UUID       `json:"from_object_id"`
	FromObjectVersion int        `json:"from_object_version"`
	ToStage           TraceStage `json:"to_stage"`
	ToObjectType      string     `json:"to_object_type"`
	ToObjectID        UUID       `json:"to_object_id"`
	ToObjectVersion   int        `json:"to_object_version"`
	TransformationRef string     `json:"transformation_ref,omitempty"`
	CreatedAt         time.Time  `json:"created_at"`
}

// Validate ensures the lineage edge connects valid stages in the canonical order.
func (e LineageEdge) Validate() error {
	if e.TenantID.IsNil() {
		return errors.New("lineage edge requires tenant_id")
	}
	if e.FromObjectID.IsNil() || e.ToObjectID.IsNil() {
		return errors.New("lineage edge requires non-nil from_object_id and to_object_id")
	}
	if e.FromStage <= 0 || e.ToStage <= 0 {
		return errors.New("lineage edge requires positive stages")
	}
	return nil
}
