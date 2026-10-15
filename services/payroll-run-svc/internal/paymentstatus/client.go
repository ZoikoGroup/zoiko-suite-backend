// Package paymentstatus reads a payment's execution state from payment-status-svc
// (BNK-07), the Banking authority for whether a payment SETTLED. WFP-06 uses it as
// the only evidence that a liability was paid: it fails closed (unreachable = unknown,
// never settled) and never trusts a payment intent.
package paymentstatus

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"

	"zoiko.io/payroll-run-svc/internal/domain"
)

type Client struct {
	baseURL string
	http    *http.Client
}

func NewClient(baseURL string, c *http.Client) *Client {
	if c == nil {
		c = &http.Client{Timeout: 5 * time.Second}
	}
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), http: c}
}

// executionState is payment-status-svc's PaymentExecutionState; it declares no JSON
// tags, so the wire names are the Go field names. It carries no amount and no payee.
type executionState struct {
	PaymentID       string
	LegalEntityID   string
	Status          string
	FinalitySource  string
	SourceReference string
	HasOpenConflict bool
}

// Evidence is what Banking said about a settled payment.
type Evidence struct {
	Status          string
	FinalitySource  string
	SourceReference string // the caller's own reference Banking holds for the payment (an instruction id)
}

// VerifySettled requires that the payment exists, belongs to the legal entity, is
// SETTLED and has no open status conflict. Anything else is an error; an inability to
// ask is ErrBankingUnavailable.
func (c *Client) VerifySettled(ctx context.Context, tenantID, legalEntityID, paymentID string) (*Evidence, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/bnk07/payments/"+url.PathEscape(paymentID), nil)
	if err != nil {
		return nil, domain.ErrBankingUnavailable
	}
	id := uuid.NewString()
	req.Header.Set("X-Tenant-Id", tenantID)
	req.Header.Set("X-Principal-Id", "payroll-run-svc")
	req.Header.Set("X-Legal-Entity-Id", legalEntityID)
	req.Header.Set("X-Correlation-ID", id)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, domain.ErrBankingUnavailable
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, domain.ErrPaymentNotFound
	case resp.StatusCode != http.StatusOK:
		return nil, domain.ErrBankingUnavailable
	}
	var st executionState
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		return nil, domain.ErrBankingUnavailable
	}
	switch {
	case st.PaymentID != paymentID:
		return nil, domain.ErrPaymentNotFound
	case st.LegalEntityID != legalEntityID:
		return nil, domain.ErrPaymentEntityMismatch
	case st.HasOpenConflict:
		return nil, domain.ErrPaymentInConflict
	case st.Status != "SETTLED":
		return nil, domain.ErrPaymentNotSettled
	}
	return &Evidence{Status: st.Status, FinalitySource: st.FinalitySource, SourceReference: st.SourceReference}, nil
}
