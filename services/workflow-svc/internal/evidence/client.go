// Package evidence looks up AUD-06 evidence contradiction flags from
// document-vault-svc without making workflow-svc an evidence owner —
// same narrow-client shape as internal/documentvault.
package evidence

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"go.uber.org/zap"
)

// ErrEvidenceServiceUnavailable is returned on any network error,
// non-200 response, or tenant/identity mismatch — the caller must fail
// closed, never treat this as "no contradiction."
var ErrEvidenceServiceUnavailable = errors.New("document-vault-svc evidence lookup unavailable")

// Client is the narrow interface the handler depends on.
type Client interface {
	// GetContradictionFlag returns whether evidenceID currently carries a
	// contradiction flag in document-vault-svc's own AUD-06 evidence
	// record. Fails closed (returns an error) on any network/non-200
	// response — a workpaper must never silently treat an unreachable
	// evidence service as "no contradiction."
	GetContradictionFlag(ctx context.Context, tenantID, evidenceID, actorID, correlationID string) (bool, error)
}

type evidenceSummary struct {
	EvidenceID  string `json:"evidence_id"`
	TenantID    string `json:"tenant_id"`
	StatusFlags struct {
		Contradictory bool `json:"contradictory"`
	} `json:"status_flags"`
}

type HTTPClient struct {
	baseURL string
	http    *http.Client
	log     *zap.Logger
}

func NewHTTPClient(baseURL string, log *zap.Logger) *HTTPClient {
	return &HTTPClient{baseURL: baseURL, log: log, http: &http.Client{Timeout: 2 * time.Second}}
}

func (c *HTTPClient) GetContradictionFlag(ctx context.Context, tenantID, evidenceID, actorID, correlationID string) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/v1/audit-evidence/"+evidenceID, nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("X-Tenant-Id", tenantID)
	req.Header.Set("X-Principal-Id", actorID)
	req.Header.Set("X-Correlation-ID", correlationID)

	resp, err := c.http.Do(req)
	if err != nil {
		c.log.Error("document-vault-svc unavailable while checking evidence contradiction flag", zap.String("evidence_id", evidenceID), zap.Error(err))
		return false, ErrEvidenceServiceUnavailable
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		c.log.Error("document-vault-svc refused evidence lookup", zap.String("evidence_id", evidenceID), zap.Int("status", resp.StatusCode))
		return false, ErrEvidenceServiceUnavailable
	}
	var summary evidenceSummary
	if err := json.NewDecoder(resp.Body).Decode(&summary); err != nil {
		return false, ErrEvidenceServiceUnavailable
	}
	if summary.EvidenceID != evidenceID || summary.TenantID != tenantID {
		return false, ErrEvidenceServiceUnavailable
	}
	return summary.StatusFlags.Contradictory, nil
}
