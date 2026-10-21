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

	idenv "zoiko.io/identity-envelope"
	"zoiko.io/workflow-svc/internal/domain"
	svcenvelope "zoiko.io/workflow-svc/internal/envelope"
	svcmiddleware "zoiko.io/workflow-svc/internal/middleware"
)

// ApprovalAction is the action every approval decision is authorized under.
// One action, with the workflow type and the stage's approver role passed as
// attributes (Governance audit H-2, decision 21 Oct 2026): a rule pack that
// wants per-type approval authority writes an ABAC rule on the attribute
// instead of the platform minting one action code per workflow type.
const ApprovalAction = "WORKFLOW_APPROVE"

// Attribute names sent with an approval authorization.
const (
	AttrWorkflowType = "workflow_type"
	AttrApproverRole = "approver_role"
	AttrStageOrder   = "stage_order"
)

// Client is the narrow interface the handler depends on.
type Client interface {
	// CheckApprovalAllowed asks whether principalID may decide an approval
	// in legalEntityID. resourceOwnerPrincipalID is the initiator, for
	// authorization-svc's own-object SoD check; attrs carries the workflow
	// type and approver role. On GRANTED it returns the authority snapshot
	// the decision is recorded under. domain.ErrAuthorizationDenied on any
	// other outcome; domain.ErrAuthorizationServiceUnavailable when
	// authorization-svc cannot answer — callers fail closed on both.
	CheckApprovalAllowed(ctx context.Context, principalID, legalEntityID, resourceOwnerPrincipalID string, attrs map[string]string) (domain.ActionAuthority, error)
	// CheckAllowed is used by typed workflow-domain modules for actions other
	// than generic stage approval. It deliberately still delegates the decision
	// to authorization-svc; a workflow module must not self-authorize.
	CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType, resourceOwnerPrincipalID string) error
	// CheckAllowedWithAuthority is CheckAllowed for lifecycle commands: it
	// returns the same authority snapshot an approval records, so cancel /
	// escalate / invalidate ledger records carry the access decision too.
	CheckAllowedWithAuthority(ctx context.Context, principalID, legalEntityID, actionType, resourceOwnerPrincipalID string) (domain.ActionAuthority, error)
	// CheckDelegation asks whether delegatePrincipalID holds WORKFLOW_APPROVE
	// by a live delegation FROM approverPrincipalID — the approver the stage
	// names, not any delegator. Returns the delegation id on success.
	CheckDelegation(ctx context.Context, delegatePrincipalID, approverPrincipalID, legalEntityID string) (domain.ActionAuthority, error)
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
	PrincipalID              string            `json:"principal_id"`
	LegalEntityID            string            `json:"legal_entity_id"`
	ActionType               string            `json:"action_type"`
	TenantID                 string            `json:"tenant_id,omitempty"`
	ResourceOwnerPrincipalID string            `json:"resource_owner_principal_id,omitempty"`
	Attributes               map[string]string `json:"attributes,omitempty"`
}

type authorizeResponse struct {
	DecisionOutcome  string `json:"decision_outcome"`
	AccessDecisionID string `json:"access_decision_id"`
	DecisionBasis    string `json:"decision_basis"`
}

func (c *HTTPClient) CheckApprovalAllowed(ctx context.Context, principalID, legalEntityID, resourceOwnerPrincipalID string, attrs map[string]string) (domain.ActionAuthority, error) {
	out, err := c.authorize(ctx, authorizeRequest{
		PrincipalID: principalID, LegalEntityID: legalEntityID, ActionType: ApprovalAction,
		ResourceOwnerPrincipalID: resourceOwnerPrincipalID, Attributes: attrs,
	})
	if err != nil {
		return domain.ActionAuthority{}, err
	}
	return domain.ActionAuthority{AccessDecisionID: out.AccessDecisionID, Basis: out.DecisionBasis}, nil
}

func (c *HTTPClient) CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType, resourceOwnerPrincipalID string) error {
	_, err := c.CheckAllowedWithAuthority(ctx, principalID, legalEntityID, actionType, resourceOwnerPrincipalID)
	return err
}

func (c *HTTPClient) CheckAllowedWithAuthority(ctx context.Context, principalID, legalEntityID, actionType, resourceOwnerPrincipalID string) (domain.ActionAuthority, error) {
	out, err := c.authorize(ctx, authorizeRequest{
		PrincipalID: principalID, LegalEntityID: legalEntityID, ActionType: actionType,
		ResourceOwnerPrincipalID: resourceOwnerPrincipalID,
	})
	if err != nil {
		return domain.ActionAuthority{}, err
	}
	return domain.ActionAuthority{AccessDecisionID: out.AccessDecisionID, Basis: out.DecisionBasis}, nil
}

// tenantOf is the verified tenant of the request. It used to be read from
// ctx.Value("tenant_id"), a key nothing ever set, so the body's tenant_id was
// always empty and authorization-svc evaluated outside any tenant.
func tenantOf(ctx context.Context) string {
	if env, ok := svcenvelope.FromContext(ctx); ok && env.TenantID != "" {
		return env.TenantID
	}
	return svcmiddleware.TenantFromContext(ctx)
}

