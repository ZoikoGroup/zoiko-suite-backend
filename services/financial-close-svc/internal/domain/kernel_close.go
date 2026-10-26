package domain

// KernelCloseEvidence is what general-ledger-svc's accounting kernel says about whether a period of an entity can close: the
// blocking issues it found across the entity's active books (open reconciliation cases, failed postings, foreign items awaiting
// revaluation, journals in the review queue). Only the fields the close reads are decoded.
type KernelCloseEvidence struct {
	LegalEntityID  string   `json:"legal_entity_id"`
	FiscalPeriod   string   `json:"fiscal_period"`
	Ready          bool     `json:"ready"`
	BlockingIssues []string `json:"blocking_issues"`
}
