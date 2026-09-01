// Package bankreconciliation is a real HTTP client to
// bank-reconciliation-svc — the literal enforcement of negative-path #1
// ("supplier refund marked received before bank confirmation"). A
// statement line that is not MATCHED is refused outright; there is no
// "close enough" state. Fails closed.
package bankreconciliation

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"go.uber.org/zap"

	"zoiko.io/supplier-recovery-svc/internal/domain"
)

// StatementLine is the subset of bank-reconciliation-svc's own
// StatementLine (snake_case wire shape — this peer predates the
// PascalCase-no-tags convention used elsewhere in this session) this
// service needs.
type StatementLine struct {
	StatementLineID string  `json:"statement_line_id"`
	TenantID        string  `json:"tenant_id"`
	LegalEntityID   string  `json:"legal_entity_id"`
	Amount          float64 `json:"amount"`
	CurrencyCode    string  `json:"currency_code"`
	BankReference   string  `json:"bank_reference"`
	Status          string  `json:"status"`
}

type Client interface {
	// GetConfirmedInboundLine fetches the statement line and confirms it is
	// a real, bank-confirmed (MATCHED) inbound (positive-amount) line
	// belonging to legalEntityID — never inferred from anything softer.
	GetConfirmedInboundLine(ctx context.Context, tenantID, legalEntityID, statementLineID string) (*StatementLine, error)
}

type HTTPClient struct {
	baseURL string
	http    *http.Client
	log     *zap.Logger
}

func NewHTTPClient(baseURL string, log *zap.Logger) *HTTPClient {
	return &HTTPClient{baseURL: baseURL, log: log, http: &http.Client{Timeout: 3 * time.Second}}
}

func (c *HTTPClient) GetConfirmedInboundLine(ctx context.Context, tenantID, legalEntityID, statementLineID string) (*StatementLine, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/v1/statement-lines/"+statementLineID, nil)
	if err != nil {
		return nil, domain.ErrBankReconciliationUnavailable
	}
	req.Header.Set("X-Tenant-Id", tenantID)

	resp, err := c.http.Do(req)
	if err != nil {
		c.log.Error("bank-reconciliation-svc unreachable — failing closed", zap.Error(err))
		return nil, domain.ErrBankReconciliationUnavailable
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, domain.ErrStatementLineNotFound
	}
	if resp.StatusCode != http.StatusOK {
		c.log.Error("unexpected response from bank-reconciliation-svc — failing closed", zap.Int("status", resp.StatusCode))
		return nil, domain.ErrBankReconciliationUnavailable
	}

	var line StatementLine
	if err := json.NewDecoder(resp.Body).Decode(&line); err != nil {
		return nil, domain.ErrBankReconciliationUnavailable
	}
	if line.LegalEntityID != legalEntityID || line.Amount <= 0 {
		return nil, domain.ErrStatementLineMismatch
	}
	if line.Status != "MATCHED" {
		return nil, domain.ErrStatementLineNotConfirmed
	}
	return &line, nil
}
