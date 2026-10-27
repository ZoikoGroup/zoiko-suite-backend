// Package registry confirms a legal entity against tenant-entity-registry-svc,
// the owner of the fact (ORG-01/02).
//
// Doc 03 §8.5: "Every obligation must be entity-bound". Until this check an
// obligation could name any UUID as its legal entity, including one that
// belongs to another tenant: authorization was asked about the entity, but
// nothing asked whether the entity existed in the caller's tenant.
package registry

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"go.uber.org/zap"
	idenv "zoiko.io/identity-envelope"

	"zoiko.io/obligations-svc/internal/domain"
	svcenvelope "zoiko.io/obligations-svc/internal/envelope"
)

// Checker confirms that a legal entity exists in the caller's tenant.
type Checker interface {
	CheckEntity(ctx context.Context, tenantID, legalEntityID string) error
}

// Client reads GET /v1/entities/{id}/status, which the registry answers only
// for an entity in the X-Tenant-Id it is called with (RLS), so another
// tenant's entity is a 404 exactly like a missing one.
type Client struct {
	baseURL string
	http    *http.Client
	log     *zap.Logger
}

// New builds a Client. An empty baseURL disables the check (Noop), which is
// refused outside local/dev by the caller's configuration.
func New(baseURL string, log *zap.Logger) *Client {
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), http: &http.Client{Timeout: 3 * time.Second}, log: log}
}

type entityStatus struct {
	EntityID     string `json:"entity_id"`
	TenantID     string `json:"tenant_id"`
	EntityStatus string `json:"entity_status"`
}

// CheckEntity returns nil when the entity exists in tenantID,
// domain.ErrLegalEntityNotFound when it does not (or belongs to another
// tenant), and domain.ErrRegistryUnavailable when the registry cannot answer —
// "cannot answer" is never treated as "yes".
func (c *Client) CheckEntity(ctx context.Context, tenantID, legalEntityID string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.baseURL+"/v1/entities/"+url.PathEscape(legalEntityID)+"/status", nil)
	if err != nil {
		return domain.ErrRegistryUnavailable
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Tenant-Id", tenantID)
	svcenvelope.ForwardTo(ctx, req)
	idenv.Forward(ctx, req)

	resp, err := c.http.Do(req)
	if err != nil {
		c.log.Error("tenant-entity-registry-svc unreachable — failing closed", zap.String("legal_entity_id", legalEntityID), zap.Error(err))
		return domain.ErrRegistryUnavailable
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return domain.ErrLegalEntityNotFound
	default:
		c.log.Error("tenant-entity-registry-svc answered unexpectedly — failing closed",
			zap.Int("status", resp.StatusCode), zap.String("legal_entity_id", legalEntityID))
		return domain.ErrRegistryUnavailable
	}
	var st entityStatus
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		return fmt.Errorf("%w: malformed registry response: %v", domain.ErrRegistryUnavailable, err)
	}
	if st.TenantID != "" && !strings.EqualFold(st.TenantID, tenantID) {
		return domain.ErrLegalEntityNotFound
	}
	return nil
}

// Noop admits every entity. Used only when no registry URL is configured in a
// local or development environment.
type Noop struct{}

// CheckEntity always succeeds.
func (Noop) CheckEntity(context.Context, string, string) error { return nil }
