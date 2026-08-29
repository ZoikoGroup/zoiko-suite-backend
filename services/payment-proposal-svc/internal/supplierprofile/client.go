// Package supplierprofile resolves a vendor invoice's payee against the
// real supplier-financial-profile-svc (AP-01). This is the ONLY real source
// of a hold concept usable by AP-09 — accounts-payable-svc itself has none
// — and of a staleness signal (updated_at; there is no version column) for
// negative-path scenario #4. supplier-financial-profile-svc has no
// server-side filter by supplier_ref, so this client lists every profile
// and filters client-side — an inherited limitation of the peer service,
// not invented here, and fine at this platform's current scale.
package supplierprofile

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"go.uber.org/zap"

	"zoiko.io/payment-proposal-svc/internal/domain"
)

type Client interface {
	// FindActiveProfile returns the profile matching tenantID/legalEntityID/
	// supplierRef, however many profiles exist platform-wide.
	FindActiveProfile(ctx context.Context, tenantID, legalEntityID, supplierRef string) (*Profile, error)
}

type Profile struct {
	ProfileID         string    `json:"profile_id"`
	TenantID          *string   `json:"tenant_id,omitempty"`
	LegalEntityID     string    `json:"legal_entity_id"`
	SupplierRef       string    `json:"supplier_ref"`
	Status            string    `json:"status"`
	TaxWithholdingRef string    `json:"tax_withholding_ref,omitempty"`
	UpdatedAt         time.Time `json:"updated_at"`
}

type listResponse struct {
	Data []Profile `json:"data"`
}

type HTTPClient struct {
	baseURL string
	http    *http.Client
	log     *zap.Logger
}

func NewHTTPClient(baseURL string, log *zap.Logger) *HTTPClient {
	return &HTTPClient{baseURL: baseURL, log: log, http: &http.Client{Timeout: 3 * time.Second}}
}

func (c *HTTPClient) FindActiveProfile(ctx context.Context, tenantID, legalEntityID, supplierRef string) (*Profile, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/ap01/supplier-financial-profiles/", nil)
	if err != nil {
		return nil, domain.ErrPayeeServiceUnavailable
	}
	req.Header.Set("X-Tenant-Id", tenantID)

	resp, err := c.http.Do(req)
	if err != nil {
		c.log.Error("supplier-financial-profile-svc unreachable — failing closed", zap.Error(err))
		return nil, domain.ErrPayeeServiceUnavailable
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		c.log.Error("unexpected response from supplier-financial-profile-svc — failing closed", zap.Int("status", resp.StatusCode))
		return nil, domain.ErrPayeeServiceUnavailable
	}

	var out listResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, domain.ErrPayeeServiceUnavailable
	}
	for i := range out.Data {
		p := out.Data[i]
		if p.LegalEntityID == legalEntityID && p.SupplierRef == supplierRef && (p.TenantID == nil || *p.TenantID == tenantID) {
			return &p, nil
		}
	}
	return nil, domain.ErrPayeeNotFound
}
