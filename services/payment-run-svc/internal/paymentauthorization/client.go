// Package paymentauthorization is a real HTTP client to
// payment-authorization-svc (AP-10). This is the first real caller of
// AP-10's ConsumeAuthorization — a gap AP-10 itself documented as having
// no real caller yet. Fails closed throughout.
package paymentauthorization

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"go.uber.org/zap"

	"zoiko.io/payment-run-svc/internal/domain"
)

type Client interface {
	// GetApprovedAuthorization verifies authorizationID exists, belongs to
	// tenantID/legalEntityID, and is APPROVED (consumable).
	GetApprovedAuthorization(ctx context.Context, tenantID, legalEntityID, authorizationID string) (*Authorization, error)
	// ValidateAuthorization live re-checks an authorization immediately
	// before consumption — a stale fingerprint or payee identity fails it.
	ValidateAuthorization(ctx context.Context, tenantID, authorizationID string) (valid bool, err error)
	// ConsumeAuthorization is the real, single-use consumption call.
	ConsumeAuthorization(ctx context.Context, tenantID, principalID, authorizationID string) error
}

// Authorization is the subset of AP-10's own PaymentAuthorization (PascalCase
// wire shape, no json tags on AP-10's side) this service needs.
type Authorization struct {
	AuthorizationID string  `json:"AuthorizationID"`
	TenantID        *string `json:"TenantID"`
	LegalEntityID   string  `json:"LegalEntityID"`
	NetAmount       float64 `json:"NetAmount"`
	Currency        string  `json:"Currency"`
	Status          string  `json:"Status"`
	// PayeeRef is populated from the first entry of AP-10's own
	// payee_snapshots, when present — informational/audit only, empty for
	// an authorization with no AP_INVOICE-sourced items.
	PayeeRef string `json:"-"`
}

type payeeSnapshot struct {
	PayeeRef string `json:"PayeeRef"`
}

type getAuthResponse struct {
	Authorization  Authorization   `json:"authorization"`
	PayeeSnapshots []payeeSnapshot `json:"payee_snapshots"`
}

type validateResponse struct {
	Valid bool `json:"valid"`
}

type HTTPClient struct {
	baseURL string
	http    *http.Client
	log     *zap.Logger
}

func NewHTTPClient(baseURL string, log *zap.Logger) *HTTPClient {
	return &HTTPClient{baseURL: baseURL, log: log, http: &http.Client{Timeout: 3 * time.Second}}
}

func (c *HTTPClient) GetApprovedAuthorization(ctx context.Context, tenantID, legalEntityID, authorizationID string) (*Authorization, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/ap10/authorizations/"+authorizationID, nil)
	if err != nil {
		return nil, domain.ErrAuthorizationServiceUnavailable
	}
	req.Header.Set("X-Tenant-Id", tenantID)

	resp, err := c.http.Do(req)
	if err != nil {
		c.log.Error("payment-authorization-svc unreachable — failing closed", zap.Error(err))
		return nil, domain.ErrAuthorizationServiceUnavailable
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, domain.ErrAuthorizationNotEligible
	}
	if resp.StatusCode != http.StatusOK {
		c.log.Error("unexpected response from payment-authorization-svc — failing closed", zap.Int("status", resp.StatusCode))
		return nil, domain.ErrAuthorizationServiceUnavailable
	}

	var out getAuthResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, domain.ErrAuthorizationServiceUnavailable
	}
	a := out.Authorization
	if a.AuthorizationID == "" || a.LegalEntityID != legalEntityID || (a.TenantID != nil && *a.TenantID != tenantID) || a.Status != "APPROVED" {
		return nil, domain.ErrAuthorizationNotEligible
	}
	if len(out.PayeeSnapshots) > 0 {
		a.PayeeRef = out.PayeeSnapshots[0].PayeeRef
	}
	return &a, nil
}

func (c *HTTPClient) ValidateAuthorization(ctx context.Context, tenantID, authorizationID string) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/ap10/authorizations/"+authorizationID+"/validate", nil)
	if err != nil {
		return false, domain.ErrAuthorizationServiceUnavailable
	}
	req.Header.Set("X-Tenant-Id", tenantID)

	resp, err := c.http.Do(req)
	if err != nil {
		c.log.Error("payment-authorization-svc unreachable — failing closed", zap.Error(err))
		return false, domain.ErrAuthorizationServiceUnavailable
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		c.log.Error("unexpected response validating authorization — failing closed", zap.Int("status", resp.StatusCode))
		return false, domain.ErrAuthorizationServiceUnavailable
	}

	var out validateResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return false, domain.ErrAuthorizationServiceUnavailable
	}
	return out.Valid, nil
}

func (c *HTTPClient) ConsumeAuthorization(ctx context.Context, tenantID, principalID, authorizationID string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/ap10/authorizations/"+authorizationID+"/consume", nil)
	if err != nil {
		return domain.ErrAuthorizationServiceUnavailable
	}
	req.Header.Set("X-Tenant-Id", tenantID)
	req.Header.Set("X-Principal-Id", principalID)

	resp, err := c.http.Do(req)
	if err != nil {
		c.log.Error("payment-authorization-svc unreachable — failing closed", zap.Error(err))
		return domain.ErrAuthorizationServiceUnavailable
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		c.log.Error("unexpected response consuming authorization — failing closed", zap.Int("status", resp.StatusCode))
		return domain.ErrAuthorizationConsumeFailed
	}
	return nil
}
