package ledger

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"go.uber.org/zap"
	"zoiko.io/intercompany-accounting-svc/internal/domain"
)

// JournalDetail is the part of general-ledger-svc's GET /v1/journals/{id}
// response (domain.JournalWithLines) this service reads.
type JournalDetail struct {
	JournalID     string `json:"journal_id"`
	LegalEntityID string `json:"legal_entity_id"`
	Status        string `json:"status"`

	// CurrencyCode is the transaction currency of every line in the journal
	// (general-ledger-svc JournalHeader.CurrencyCode). Journals that predate
	// that column carry "XXX" (domain.NoCurrency).
	CurrencyCode string `json:"currency_code"`
	FiscalPeriod string `json:"fiscal_period"`

	Lines []JournalLine `json:"lines"`
}

// JournalLine amounts are decoded as exact decimals, never float64:
// general-ledger-svc writes them as JSON numbers, and domain.Decimal keeps
// the digits it was sent.
type JournalLine struct {
	AccountCode  string         `json:"account_code"`
	DebitAmount  domain.Decimal `json:"debit_amount"`
	CreditAmount domain.Decimal `json:"credit_amount"`
}

type Client struct {
	baseURL string
	http    *http.Client
	log     *zap.Logger
}

func NewClient(baseURL string, log *zap.Logger) *Client {
	return &Client{
		baseURL: baseURL,
		log:     log,
		http:    &http.Client{Timeout: 5 * time.Second},
	}
}

func (c *Client) GetJournal(ctx context.Context, tenantID, journalID string) (*JournalDetail, error) {
	reqURL := fmt.Sprintf("%s/v1/journals/%s", c.baseURL, journalID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Tenant-Id", tenantID)

	resp, err := c.http.Do(req)
	if err != nil {
		c.log.Error("failed to query general-ledger-svc", zap.Error(err))
		return nil, domain.ErrGLServiceUnavailable
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, domain.ErrEntryNotFound
	}
	if resp.StatusCode != http.StatusOK {
		c.log.Error("unexpected status from general-ledger-svc", zap.Int("status", resp.StatusCode))
		return nil, domain.ErrGLServiceUnavailable
	}

	var detail JournalDetail
	if err := json.NewDecoder(resp.Body).Decode(&detail); err != nil {
		return nil, err
	}

	return &detail, nil
}
