// Package clients holds asset-management-svc's outbound HTTP clients to
// other platform services.
package clients

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"time"

	"go.uber.org/zap"

	"zoiko.io/asset-management-svc/internal/domain"
)

type Clients struct {
	ledgerURL string
	closeURL  string
	http      *http.Client
	log       *zap.Logger
}

func New(ledgerURL, closeURL string, log *zap.Logger) *Clients {
	return &Clients{ledgerURL: ledgerURL, closeURL: closeURL, http: &http.Client{Timeout: 5 * time.Second}, log: log}
}

type periodStatusResp struct {
	CloseStatus string `json:"close_status"`
}

// CheckPeriodOpen is AST-03's own "hard-closed-period" dependency on
// financial-close-svc — the spec's own negative path, "Hard-closed-period
// event silently backdated." Mirrors general-ledger-svc's own
// internal/close.Client exactly: a period financial-close-svc has never
// heard of defaults to open (this service does not own the calendar),
// and an unreachable financial-close-svc fails CLOSED, never silently
// open.
func (c *Clients) CheckPeriodOpen(ctx context.Context, tenantID, legalEntityID, periodName string) error {
	u, err := url.Parse(c.closeURL + "/v1/close/periods/status")
	if err != nil {
		return err
	}
	q := u.Query()
	q.Set("legal_entity_id", legalEntityID)
	q.Set("period_name", periodName)
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return domain.ErrStoreUnavailable
	}
	req.Header.Set("X-Tenant-Id", tenantID)

	resp, err := c.http.Do(req)
	if err != nil {
		c.log.Error("financial-close-svc unreachable — failing closed on asset event apply", zap.Error(err))
		return domain.ErrPeriodCheckUnavailable
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil // period not registered with financial-close-svc — defaults to open
	}
	if resp.StatusCode != http.StatusOK {
		c.log.Error("unexpected status from financial-close-svc — failing closed", zap.Int("status", resp.StatusCode))
		return domain.ErrPeriodCheckUnavailable
	}

	var statusResp periodStatusResp
	if err := json.NewDecoder(resp.Body).Decode(&statusResp); err != nil {
		return domain.ErrPeriodCheckUnavailable
	}
	if statusResp.CloseStatus == "LOCKED" || statusResp.CloseStatus == "CLOSED" {
		return domain.ErrPeriodLocked
	}
	return nil
}

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
// the same pattern used by financial-close-svc's own PostAccrualRecognitionJournal
// and consolidation-svc's PostConsolidationAdjustmentJournal clients this
// session.
type LedgerLine struct {
	AccountCode  string
	DebitAmount  float64
	CreditAmount float64
}

// PostDepreciationAccountingEvent is AST-02's own "ACC-04" dependency —
// posts through general-ledger-svc's real system-originated posting path
// (POST /v1/postings/events), never a bespoke ledger write. sourceEventID
// is the run's own ID, making a retried Emit call idempotent against
// GL's own UNIQUE(tenant_id, source_event_id) — the spec's own negative
// path, "Rerun emits duplicate accounting event."
func (c *Clients) PostDepreciationAccountingEvent(ctx context.Context, tenantID, principalID, legalEntityID, fiscalPeriod, description, sourceEventID, correlationID string, lines []LedgerLine) (journalID string, err error) {
	return c.postAccountingEvent(ctx, tenantID, principalID, legalEntityID, fiscalPeriod, description, sourceEventID, correlationID, lines)
}

func (c *Clients) postAccountingEvent(ctx context.Context, tenantID, principalID, legalEntityID, fiscalPeriod, description, sourceEventID, correlationID string, lines []LedgerLine) (journalID string, err error) {
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
		c.log.Error("failed to post accounting event", zap.Error(err))
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

// ReverseDepreciationJournal calls general-ledger-svc's own
// CreateReversalPosting (via ACC-04's /v1/postings/reversals) — the same
// reversal primitive every other posted journal on this platform uses.
func (c *Clients) ReverseDepreciationJournal(ctx context.Context, tenantID, principalID, journalID, reason string) error {
	return c.reverseJournal(ctx, tenantID, principalID, journalID, reason, "reverse depreciation journal")
}

// PostAssetEventAccountingEvent is AST-03's own "ACC-04" dependency — the
// same system-originated posting path as AST-02's own
// PostDepreciationAccountingEvent, keyed by the event's own ID as
// source_event_id for idempotency.
func (c *Clients) PostAssetEventAccountingEvent(ctx context.Context, tenantID, principalID, legalEntityID, fiscalPeriod, description, sourceEventID, correlationID string, lines []LedgerLine) (journalID string, err error) {
	return c.postAccountingEvent(ctx, tenantID, principalID, legalEntityID, fiscalPeriod, description, sourceEventID, correlationID, lines)
}

// ReverseAssetEventJournal calls general-ledger-svc's own
// CreateReversalPosting — the same reversal primitive
// ReverseDepreciationJournal uses, for AST-03's own ReverseAssetEvent/
// SupersedeAssetEvent commands.
func (c *Clients) ReverseAssetEventJournal(ctx context.Context, tenantID, principalID, journalID, reason string) error {
	return c.reverseJournal(ctx, tenantID, principalID, journalID, reason, "reverse asset event journal")
}

func (c *Clients) reverseJournal(ctx context.Context, tenantID, principalID, journalID, reason, logMsg string) error {
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
		c.log.Error("failed to "+logMsg, zap.Error(err))
		return domain.ErrStoreUnavailable
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return domain.ErrStoreUnavailable
	}
	return nil
}
