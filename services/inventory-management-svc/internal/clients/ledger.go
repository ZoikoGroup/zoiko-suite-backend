package clients

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"

	"go.uber.org/zap"

	"zoiko.io/inventory-management-svc/internal/domain"
)

type postingEventLine struct {
	AccountCode  string  `json:"account_code"`
	DebitAmount  float64 `json:"debit_amount,omitempty"`
	CreditAmount float64 `json:"credit_amount,omitempty"`
}

type postAccountingEventRequest struct {
	LegalEntityID string             `json:"legal_entity_id"`
	FiscalPeriod  string             `json:"fiscal_period"`
	Description   string             `json:"description"`
	SourceEventID string             `json:"source_event_id"`
	CorrelationID string             `json:"correlation_id"`
	Lines         []postingEventLine `json:"lines"`
}

type postingExecutionResponse struct {
	JournalID *string `json:"journal_id"`
	Status    string  `json:"status"`
}

// LedgerLine is the caller-facing shape for one journal line — mirrors
// asset-management-svc's own clients.LedgerLine.
type LedgerLine struct {
	AccountCode  string
	DebitAmount  float64
	CreditAmount float64
}

// PostInventoryAccountingEvent is INV-04's own "ACC-04" dependency —
// posts through general-ledger-svc's real system-originated posting path
// (POST /v1/postings/events), never a bespoke ledger write. sourceEventID
// is the caller's own idempotency key (a run ID or write-down ID),
// mirroring asset-management-svc's own AST-02/03 clients exactly.
func (c *Clients) PostInventoryAccountingEvent(ctx context.Context, tenantID, principalID, legalEntityID, fiscalPeriod, description, sourceEventID, correlationID string, lines []LedgerLine) (journalID string, err error) {
	body := postAccountingEventRequest{
		LegalEntityID: legalEntityID, FiscalPeriod: fiscalPeriod, Description: description,
		SourceEventID: sourceEventID, CorrelationID: correlationID,
	}
	for _, l := range lines {
		body.Lines = append(body.Lines, postingEventLine{AccountCode: l.AccountCode, DebitAmount: l.DebitAmount, CreditAmount: l.CreditAmount})
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.ledgerURL+"/v1/postings/events", bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-Id", tenantID)
	req.Header.Set("X-Principal-Id", principalID)

	resp, err := c.http.Do(req)
	if err != nil {
		c.log.Error("failed to post inventory accounting event", zap.Error(err))
		return "", domain.ErrStoreUnavailable
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return "", domain.ErrStoreUnavailable
	}
	var execResp postingExecutionResponse
	if err := json.NewDecoder(resp.Body).Decode(&execResp); err != nil {
		return "", err
	}
	if execResp.JournalID == nil {
		return "", domain.ErrStoreUnavailable
	}
	return *execResp.JournalID, nil
}

// ReverseInventoryJournal calls general-ledger-svc's own
// CreateReversalPosting — the same reversal primitive every other posted
// journal on this platform uses.
func (c *Clients) ReverseInventoryJournal(ctx context.Context, tenantID, principalID, journalID, reason string) error {
	payload, err := json.Marshal(map[string]string{"original_journal_id": journalID, "reason": reason})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.ledgerURL+"/v1/postings/reversals", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-Id", tenantID)
	req.Header.Set("X-Principal-Id", principalID)

	resp, err := c.http.Do(req)
	if err != nil {
		c.log.Error("failed to reverse inventory journal", zap.Error(err))
		return domain.ErrStoreUnavailable
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return domain.ErrStoreUnavailable
	}
	return nil
}
