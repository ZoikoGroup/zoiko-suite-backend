// Package manifest provides a client for verifying, against the real
// evidence-manifest-svc, that an artifact a caller claims as evidence genuinely
// exists and belongs to the caller's own tenant and legal entity.
//
// Scope of verification in v1: existence plus tenant/legal-entity match.
// It does NOT gate on the manifest's status — evidence-manifest-svc owns that
// lifecycle and no spec section says which statuses count as valid evidence,
// so asserting one would be scope invention. Recorded in context.md §10.
//
// Fail-closed throughout: any network error, timeout, or unexpected response
// means the artifact does not count, rather than silently counting.
package manifest

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"

	"go.uber.org/zap"
	idenv "zoiko.io/identity-envelope"

	"github.com/go-chi/chi/v5/middleware"

	"zoiko.io/evidence-requirements-svc/internal/domain"
	svcenvelope "zoiko.io/evidence-requirements-svc/internal/envelope"
)

// Client is the narrow interface the evaluator depends on.
type Client interface {
	// VerifyManifest returns nil if manifestID exists in evidence-manifest-svc
	// and belongs to tenantID/legalEntityID. Returns
	// domain.ErrManifestNotFound, domain.ErrManifestMismatch, or
	// domain.ErrManifestServiceUnavailable otherwise — all of which mean
	// "this artifact does not count as evidence".
	VerifyManifest(ctx context.Context, tenantID, legalEntityID, manifestID, principalID string) error
}

// manifestSummary is the subset of evidence-manifest-svc's EvidenceManifest this service needs.
type manifestSummary struct {
	ManifestID    string `json:"manifest_id"`
	TenantID      string `json:"tenant_id"`
	LegalEntityID string `json:"legal_entity_id"`
}

// HTTPClient implements Client against a real evidence-manifest-svc instance.
type HTTPClient struct {
	baseURL string
	http    *http.Client
	log     *zap.Logger
}

// NewHTTPClient constructs an HTTPClient bound to baseURL, e.g.
// "http://evidence-manifest-svc:8131" (no trailing slash).
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

func (c *HTTPClient) VerifyManifest(ctx context.Context, tenantID, legalEntityID, manifestID, principalID string) error {
	// The id is caller input: escape it so it can only name one path segment.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/v1/evidence-manifests/"+url.PathEscape(strings.TrimSpace(manifestID)), nil)
	if err != nil {
		return domain.ErrManifestServiceUnavailable
	}
	// evidence-manifest-svc resolves tenant scope from this header via its own
	// TenantContext middleware, not a query param.
	req.Header.Set("X-Tenant-Id", tenantID)
	requestID := middleware.GetReqID(ctx)
	sourceChannel := "system"
	// evidence-manifest-svc requires X-Principal-Id; without it every
	// manifest-backed evaluation fails closed with 503. The caller's
	// principal is the default, the request envelope's actor wins.
	if principalID == "" {
		principalID = "evidence-requirements-svc"
	}

	if env, ok := svcenvelope.FromContext(ctx); ok {
		if env.TenantID != "" {
			req.Header.Set("X-Tenant-Id", env.TenantID)
		}
		if env.Actor() != "" {
			principalID = env.Actor()
		}
		if env.RequestID != "" {
			requestID = env.RequestID
		}
		if env.SourceChannel != "" {
			sourceChannel = string(env.SourceChannel)
		}
		if env.CorrelationID != "" {
			req.Header.Set("X-Correlation-ID", env.CorrelationID)
		}
		if env.CausationID != "" {
			req.Header.Set("X-Causation-Id", env.CausationID)
		}
	}
	req.Header.Set("X-Principal-Id", principalID)
	req.Header.Set("X-Request-Id", requestID)
	req.Header.Set("X-Source-Channel", sourceChannel)
	// The caller's verified identity envelope, so evidence-manifest-svc checks
	// EVIDENCE_MANIFEST_READ for the person whose evaluation this is.
	idenv.Forward(ctx, req)

	resp, err := c.http.Do(req)
	if err != nil {
		c.log.Error("evidence-manifest-svc unreachable — artifact cannot be confirmed, failing closed",
			zap.String("manifest_id", manifestID), zap.Error(err))
		return domain.ErrManifestServiceUnavailable
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return domain.ErrManifestNotFound
	}
	if resp.StatusCode != http.StatusOK {
		c.log.Error("unexpected response from evidence-manifest-svc — failing closed",
			zap.Int("status", resp.StatusCode), zap.String("manifest_id", manifestID))
		return domain.ErrManifestServiceUnavailable
	}

	var s manifestSummary
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
		return domain.ErrManifestServiceUnavailable
	}
	// An empty manifest_id means the response did not actually carry a
	// manifest (e.g. a tenant-scope mismatch resolved server-side to an
	// empty body rather than a 404).
	if s.ManifestID == "" {
		return domain.ErrManifestNotFound
	}
	// UUIDs compare case-insensitively: the same id in another case is the
	// same tenant or entity, not a mismatch.
	if !strings.EqualFold(s.TenantID, tenantID) || !strings.EqualFold(s.LegalEntityID, legalEntityID) {
		return domain.ErrManifestMismatch
	}
	return nil
}
