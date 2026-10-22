package authz

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"zoiko.io/privacy-rights-svc/internal/middleware"
)

var ErrAuthzServiceUnavailable = errors.New("authorization-svc unavailable")
var ErrAuthorizationDenied = errors.New("authorization denied")

type Client struct {
	httpClient *http.Client
	baseURL    string
}

func NewClient(baseURL string) *Client {
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), httpClient: &http.Client{Timeout: 5 * time.Second}}
}

func (c *Client) CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error {
	reqBody, _ := json.Marshal(map[string]string{
		"principal_id":    principalID,
		"legal_entity_id": legalEntityID,
		"action_type":     actionType,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/authorize", bytes.NewReader(reqBody))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	// The envelope authorization-svc expects of every caller (the same set the
	// sibling privacy services send): who is asking, in which tenant, and a
	// traceable request. Without it authorization-svc can only admit the call
	// as an envelope violation, and would refuse it outright under strict
	// enforcement — which would turn every privacy-rights-svc request into a 503.
	req.Header.Set("X-Principal-Id", principalID)
	req.Header.Set("X-Legal-Entity-Id", legalEntityID)
	// X-Tenant-Id is the caller's own VERIFIED tenant from the request context;
	// every handler passes that same tenant as legalEntityID, which is the
	// fallback only when no tenant is on the context.
	tenantID := middleware.TenantFromContext(ctx)
	if tenantID == "" {
		tenantID = legalEntityID
	}
	req.Header.Set("X-Tenant-Id", tenantID)
	req.Header.Set("X-Request-Id", uuid.New().String())
	// The caller's correlation id when it sent one (mutating routes), else a
	// fresh one, so the authorization decision is always traceable.
	correlation := middleware.CorrelationFromContext(ctx)
	if correlation == "" {
		correlation = uuid.New().String()
	}
	req.Header.Set("X-Correlation-ID", correlation)
	req.Header.Set("X-Source-Channel", "web")
	// A fresh key per check, never the caller's own Idempotency-Key: an
	// authorization question must be evaluated every time it is asked, and
	// forwarding the caller's key could make authorization-svc replay an earlier
	// decision after a grant was revoked.
	req.Header.Set("Idempotency-Key", uuid.New().String())

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return ErrAuthzServiceUnavailable
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return ErrAuthzServiceUnavailable
	}
	var res struct {
		DecisionOutcome string `json:"decision_outcome"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return ErrAuthzServiceUnavailable
	}
	if res.DecisionOutcome != "GRANTED" {
		return ErrAuthorizationDenied
	}
	return nil
}
