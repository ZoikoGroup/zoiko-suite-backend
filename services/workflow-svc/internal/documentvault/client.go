// Package documentvault verifies that material referenced by an audit
// engagement exists in the authoritative vault and records the immutable
// version that was reviewed.
package documentvault

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"go.uber.org/zap"

	"zoiko.io/workflow-svc/internal/domain"
)

// Client is deliberately narrow: the workflow service neither stores nor
// interprets evidence bytes. It asks the document vault to establish the
// document's scope and current immutable version before recording a pointer.
type Client interface {
	VerifyDocument(ctx context.Context, tenantID, legalEntityID, documentID, actorID, correlationID string) (int, error)
}

type documentSummary struct {
	DocumentID     string `json:"document_id"`
	TenantID       string `json:"tenant_id"`
	LegalEntityID  string `json:"legal_entity_id"`
	CurrentVersion int    `json:"current_version"`
}

type HTTPClient struct {
	baseURL string
	http    *http.Client
	log     *zap.Logger
}

func NewHTTPClient(baseURL string, log *zap.Logger) *HTTPClient {
	return &HTTPClient{baseURL: baseURL, log: log, http: &http.Client{Timeout: 2 * time.Second}}
}

// VerifyDocument returns the vault's current version only after both tenant
// and legal entity match. The requesting audit actor is forwarded because the
// vault records a real, authorized metadata access; a service must not bypass
// that audit trail through an anonymous internal request.
func (c *HTTPClient) VerifyDocument(ctx context.Context, tenantID, legalEntityID, documentID, actorID, correlationID string) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/v1/documents/"+documentID, nil)
	if err != nil {
		return 0, domain.ErrAuditAcceptanceEvidenceUnavailable
	}
	req.Header.Set("X-Tenant-Id", tenantID)
	req.Header.Set("X-Principal-Id", actorID)
	req.Header.Set("X-Correlation-ID", correlationID)

	resp, err := c.http.Do(req)
	if err != nil {
		c.log.Error("document-vault-svc unavailable while verifying audit acceptance evidence", zap.String("document_id", documentID), zap.Error(err))
		return 0, domain.ErrAuditAcceptanceEvidenceUnavailable
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return 0, domain.ErrAuditAcceptanceEvidenceInvalid
	}
	if resp.StatusCode != http.StatusOK {
		c.log.Error("document-vault-svc refused audit acceptance evidence verification", zap.String("document_id", documentID), zap.Int("status", resp.StatusCode))
		return 0, domain.ErrAuditAcceptanceEvidenceUnavailable
	}

	var doc documentSummary
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return 0, domain.ErrAuditAcceptanceEvidenceUnavailable
	}
	if doc.DocumentID != documentID || doc.TenantID != tenantID || doc.LegalEntityID != legalEntityID || doc.CurrentVersion < 1 {
		return 0, domain.ErrAuditAcceptanceEvidenceInvalid
	}
	return doc.CurrentVersion, nil
}
