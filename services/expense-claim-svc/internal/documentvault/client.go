// Package documentvault verifies a receipt reference against the real
// document-vault-svc before it can be attached to an expense line. Fails
// closed. Cross-claim receipt reuse (negative-path scenario #2) is enforced
// by this service's own database constraint, not by document-vault-svc,
// which has no concept of expense claims.
package documentvault

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"go.uber.org/zap"

	"zoiko.io/expense-claim-svc/internal/domain"
)

type Client interface {
	// VerifyReceipt confirms documentID exists, belongs to
	// tenantID/legalEntityID, and is not PURGE_PENDING.
	VerifyReceipt(ctx context.Context, actingPrincipalID, tenantID, legalEntityID, documentID string) error
}

type document struct {
	DocumentID    string `json:"document_id"`
	TenantID      string `json:"tenant_id"`
	LegalEntityID string `json:"legal_entity_id"`
	Status        string `json:"status"`
}

type HTTPClient struct {
	baseURL string
	http    *http.Client
	log     *zap.Logger
}

func NewHTTPClient(baseURL string, log *zap.Logger) *HTTPClient {
	return &HTTPClient{baseURL: baseURL, log: log, http: &http.Client{Timeout: 2 * time.Second}}
}

func (c *HTTPClient) VerifyReceipt(ctx context.Context, actingPrincipalID, tenantID, legalEntityID, documentID string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/v1/documents/"+documentID, nil)
	if err != nil {
		return domain.ErrDocumentServiceUnavailable
	}
	req.Header.Set("X-Principal-Id", actingPrincipalID)
	req.Header.Set("X-Tenant-Id", tenantID)

	resp, err := c.http.Do(req)
	if err != nil {
		c.log.Error("document-vault-svc unreachable — failing closed", zap.Error(err))
		return domain.ErrDocumentServiceUnavailable
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return domain.ErrDocumentNotFound
	}
	if resp.StatusCode != http.StatusOK {
		c.log.Error("unexpected response from document-vault-svc — failing closed", zap.Int("status", resp.StatusCode))
		return domain.ErrDocumentServiceUnavailable
	}

	var d document
	if err := json.NewDecoder(resp.Body).Decode(&d); err != nil {
		return domain.ErrDocumentServiceUnavailable
	}
	if d.DocumentID == "" {
		return domain.ErrDocumentNotFound
	}
	if d.TenantID != tenantID || d.LegalEntityID != legalEntityID {
		return domain.ErrDocumentMismatch
	}
	if d.Status == "PURGE_PENDING" {
		return domain.ErrDocumentNotUsable
	}
	return nil
}
