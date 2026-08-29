// Package ledger posts a real GRNI (goods-received-not-invoiced) journal
// entry to general-ledger-svc when a receipt is confirmed. It is
// deliberately best-effort: any failure here is reported to the caller as a
// typed error and recorded as an EXCEPTION accounting event by the handler —
// it never blocks or reverses the underlying receipt confirmation, matching
// AP-04's own stated failure semantics ("accounting event failure leaves
// visible pending/exception state, never silent receipt deletion").
//
// Idempotency rides entirely on general-ledger-svc's own correlation_id
// contract: this client always posts with correlation_id = receiptID, so a
// replayed confirmation calls CreateJournal again, gets the *same* journal
// back (created=false, 200 OK) instead of a duplicate, and this client's
// subsequent validate/post calls on that journal ID are themselves
// idempotent state transitions (a second post attempt after a journal is
// already FINALIZED simply confirms it is finalized — see poll below).
package ledger

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

var ErrGeneralLedgerUnavailable = errors.New("general-ledger-svc unavailable")
var ErrGeneralLedgerRejected = errors.New("general-ledger-svc rejected the journal")

// Client is the narrow interface the handler depends on.
type Client interface {
	// PostGRNI creates, validates and posts a balanced GRNI journal entry
	// (debit debitAccountCode / credit creditAccountCode, both for amount)
	// keyed by correlationID (the receipt ID) for idempotency, acting as
	// principalID. Returns the finalized journal ID on success.
	PostGRNI(ctx context.Context, params PostGRNIParams) (journalID string, err error)
}

type PostGRNIParams struct {
	TenantID          string
	LegalEntityID     string
	CorrelationID     string
	PrincipalID       string
	Amount            float64
	DebitAccountCode  string
	CreditAccountCode string
	Description       string
}

type HTTPClient struct {
	baseURL string
	http    *http.Client
}

func NewHTTPClient(baseURL string) *HTTPClient {
	return &HTTPClient{baseURL: strings.TrimRight(baseURL, "/"), http: &http.Client{Timeout: 5 * time.Second}}
}

type journalLine struct {
	AccountCode  string  `json:"account_code"`
	DebitAmount  float64 `json:"debit_amount"`
	CreditAmount float64 `json:"credit_amount"`
	Description  string  `json:"description"`
}

type createJournalRequest struct {
	TenantID      string        `json:"tenant_id"`
	LegalEntityID string        `json:"legal_entity_id"`
	FiscalPeriod  string        `json:"fiscal_period"`
	Description   string        `json:"description"`
	CorrelationID string        `json:"correlation_id"`
	Lines         []journalLine `json:"lines"`
}

type journalResponse struct {
	JournalID string `json:"journal_id"`
	Status    string `json:"status"`
}

func (c *HTTPClient) PostGRNI(ctx context.Context, p PostGRNIParams) (string, error) {
	body := createJournalRequest{
		TenantID:      p.TenantID,
		LegalEntityID: p.LegalEntityID,
		FiscalPeriod:  time.Now().UTC().Format("2006-01"),
		Description:   p.Description,
		CorrelationID: p.CorrelationID,
		Lines: []journalLine{
			{AccountCode: p.DebitAccountCode, DebitAmount: p.Amount, Description: p.Description},
			{AccountCode: p.CreditAccountCode, CreditAmount: p.Amount, Description: p.Description},
		},
	}

	journal, err := c.createJournal(ctx, p.PrincipalID, body)
	if err != nil {
		return "", err
	}
	if journal.Status == "PENDING" {
		if err := c.transition(ctx, p.PrincipalID, journal.JournalID, "validate"); err != nil {
			return "", err
		}
		journal.Status = "VALIDATED"
	}
	if journal.Status == "VALIDATED" {
		if err := c.transition(ctx, p.PrincipalID, journal.JournalID, "post"); err != nil {
			return "", err
		}
	}
	return journal.JournalID, nil
}

func (c *HTTPClient) createJournal(ctx context.Context, principalID string, body createJournalRequest) (*journalResponse, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrGeneralLedgerUnavailable, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/journals", bytes.NewReader(payload))
	if err != nil {
		return nil, ErrGeneralLedgerUnavailable
	}
	c.setHeaders(req, body.TenantID, principalID)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, ErrGeneralLedgerUnavailable
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return nil, ErrGeneralLedgerRejected
	}
	var out journalResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, ErrGeneralLedgerUnavailable
	}
	if out.JournalID == "" {
		return nil, ErrGeneralLedgerUnavailable
	}
	return &out, nil
}

func (c *HTTPClient) transition(ctx context.Context, principalID, journalID, action string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/journals/"+journalID+"/"+action, nil)
	if err != nil {
		return ErrGeneralLedgerUnavailable
	}
	c.setHeaders(req, "", principalID)

	resp, err := c.http.Do(req)
	if err != nil {
		return ErrGeneralLedgerUnavailable
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return ErrGeneralLedgerRejected
	}
	return nil
}

func (c *HTTPClient) setHeaders(req *http.Request, tenantID, principalID string) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Principal-Id", principalID)
	if tenantID != "" {
		req.Header.Set("X-Tenant-Id", tenantID)
	}
}
