// Package tax calls the real tax-determination-svc to obtain a genuine
// withholding determination for an AP_INVOICE item, when the caller
// declares ApplyWithholding. Never inferred — WithholdingAmount comes only
// from this call's own response, the same doctrine as AP-07's tax-recovery
// lines, reused here for withholding instead of reclaim.
package tax

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"time"

	"go.uber.org/zap"

	"zoiko.io/payment-proposal-svc/internal/domain"
)

type Client interface {
	Determine(ctx context.Context, principalID string, req DetermineRequest) (*Result, error)
}

type DetermineRequest struct {
	TransactionID  string
	LegalEntityID  string
	JurisdictionID string
	TaxCategory    string
	GrossAmount    float64
	Currency       string
	EffectiveFrom  string
}

type Result struct {
	DeterminationID     string  `json:"determination_id"`
	CalculatedTaxAmount float64 `json:"calculated_tax_amount"`
}

type wireRequest struct {
	TransactionID  string  `json:"transaction_id"`
	SourceModule   string  `json:"source_module"`
	LegalEntityID  string  `json:"legal_entity_id"`
	JurisdictionID string  `json:"jurisdiction_id"`
	TaxCategory    string  `json:"tax_category"`
	GrossAmount    float64 `json:"gross_amount"`
	Currency       string  `json:"currency"`
	EffectiveFrom  string  `json:"effective_from"`
	EvaluatedBy    string  `json:"evaluated_by"`
}

type HTTPClient struct {
	baseURL string
	http    *http.Client
	log     *zap.Logger
}

func NewHTTPClient(baseURL string, log *zap.Logger) *HTTPClient {
	return &HTTPClient{baseURL: baseURL, log: log, http: &http.Client{Timeout: 5 * time.Second}}
}

func (c *HTTPClient) Determine(ctx context.Context, principalID string, r DetermineRequest) (*Result, error) {
	body := wireRequest{
		TransactionID: r.TransactionID, SourceModule: "AP", LegalEntityID: r.LegalEntityID,
		JurisdictionID: r.JurisdictionID, TaxCategory: r.TaxCategory, GrossAmount: r.GrossAmount,
		Currency: r.Currency, EffectiveFrom: r.EffectiveFrom, EvaluatedBy: principalID,
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, domain.ErrTaxDeterminationFailed
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/tax-determinations", bytes.NewReader(payload))
	if err != nil {
		return nil, domain.ErrTaxDeterminationFailed
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Principal-Id", principalID)

	resp, err := c.http.Do(req)
	if err != nil {
		c.log.Error("tax-determination-svc unreachable — failing closed", zap.Error(err))
		return nil, domain.ErrTaxDeterminationFailed
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		c.log.Error("unexpected response from tax-determination-svc — failing closed", zap.Int("status", resp.StatusCode))
		return nil, domain.ErrTaxDeterminationFailed
	}

	var out Result
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || out.DeterminationID == "" {
		return nil, domain.ErrTaxDeterminationFailed
	}
	return &out, nil
}
