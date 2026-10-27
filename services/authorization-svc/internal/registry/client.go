// Package registry asks tenant-entity-registry-svc whether a legal entity
// belongs to a tenant. /v1/authorize uses it when the entity_status_projection
// has not heard of the entity — every entity created before this service
// consumed entity.created, whose events Kafka retention has since dropped.
//
// The registry scopes every read by the request tenant (legal_entities
// WHERE tenant_id = $2), so it cannot answer "who owns X?" — only "is X in
// tenant T?". GET /v1/entities/{id}/status is that question: 200 with the
// tenant and status, 404 when the entity is not in T (or does not exist).
// It makes no authorization call back to this service, so there is no loop.
package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
)

// ErrUnavailable is any answer other than 200 or 404. Callers fail closed.
var ErrUnavailable = errors.New("tenant-entity-registry-svc unavailable")

// ServicePrincipal is the X-Principal-Id this service reads the registry as.
const ServicePrincipal = "authorization-svc"

// Membership is the registry's answer for one (entity, tenant) pair.
type Membership struct {
	Found  bool
	Status string // the registry's EntityStatus verbatim when Found
}

// Client is what the handler needs; tests substitute it.
type Client interface {
	EntityInTenant(ctx context.Context, legalEntityID, tenantID string) (Membership, error)
}

// HTTPClient implements Client against a real registry.
type HTTPClient struct {
	baseURL string
	client  *http.Client
	log     *zap.Logger
}

func NewHTTPClient(baseURL string, log *zap.Logger) *HTTPClient {
	return &HTTPClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		client:  &http.Client{Timeout: 2 * time.Second},
		log:     log,
	}
}

type statusResponse struct {
	EntityID     string `json:"entity_id"`
	TenantID     string `json:"tenant_id"`
	EntityStatus string `json:"entity_status"`
}

func (c *HTTPClient) EntityInTenant(ctx context.Context, legalEntityID, tenantID string) (Membership, error) {
	u := fmt.Sprintf("%s/v1/entities/%s/status", c.baseURL, url.PathEscape(legalEntityID))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return Membership{}, ErrUnavailable
	}
	req.Header.Set("X-Tenant-Id", tenantID)
	req.Header.Set("X-Principal-Id", ServicePrincipal)
	req.Header.Set("X-Source-System", ServicePrincipal)
	req.Header.Set("X-Source-Channel", "system")
	req.Header.Set("X-Request-Id", uuid.NewString())
	cid, _ := ctx.Value(correlationKey{}).(string)
	if cid == "" {
		cid = uuid.NewString()
	}
	req.Header.Set("X-Correlation-ID", cid)

	resp, err := c.client.Do(req)
	if err != nil {
		c.log.Error("tenant-entity-registry-svc unreachable — failing closed",
			zap.String("legal_entity_id", legalEntityID), zap.Error(err))
		return Membership{}, ErrUnavailable
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		var body statusResponse
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			c.log.Error("tenant-entity-registry-svc: undecodable status — failing closed", zap.Error(err))
			return Membership{}, ErrUnavailable
		}
		// The registry filters on the request tenant; a row for any other
		// tenant would be its defect, and is not taken as membership.
		if !strings.EqualFold(body.TenantID, tenantID) {
			c.log.Error("tenant-entity-registry-svc answered for another tenant — failing closed",
				zap.String("legal_entity_id", legalEntityID))
			return Membership{}, ErrUnavailable
		}
		return Membership{Found: true, Status: strings.ToUpper(body.EntityStatus)}, nil
	case http.StatusNotFound:
		return Membership{}, nil
	default:
		c.log.Error("unexpected response from tenant-entity-registry-svc — failing closed",
			zap.Int("status", resp.StatusCode), zap.String("legal_entity_id", legalEntityID))
		return Membership{}, ErrUnavailable
	}
}

type correlationKey struct{}

// WithCorrelationID carries the request's correlation id onto the registry call.
func WithCorrelationID(ctx context.Context, correlationID string) context.Context {
	return context.WithValue(ctx, correlationKey{}, correlationID)
}
