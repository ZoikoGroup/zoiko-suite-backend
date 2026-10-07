// Package entitlement resolves ORG-02 §4.2's "plan entitlement" — the
// server-resolved context that decides whether a commercial subscription may
// be provisioned as a tenant. The authority is commercial-account-svc
// (COM Entitlement, a §4.2 dependency); this service only asks.
package entitlement

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"go.uber.org/zap"
)

var (
	// ErrNotEntitled — the subscription exists but its status does not permit
	// provisioning (past due, restricted, suspended, canceled, terminated).
	ErrNotEntitled = fmt.Errorf("subscription is not entitled to provision a tenant")
	// ErrSubscriptionNotFound — no such subscription.
	ErrSubscriptionNotFound = fmt.Errorf("subscription not found")
	// ErrUnavailable — commercial-account-svc could not answer. Fail closed.
	ErrUnavailable = fmt.Errorf("entitlement service unavailable")
)

// Checker decides whether a subscription may be provisioned.
type Checker interface {
	CheckProvisioning(ctx context.Context, subscriptionID, principalID string) error
}

// entitled are the statuses that may provision: a paid subscription and an
// evaluation (trial) one. Everything else is a commercial hold.
var entitled = map[string]bool{"ACTIVE": true, "EVALUATION": true}

// HTTPChecker asks commercial-account-svc: GET /v1/subscriptions/{id}.
type HTTPChecker struct {
	baseURL string
	client  *http.Client
	log     *zap.Logger
}

func NewHTTPChecker(baseURL string, log *zap.Logger) *HTTPChecker {
	return &HTTPChecker{
		baseURL: strings.TrimRight(baseURL, "/"),
		client:  &http.Client{Timeout: 2 * time.Second},
		log:     log,
	}
}

func (c *HTTPChecker) CheckProvisioning(ctx context.Context, subscriptionID, principalID string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/v1/subscriptions/"+subscriptionID, nil)
	if err != nil {
		return ErrUnavailable
	}
	req.Header.Set("X-Principal-Id", principalID)
	req.Header.Set("X-Source-Channel", "system")
	resp, err := c.client.Do(req)
	if err != nil {
		c.log.Error("commercial-account-svc unreachable — refusing provisioning (fail-closed)", zap.Error(err))
		return ErrUnavailable
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return fmt.Errorf("%w: %s", ErrSubscriptionNotFound, subscriptionID)
	default:
		c.log.Error("unexpected entitlement answer — refusing provisioning (fail-closed)",
			zap.Int("status", resp.StatusCode))
		return ErrUnavailable
	}
	var sub struct {
		Status string `json:"status"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&sub); err != nil {
		return ErrUnavailable
	}
	if !entitled[sub.Status] {
		return fmt.Errorf("%w: subscription %s is %s", ErrNotEntitled, subscriptionID, sub.Status)
	}
	return nil
}

// StubChecker permits every subscription. Local development only: config
// refuses to start staging or production without COMMERCIAL_ACCOUNT_URL.
type StubChecker struct{ log *zap.Logger }

func NewStubChecker(log *zap.Logger) *StubChecker { return &StubChecker{log: log} }

func (c *StubChecker) CheckProvisioning(_ context.Context, subscriptionID, _ string) error {
	c.log.Warn("entitlement STUB — subscription not checked (local only)", zap.String("subscription_id", subscriptionID))
	return nil
}
