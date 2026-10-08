// Package payableopenitem is an HTTP client to payable-open-item-svc
// (AP-08). AP-11 is the caller of AP-08's ApplyConfirmedPayment: once
// BNK-07 reports an instruction SETTLED, every payable the instruction pays
// is settled in AP-08 — net payment plus the withholding portion — keyed by
// BNK-07's payment id so a repeat is idempotent. Fails closed throughout.
package payableopenitem

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"time"

	"go.uber.org/zap"

	"zoiko.io/payment-run-svc/internal/domain"
)

type Client interface {
	// ApplyConfirmedPayment resolves the AP-08 payable for (payableSource,
	// sourceReference) and applies the Banking-confirmed settlement to it.
	ApplyConfirmedPayment(ctx context.Context, tenantID, principalID string, req ApplyRequest) error
}

type ApplyRequest struct {
	PayableSource     string // AP-09 vocabulary: AP_INVOICE | EXPENSE_CLAIM
	SourceReference   string
	NetAmount         float64
	WithholdingAmount float64
	Bnk07PaymentID    string
}

// AP-09 calls an AP invoice payable source "AP_INVOICE"; AP-08's own source
// type for the same payable is "SUPPLIER_INVOICE".
func ap08SourceType(payableSource string) string {
	if payableSource == "AP_INVOICE" {
		return "SUPPLIER_INVOICE"
	}
	return payableSource
}

type payable struct {
	PayableID string `json:"PayableID"`
}

type applyBody struct {
	Amount             float64 `json:"Amount"`
	WithholdingAmount  float64 `json:"WithholdingAmount"`
	ProviderPaymentRef string  `json:"ProviderPaymentRef"`
	Bnk07PaymentID     string  `json:"Bnk07PaymentID"`
}

type HTTPClient struct {
	baseURL string
	http    *http.Client
	log     *zap.Logger
}

func NewHTTPClient(baseURL string, log *zap.Logger) *HTTPClient {
	return &HTTPClient{baseURL: baseURL, log: log, http: &http.Client{Timeout: 5 * time.Second}}
}

func (c *HTTPClient) ApplyConfirmedPayment(ctx context.Context, tenantID, principalID string, req ApplyRequest) error {
	q := url.Values{"source_type": {ap08SourceType(req.PayableSource)}, "source_reference": {req.SourceReference}}
	lookup, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/ap08/payables/by-source?"+q.Encode(), nil)
	if err != nil {
		return domain.ErrPayableServiceUnavailable
	}
	lookup.Header.Set("X-Tenant-Id", tenantID)
	lookup.Header.Set("X-Principal-Id", principalID)
	resp, err := c.http.Do(lookup)
	if err != nil {
		c.log.Error("payable-open-item-svc unreachable — failing closed", zap.Error(err))
		return domain.ErrPayableServiceUnavailable
	}
	var p payable
	decodeErr := json.NewDecoder(resp.Body).Decode(&p)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || decodeErr != nil || p.PayableID == "" {
		c.log.Error("payable lookup by source failed — failing closed", zap.Int("status", resp.StatusCode), zap.String("source_reference", req.SourceReference))
		return domain.ErrPayableServiceUnavailable
	}

	body, err := json.Marshal(applyBody{
		Amount: req.NetAmount, WithholdingAmount: req.WithholdingAmount,
		ProviderPaymentRef: req.Bnk07PaymentID, Bnk07PaymentID: req.Bnk07PaymentID,
	})
	if err != nil {
		return domain.ErrPayableServiceUnavailable
	}
	apply, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/ap08/payables/"+p.PayableID+"/apply-confirmed-payment", bytes.NewReader(body))
	if err != nil {
		return domain.ErrPayableServiceUnavailable
	}
	apply.Header.Set("Content-Type", "application/json")
	apply.Header.Set("X-Tenant-Id", tenantID)
	apply.Header.Set("X-Principal-Id", principalID)
	resp, err = c.http.Do(apply)
	if err != nil {
		c.log.Error("payable-open-item-svc unreachable — failing closed", zap.Error(err))
		return domain.ErrPayableServiceUnavailable
	}
	defer resp.Body.Close()
	// 200 covers both a fresh application and an idempotent replay
	// (applied=false), which is exactly what a retried settlement needs.
	if resp.StatusCode != http.StatusOK {
		c.log.Error("apply-confirmed-payment refused — will retry on next poll", zap.Int("status", resp.StatusCode), zap.String("payable_id", p.PayableID))
		return domain.ErrPayableServiceUnavailable
	}
	return nil
}
