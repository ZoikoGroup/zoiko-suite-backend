// Package documentvault provides a client for verifying, against the real
// document-vault-svc, that an artifact a caller claims as evidence genuinely
// exists and belongs to the caller's own tenant and legal entity.
//
// Why this exists (context.md §11.2, decided the strict way): the evaluate
// contract has the caller assert which artifacts are present. Trusting that
// list unverified would let a caller claim evidence it does not have and walk
// straight through a governance gate — which defeats the entire point of the
// gate, and is the exact anti-pattern the 2026-07-23 audit flagged in
// corporate-actions-svc (executing a corporate action against an unverified
// resolution_id). purchase-order-svc set the correct precedent by verifying
// its purchase_request_id against the real upstream record.
//
// Scope of verification in v1: existence plus tenant/legal-entity match. It
// does NOT gate on the document's status — document-vault-svc owns that
// lifecycle and no spec section says which statuses count as valid evidence,
// so asserting one here would be scope invention. Recorded in context.md §10.
//
// Fail-closed throughout: any network error, timeout, or unexpected response
// means the artifact does not count, rather than silently counting.
package documentvault

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"go.uber.org/zap"

	"zoiko.io/evidence-requirements-svc/internal/domain"
)

// Client is the narrow interface the evaluator depends on.
type Client interface {
	// VerifyDocument returns nil if documentID exists in document-vault-svc
	// and belongs to tenantID/legalEntityID. Returns
	// domain.ErrDocumentNotFound, domain.ErrDocumentMismatch, or
	// domain.ErrDocumentServiceUnavailable otherwise — all of which mean
	// "this artifact does not count as evidence".
	VerifyDocument(ctx context.Context, tenantID, legalEntityID, documentID string) error
}

// summary is the subset of document-vault-svc's Document this service needs.
type summary struct {
	DocumentID    string `json:"document_id"`
	TenantID      string `json:"tenant_id"`
	LegalEntityID string `json:"legal_entity_id"`
}

// HTTPClient implements Client against a real document-vault-svc instance.
type HTTPClient struct {
	baseURL string
	http    *http.Client
	log     *zap.Logger
}

// NewHTTPClient constructs an HTTPClient bound to baseURL, e.g.
// "http://document-vault-svc:8094" (no trailing slash).
func NewHTTPClient(baseURL string, log *zap.Logger) *HTTPClient {
	return &HTTPClient{
		baseURL: baseURL,
		log:     log,
		// Tight timeout: this sits on a blocking precondition check in a
		// caller's in-flight transaction, so it must not stall a
		// finalization indefinitely.
		http: &http.Client{Timeout: 2 * time.Second},
	}
}

func (c *HTTPClient) VerifyDocument(ctx context.Context, tenantID, legalEntityID, documentID string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/v1/documents/"+documentID, nil)
	if err != nil {
		return domain.ErrDocumentServiceUnavailable
	}
	// document-vault-svc resolves tenant scope from this header via its own
	// TenantContext middleware, not a query param.
	req.Header.Set("X-Tenant-Id", tenantID)

	resp, err := c.http.Do(req)
	if err != nil {
		c.log.Error("document-vault-svc unreachable — artifact cannot be confirmed, failing closed",
			zap.String("document_id", documentID), zap.Error(err))
		return domain.ErrDocumentServiceUnavailable
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return domain.ErrDocumentNotFound
	}
	if resp.StatusCode != http.StatusOK {
		c.log.Error("unexpected response from document-vault-svc — failing closed",
			zap.Int("status", resp.StatusCode), zap.String("document_id", documentID))
		return domain.ErrDocumentServiceUnavailable
	}

	var s summary
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
		return domain.ErrDocumentServiceUnavailable
	}
	// An empty document_id means the response did not actually carry a
	// document (e.g. a tenant-scope mismatch resolved server-side to an
	// empty body rather than a 404).
	if s.DocumentID == "" {
		return domain.ErrDocumentNotFound
	}
	if s.TenantID != tenantID || s.LegalEntityID != legalEntityID {
		return domain.ErrDocumentMismatch
	}
	return nil
}
