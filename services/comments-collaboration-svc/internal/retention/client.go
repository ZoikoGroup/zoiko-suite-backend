// Package retention checks retention-registry-svc's real Resolve endpoint
// before DeleteComment redacts a comment — same narrow-client shape as
// reporting-orchestration-svc's own internal/retention client. A resolved
// legal hold blocks the redaction outright; an unreachable
// retention-registry-svc blocks it too — fail closed, never assume "no
// hold."
package retention

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"time"

	"go.uber.org/zap"
)

// ErrRetentionServiceUnavailable is returned on any network error or
// non-200 response.
var ErrRetentionServiceUnavailable = errors.New("retention-registry-svc unavailable")

type resolution struct {
	Blocked bool `json:"blocked"`
}

// Client is the narrow interface the handler depends on.
type Client interface {
	// IsBlocked asks retention-registry-svc's real /v1/retention/resolve
	// whether this comment is currently under a legal hold. Fails closed:
	// any transport/decode error is returned as
	// ErrRetentionServiceUnavailable, never treated as "not blocked."
	IsBlocked(ctx context.Context, tenantID, commentID, actorID, correlationID string) (bool, error)
}

type HTTPClient struct {
	baseURL string
	http    *http.Client
	log     *zap.Logger
}

func NewHTTPClient(baseURL string, log *zap.Logger) *HTTPClient {
	return &HTTPClient{baseURL: baseURL, log: log, http: &http.Client{Timeout: 3 * time.Second}}
}

// recordClass is a fixed constant — every comment is the same kind of
// record for retention purposes; only the entity_ref (the comment id)
// varies per call.
const recordClass = "COMMENT"

func (c *HTTPClient) IsBlocked(ctx context.Context, tenantID, commentID, actorID, correlationID string) (bool, error) {
	q := url.Values{}
	q.Set("record_class", recordClass)
	q.Set("tenant_id", tenantID)
	q.Set("entity_ref", commentID)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/v1/retention/resolve?"+q.Encode(), nil)
	if err != nil {
		return true, err
	}
	req.Header.Set("X-Principal-Id", actorID)
	req.Header.Set("X-Correlation-ID", correlationID)
	req.Header.Set("X-Tenant-Id", tenantID)

	resp, err := c.http.Do(req)
	if err != nil {
		c.log.Error("retention-registry-svc unavailable while checking legal hold", zap.String("comment_id", commentID), zap.Error(err))
		return true, ErrRetentionServiceUnavailable
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		c.log.Error("retention-registry-svc refused resolve", zap.String("comment_id", commentID), zap.Int("status", resp.StatusCode))
		return true, ErrRetentionServiceUnavailable
	}
	var r resolution
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return true, ErrRetentionServiceUnavailable
	}
	return r.Blocked, nil
}