// stamp sets the headers every authorization-svc call carries: the verified
// identity forwarded (envelope, principal, tenant), and the trace.
func stamp(ctx context.Context, req *http.Request, principalID, legalEntityID, tenantID string) {
	req.Header.Set("Content-Type", "application/json")
	idenv.Forward(ctx, req)
	if req.Header.Get("X-Principal-Id") == "" {
		req.Header.Set("X-Principal-Id", principalID)
	}
	if tenantID != "" {
		req.Header.Set("X-Tenant-Id", tenantID)
	}
	req.Header.Set("X-Legal-Entity-Id", legalEntityID)

	requestID, channel := "", "web"
	if env, ok := svcenvelope.FromContext(ctx); ok {
		requestID = env.RequestID
		if env.SourceChannel != "" {
			channel = string(env.SourceChannel)
		}
		if env.CorrelationID != "" {
			req.Header.Set("X-Correlation-ID", env.CorrelationID)
		}
		if env.CausationID != "" {
			req.Header.Set("X-Causation-Id", env.CausationID)
		}
	}
	if requestID == "" {
		requestID = uuid.New().String()
	}
	req.Header.Set("X-Request-Id", requestID)
	req.Header.Set("X-Source-Channel", channel)
	// An authorization question is not the caller's command: its own
	// Idempotency-Key would make authorization-svc replay the first answer
	// for a different action or principal under the same key.
	req.Header.Set("Idempotency-Key", "authz-"+uuid.New().String())
}

func (c *HTTPClient) authorize(ctx context.Context, in authorizeRequest) (authorizeResponse, error) {
	in.TenantID = tenantOf(ctx)
	body, err := json.Marshal(in)
	if err != nil {
		return authorizeResponse{}, fmt.Errorf("marshal authorize request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/authorize", bytes.NewReader(body))
	if err != nil {
		return authorizeResponse{}, domain.ErrAuthorizationServiceUnavailable
	}
	stamp(ctx, req, in.PrincipalID, in.LegalEntityID, in.TenantID)

	resp, err := c.http.Do(req)
	if err != nil {
		c.log.Error("authorization-svc unreachable — failing closed", zap.String("principal_id", in.PrincipalID), zap.Error(err))
		return authorizeResponse{}, domain.ErrAuthorizationServiceUnavailable
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		c.log.Error("unexpected response from authorization-svc — failing closed",
			zap.Int("status", resp.StatusCode), zap.ByteString("body", respBody))
		return authorizeResponse{}, domain.ErrAuthorizationServiceUnavailable
	}

	var out authorizeResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return authorizeResponse{}, domain.ErrAuthorizationServiceUnavailable
	}
	if out.DecisionOutcome != "GRANTED" {
		return out, domain.ErrAuthorizationDenied
	}
	return out, nil
}

// CheckDelegation asks authorization-svc's delegated-access evaluation the
// delegator-specific question. Before 21 Oct 2026 it asked for
// WORKFLOW_DELEGATE (an action no delegation confers) and then matched the
// approver as a substring of a basis that names only the FIRST delegator, so
// a delegate covering for two approvers was refused for the second, and an
// approver id that was a prefix of another's matched it.
func (c *HTTPClient) CheckDelegation(ctx context.Context, delegatePrincipalID, approverPrincipalID, legalEntityID string) (domain.ActionAuthority, error) {
	if approverPrincipalID == "" {
		return domain.ActionAuthority{}, domain.ErrAuthorizationDenied
	}
	tenantID := tenantOf(ctx)
	body, err := json.Marshal(map[string]string{
		"principal_id":           delegatePrincipalID,
		"legal_entity_id":        legalEntityID,
		"tenant_id":              tenantID,
		"action_type":            ApprovalAction,
		"delegator_principal_id": approverPrincipalID,
	})
	if err != nil {
		return domain.ActionAuthority{}, fmt.Errorf("marshal delegation check request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/delegated-access/evaluate", bytes.NewReader(body))
	if err != nil {
		return domain.ActionAuthority{}, domain.ErrAuthorizationServiceUnavailable
	}
	stamp(ctx, req, delegatePrincipalID, legalEntityID, tenantID)
	req.Header.Set("X-Purpose-Context", "workflow_delegation_evaluation")

	resp, err := c.http.Do(req)
	if err != nil {
		c.log.Error("authorization-svc unreachable for delegation check — failing closed", zap.String("delegate_principal_id", delegatePrincipalID), zap.Error(err))
		return domain.ActionAuthority{}, domain.ErrAuthorizationServiceUnavailable
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		c.log.Error("unexpected response from authorization-svc for delegation check — failing closed",
			zap.Int("status", resp.StatusCode), zap.ByteString("body", respBody))
		return domain.ActionAuthority{}, domain.ErrAuthorizationServiceUnavailable
	}

	var out struct {
		HasDelegatedAccess bool   `json:"has_delegated_access"`
		Basis              string `json:"basis"`
		DelegationID       string `json:"delegation_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return domain.ActionAuthority{}, domain.ErrAuthorizationServiceUnavailable
	}
	// An authorization-svc that predates the delegator-specific question
	// ignores delegator_principal_id and answers for any delegator; without a
	// delegation id there is no proof the answer is about this approver.
	if !out.HasDelegatedAccess || out.DelegationID == "" || out.Basis != "delegated:from="+approverPrincipalID {
		return domain.ActionAuthority{}, domain.ErrAuthorizationDenied
	}
	return domain.ActionAuthority{Basis: out.Basis, DelegationID: out.DelegationID}, nil
}
