// Package authz provides a client for confirming, via authorization-svc,
// that a principal is actually allowed to submit an approval action.
//
// Doctrine (03-microservices.md §8.4): "Approval workflows extend
// authorization. They do not replace it." Every approval action is
// checked against authorization-svc synchronously, fail-closed — an
// unreachable authorization-svc rejects the action, it never silently
// permits it.
package authz

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/workflow-svc/internal/domain"
	svcenvelope "zoiko.io/workflow-svc/internal/envelope"
	svcmiddleware "zoiko.io/workflow-svc/internal/middleware"
)

// Client is the narrow interface the handler depends on.
type Client interface {
	// CheckApprovalAllowed returns nil if principalID is authorized to
	// approve within legalEntityID. Returns domain.ErrAuthorizationDenied
	// if authorization-svc says DENIED, or
	// domain.ErrAuthorizationServiceUnavailable if it cannot be reached —
	// callers must fail-closed on the latter, same as every other
	// synchronous cross-service call in this platform.
	// resourceOwnerPrincipalID is the principal who owns the resource (e.g.
	// workflow initiator) for own-object SoD checks in authorization-svc.
	CheckApprovalAllowed(ctx context.Context, principalID, legalEntityID, resourceOwnerPrincipalID string) error
	// CheckAllowed is used by typed workflow-domain modules for actions other
	// than generic stage approval. It deliberately still delegates the decision
	// to authorization-svc; a workflow module must not self-authorize.
	CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType, resourceOwnerPrincipalID string) error
	// CheckDelegation returns nil if delegatePrincipalID is authorized to act
	// as a delegate for approverPrincipalID within legalEntityID. This enables
	// approval delegation where a designated delegate can approve on behalf of
	// the assigned approver.
	CheckDelegation(ctx context.Context, delegatePrincipalID, approverPrincipalID, legalEntityID string) error
}

// HTTPClient implements Client against a real authorization-svc instance.
type HTTPClient struct {
	baseURL string
	http    *http.Client
	log     *zap.Logger
}

// NewHTTPClient constructs an HTTPClient bound to baseURL, e.g.
// "http://authorization-svc:8089" (no trailing slash).
func NewHTTPClient(baseURL string, log *zap.Logger) *HTTPClient {
	return &HTTPClient{
		baseURL: baseURL,
		log:     log,
		// Tight timeout — an approval submission must not stall
		// indefinitely because authorization-svc is slow.
		http: &http.Client{Timeout: 2 * time.Second},
	}
}

type authorizeRequest struct {
	PrincipalID              string `json:"principal_id"`
	LegalEntityID            string `json:"legal_entity_id"`
	ActionType               string `json:"action_type"`
	TenantID                 string `json:"tenant_id,omitempty"`
	ResourceOwnerPrincipalID string `json:"resource_owner_principal_id,omitempty"`
}

type authorizeResponse struct {
	DecisionOutcome string `json:"decision_outcome"`
}

// approvalActionType is the action_type checked against authorization-svc
// for every approval submission. A single, platform-wide action type is
// intentional for v1 — nothing in the docs specifies per-workflow-type
// action codes.
const approvalActionType = "WORKFLOW_APPROVE"

func (c *HTTPClient) CheckApprovalAllowed(ctx context.Context, principalID, legalEntityID, resourceOwnerPrincipalID string) error {
	return c.checkAllowed(ctx, principalID, legalEntityID, approvalActionType, resourceOwnerPrincipalID)
}

func (c *HTTPClient) CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType, resourceOwnerPrincipalID string) error {
	return c.checkAllowed(ctx, principalID, legalEntityID, actionType, resourceOwnerPrincipalID)
}

