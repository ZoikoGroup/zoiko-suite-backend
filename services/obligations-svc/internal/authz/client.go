// Package authz confirms, via authorization-svc, that a principal may mutate
// the obligation register.
//
// This service shipped with NO authorization at all. Its config carried the
// comment "admin writes do not call Authorization Service yet — it doesn't
// exist. Deliberate, documented deferral matching policy-svc's and
// governance-decision-log-svc's precedent." Both halves of that are now stale:
// authorization-svc exists and is live on :8089, and both services cited as
// precedent have since been wired to it. What was left was an open write
// surface on a statutory compliance register — anything able to reach the port
// could raise an obligation, close one, or file against one.
//
// Fail-closed, like every other client on this platform: an unreachable
// authorization-svc REFUSES the mutation, it never silently permits it.
package authz

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"go.uber.org/zap"

	"zoiko.io/obligations-svc/internal/domain"
)

// Client is the narrow interface the handler depends on.
type Client interface {
	// CheckAllowed returns nil only when authorization-svc explicitly GRANTS
	// actionType for principalID within legalEntityID.
	CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType, correlationID string) error
}

// The three mutating actions this service exposes. One action per route rather
// than a single blanket OBLIGATION_WRITE: closing an obligation and raising one
// are different authorities, and a register that cannot tell them apart cannot
// express "may record, may not close".
const (
	ActionObligationCreate       = "OBLIGATION_CREATE"
	ActionObligationStatusUpdate = "OBLIGATION_STATUS_UPDATE"
	ActionFilingRequirementAdd   = "FILING_REQUIREMENT_CREATE"
)

type HTTPClient struct {
	baseURL string
	http    *http.Client
	log     *zap.Logger
}

func NewHTTPClient(baseURL string, log *zap.Logger) *HTTPClient {
	return &HTTPClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		http:    &http.Client{Timeout: 5 * time.Second},
		log:     log,
	}
}

type authorizeRequest struct {
	PrincipalID   string `json:"principal_id"`
	LegalEntityID string `json:"legal_entity_id"`
	ActionType    string `json:"action_type"`
}

// authorizeResponse is the shape authorization-svc actually sends: it always
// answers 200 and signals the decision through decision_outcome
// ("GRANTED" | "DENIED"). There is no boolean field — a client that decoded one
// would read Go's zero value false for every request and deny everything, which
// is precisely how financial-close-svc lost its entire write surface.
type authorizeResponse struct {
	DecisionOutcome string `json:"decision_outcome"`
}

func (c *HTTPClient) CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType, correlationID string) error {
	body, err := json.Marshal(authorizeRequest{
		PrincipalID:   principalID,
		LegalEntityID: legalEntityID,
		ActionType:    actionType,
	})
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/authorize", bytes.NewReader(body))
	if err != nil {
		return domain.ErrAuthorizationUnavailable
	}
	req.Header.Set("Content-Type", "application/json")
	if correlationID != "" {
		req.Header.Set("X-Correlation-ID", correlationID)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		c.log.Error("authorization-svc unreachable — failing closed",
			zap.String("principal_id", principalID),
			zap.String("action_type", actionType),
			zap.Error(err))
		return domain.ErrAuthorizationUnavailable
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return domain.ErrAuthorizationUnavailable
	}

	var out authorizeResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return domain.ErrAuthorizationUnavailable
	}

	switch out.DecisionOutcome {
	case "GRANTED":
		return nil
	case "DENIED":
		return domain.ErrAuthorizationDenied
	default:
		// Includes the empty string — the zero value, which is what a renamed
		// field or a changed envelope produces. Refused as an unusable answer
		// rather than reported as a denial, because it is not a decision this
		// service was given.
		return fmt.Errorf("%w: unrecognised decision_outcome %q",
			domain.ErrAuthorizationUnavailable, out.DecisionOutcome)
	}
}
