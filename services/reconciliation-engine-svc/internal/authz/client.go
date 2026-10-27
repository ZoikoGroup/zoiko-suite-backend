package authz

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"go.uber.org/zap"

	"zoiko.io/reconciliation-engine-svc/internal/domain"
)

// Client calls authorization-svc's POST /v1/authorize.
//
// It used to call POST /v1/authorization/check, a route authorization-svc does
// not register (its one evaluation route is /v1/authorize): every call got a
// 404, which the fail-closed handling turned into a denial. The same defect was
// fixed the same way in data-quality-svc, data-lineage-svc, global-search-svc
// and semantic-model-svc. Every call is live; there is no decision cache.
type Client struct {
	baseURL string
	logger  *zap.Logger
	client  *http.Client
}

var ErrAuthorizationDenied = &AuthorizationError{Message: "authorization denied"}

// ErrAuthorizationUnavailable is returned when authorization-svc could not be
// reached, answered unexpectedly, or granted without a traceable decision.
// Callers fail closed (503).
var ErrAuthorizationUnavailable = errors.New("authorization service unavailable")

type AuthorizationError struct {
	Message string
}

func (e *AuthorizationError) Error() string {
	return e.Message
}

func NewClient(baseURL string, logger *zap.Logger) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		logger:  logger,
		client:  &http.Client{Timeout: 5 * time.Second},
	}
}

func NewClientWithHTTPClient(baseURL string, logger *zap.Logger, httpClient *http.Client) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		logger:  logger,
		client:  httpClient,
	}
}

type authorizeRequest struct {
	PrincipalID   string `json:"principal_id"`
	LegalEntityID string `json:"legal_entity_id"`
	ActionType    string `json:"action_type"`
}

// authorizeResponse is the part of authorization-svc's /v1/authorize answer
// this service reads.
type authorizeResponse struct {
	DecisionOutcome  string   `json:"decision_outcome"`
	DecisionBasis    string   `json:"decision_basis"`
	AccessDecisionID string   `json:"access_decision_id"`
	Decision         string   `json:"decision"`
	PolicySetVersion string   `json:"policy_set_version"`
	ReasonCodes      []string `json:"reason_codes"`
}

func (c *Client) CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error {
	out, err := c.authorize(ctx, principalID, legalEntityID, actionType)
	if err != nil {
		return err
	}
	if out.DecisionOutcome != "GRANTED" {
		return ErrAuthorizationDenied
	}
	return nil
}

// Decide is CheckAllowed for a command that must record its authority (A12:
// a certification binds its certifier's authority, ZS-CONTROL-001 Invariant
// 9). On GRANTED it returns the decision as authorization-svc recorded it. A
// GRANTED answer that names no access decision cannot be traced, so it is
// treated as authorization-svc being unavailable (fail-closed).
func (c *Client) Decide(ctx context.Context, principalID, legalEntityID, actionType string) (*domain.AuthoritySnapshot, error) {
	out, err := c.authorize(ctx, principalID, legalEntityID, actionType)
	if err != nil {
		return nil, err
	}
	if out.DecisionOutcome != "GRANTED" {
		return nil, ErrAuthorizationDenied
	}
	if out.AccessDecisionID == "" {
		c.logger.Error("authorization-svc granted without an access decision id — failing closed",
			zap.String("principal_id", principalID), zap.String("action_type", actionType))
		return nil, ErrAuthorizationUnavailable
	}
	return &domain.AuthoritySnapshot{
		Source: "authorization-svc", PrincipalID: principalID, LegalEntityID: legalEntityID, ActionType: actionType,
		AccessDecisionID: out.AccessDecisionID, DecisionOutcome: out.DecisionOutcome, Decision: out.Decision,
		DecisionBasis: out.DecisionBasis, PolicySetVersion: out.PolicySetVersion, ReasonCodes: out.ReasonCodes,
		DecidedAt: time.Now().UTC(),
	}, nil
}

// authorize performs one /v1/authorize call. Any transport, status or decoding
// failure is ErrAuthorizationUnavailable.
func (c *Client) authorize(ctx context.Context, principalID, legalEntityID, actionType string) (*authorizeResponse, error) {
	body, err := json.Marshal(authorizeRequest{PrincipalID: principalID, LegalEntityID: legalEntityID, ActionType: actionType})
	if err != nil {
		return nil, ErrAuthorizationUnavailable
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/authorize", bytes.NewReader(body))
	if err != nil {
		return nil, ErrAuthorizationUnavailable
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		c.logger.Error("authorization-svc unreachable — failing closed",
			zap.String("principal_id", principalID), zap.String("action_type", actionType), zap.Error(err))
		return nil, ErrAuthorizationUnavailable
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		c.logger.Error("unexpected response from authorization-svc — failing closed",
			zap.Int("status", resp.StatusCode), zap.ByteString("body", respBody))
		return nil, ErrAuthorizationUnavailable
	}

	var out authorizeResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		c.logger.Error("failed to decode authorization response — failing closed", zap.Error(err))
		return nil, ErrAuthorizationUnavailable
	}
	return &out, nil
}