func (c *HTTPClient) checkAllowed(ctx context.Context, principalID, legalEntityID, actionType string, resourceOwnerPrincipalID string) error {
	// Extract tenant_id from context (set by TenantContext middleware)
	tenantID := ""
	if v := ctx.Value("tenant_id"); v != nil {
		if s, ok := v.(string); ok {
			tenantID = s
		}
	}

	body, err := json.Marshal(authorizeRequest{
		PrincipalID:              principalID,
		LegalEntityID:            legalEntityID,
		ActionType:               actionType,
		TenantID:                 tenantID,
		ResourceOwnerPrincipalID: resourceOwnerPrincipalID,
	})
	if err != nil {
		return fmt.Errorf("marshal authorize request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/authorize", bytes.NewReader(body))
	if err != nil {
		return domain.ErrAuthorizationServiceUnavailable
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Principal-Id", principalID)
	req.Header.Set("X-Legal-Entity-Id", legalEntityID)

	authzRequestID := ""
	authzSourceChannel := "web"
	if env, ok := svcenvelope.FromContext(ctx); ok {
		if env.TenantID != "" {
			req.Header.Set("X-Tenant-Id", env.TenantID)
		}
		if env.RequestID != "" {
			authzRequestID = env.RequestID
		}
		if env.SourceChannel != "" {
			authzSourceChannel = string(env.SourceChannel)
		}
		if env.CorrelationID != "" {
			req.Header.Set("X-Correlation-ID", env.CorrelationID)
		}
		if env.CausationID != "" {
			req.Header.Set("X-Causation-Id", env.CausationID)
		}
		if env.IdempotencyKey != "" {
			req.Header.Set("Idempotency-Key", env.IdempotencyKey)
		}
	}
	if req.Header.Get("X-Tenant-Id") == "" {
		if tid := svcmiddleware.TenantFromContext(ctx); tid != "" {
			req.Header.Set("X-Tenant-Id", tid)
		}
	}
	if authzRequestID == "" {
		authzRequestID = uuid.New().String()
	}
	req.Header.Set("X-Request-Id", authzRequestID)
	req.Header.Set("X-Source-Channel", authzSourceChannel)
	if req.Header.Get("Idempotency-Key") == "" {
		req.Header.Set("Idempotency-Key", "authz-"+authzRequestID)
	}

	// Forward envelope headers for audit trail
	if requestID := ctx.Value("request_id"); requestID != nil {
		req.Header.Set("X-Request-Id", requestID.(string))
	}
	if correlationID := ctx.Value("correlation_id"); correlationID != nil {
		req.Header.Set("X-Correlation-ID", correlationID.(string))
	}
	if sourceChannel := ctx.Value("source_channel"); sourceChannel != nil {
		req.Header.Set("X-Source-Channel", sourceChannel.(string))
	}
	if idempotencyKey := ctx.Value("idempotency_key"); idempotencyKey != nil {
		req.Header.Set("Idempotency-Key", idempotencyKey.(string))
	}

	resp, err := c.http.Do(req)
	if err != nil {
		c.log.Error("authorization-svc unreachable — failing closed", zap.String("principal_id", principalID), zap.Error(err))
		return domain.ErrAuthorizationServiceUnavailable
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		c.log.Error("unexpected response from authorization-svc — failing closed",
			zap.Int("status", resp.StatusCode), zap.ByteString("body", respBody))
		return domain.ErrAuthorizationServiceUnavailable
	}

	var out authorizeResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return domain.ErrAuthorizationServiceUnavailable
	}
	if out.DecisionOutcome != "GRANTED" {
		return domain.ErrAuthorizationDenied
	}
	return nil
}

// CheckDelegation returns nil if delegatePrincipalID is authorized to act
// as a delegate for approverPrincipalID within legalEntityID. This enables
// approval delegation where a designated delegate can approve on behalf of
// the assigned approver.
func (c *HTTPClient) CheckDelegation(ctx context.Context, delegatePrincipalID, approverPrincipalID, legalEntityID string) error {
	// Extract tenant_id from context (set by TenantContext middleware)
	tenantID := ""
	if v := ctx.Value("tenant_id"); v != nil {
		if s, ok := v.(string); ok {
			tenantID = s
		}
	}

	body, err := json.Marshal(map[string]string{
		"delegate_principal_id": delegatePrincipalID,
		"approver_principal_id": approverPrincipalID,
		"legal_entity_id":       legalEntityID,
		"tenant_id":             tenantID,
		"action_type":           "WORKFLOW_DELEGATE",
	})
	if err != nil {
		return fmt.Errorf("marshal delegation check request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/authorize/delegation", bytes.NewReader(body))
	if err != nil {
		return domain.ErrAuthorizationServiceUnavailable
	}
	req.Header.Set("Content-Type", "application/json")

	// Forward envelope headers for audit trail
	if requestID := ctx.Value("request_id"); requestID != nil {
		req.Header.Set("X-Request-Id", requestID.(string))
	}
	if correlationID := ctx.Value("correlation_id"); correlationID != nil {
		req.Header.Set("X-Correlation-ID", correlationID.(string))
	}
	if sourceChannel := ctx.Value("source_channel"); sourceChannel != nil {
		req.Header.Set("X-Source-Channel", sourceChannel.(string))
	}
	if idempotencyKey := ctx.Value("idempotency_key"); idempotencyKey != nil {
		req.Header.Set("Idempotency-Key", idempotencyKey.(string))
	}

	resp, err := c.http.Do(req)
	if err != nil {
		c.log.Error("authorization-svc unreachable for delegation check — failing closed", zap.String("delegate_principal_id", delegatePrincipalID), zap.Error(err))
		return domain.ErrAuthorizationServiceUnavailable
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		c.log.Error("unexpected response from authorization-svc for delegation check — failing closed",
			zap.Int("status", resp.StatusCode), zap.ByteString("body", respBody))
		return domain.ErrAuthorizationServiceUnavailable
	}

	var out authorizeResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return domain.ErrAuthorizationServiceUnavailable
	}
	if out.DecisionOutcome != "GRANTED" {
		return domain.ErrAuthorizationDenied
	}
	return nil
}
