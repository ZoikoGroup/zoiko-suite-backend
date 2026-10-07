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

	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/obligations-svc/internal/domain"
	svcenvelope "zoiko.io/obligations-svc/internal/envelope"
)

// Client is the narrow interface the handler depends on.
type Client interface {
	// CheckAllowed returns nil only when authorization-svc explicitly GRANTS
	// actionType for principalID within legalEntityID.
	CheckAllowed(ctx context.Context, principalID, legalEntityID, tenantID, actionType, correlationID string) error
}

// The four mutating actions this service exposes. One action per route rather
// than a single blanket OBLIGATION_WRITE: closing an obligation and raising one
// are different authorities, and a register that cannot tell them apart cannot
// express "may record, may not close".
//
// ActionApplicabilityDecide is separate for the same reason and is the strongest
// of the four in practice: an applicability decision is the record of WHETHER a
// statutory obligation binds an entity at all, and everything downstream —
// filings, evidence, aging — is derived from it. "May raise an obligation" and
// "may decide it does not apply" are emphatically not the same authority.
const (
	ActionObligationCreate       = "OBLIGATION_CREATE"
	ActionObligationStatusUpdate = "OBLIGATION_STATUS_UPDATE"
	ActionFilingRequirementAdd   = "FILING_REQUIREMENT_CREATE"
	ActionApplicabilityDecide    = "APPLICABILITY_DECISION_RECORD"
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
	// TenantID is the pre-header fallback authorization-svc's resolveTenantScope
	// still accepts. The X-Tenant-Id header set below is what its mandatory §4
	// envelope check actually looks for; this is sent alongside it so the two
	// calling conventions can never disagree about which tenant is asking.
	TenantID string `json:"tenant_id,omitempty"`
}

// authorizeResponse is the shape authorization-svc actually sends: it always
// answers 200 and signals the decision through decision_outcome
// ("GRANTED" | "DENIED"). There is no boolean field — a client that decoded one
// would read Go's zero value false for every request and deny everything, which
// is precisely how financial-close-svc lost its entire write surface.
type authorizeResponse struct {
	DecisionOutcome string `json:"decision_outcome"`
}

func (c *HTTPClient) CheckAllowed(ctx context.Context, principalID, legalEntityID, tenantID, actionType, correlationID string) error {
	body, err := json.Marshal(authorizeRequest{
		PrincipalID:   principalID,
		LegalEntityID: legalEntityID,
		ActionType:    actionType,
		TenantID:      tenantID,
	})
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/authorize", bytes.NewReader(body))
	if err != nil {
		return domain.ErrAuthorizationUnavailable
	}
	req.Header.Set("Content-Type", "application/json")
	if correlationID == "" {
		correlationID = uuid.NewString()
	}
	req.Header.Set("X-Correlation-ID", correlationID)
	// authorization-svc enforces its own §4 canonical input contract on every
	// route, including /v1/authorize, and this client used to send none of it:
	// only Content-Type and an optional X-Correlation-ID. tenant_id and
	// actor_subject_id (X-Tenant-Id / X-Principal-Id) missing answers 401 before
	// the request reaches RBAC evaluation at all; request_id, source_channel,
	// idempotency_key and (since this is a POST, so RequiredOnWrite promotes it)
	// legal_entity_id missing answers 400. Either way this client folded the
	// refusal into ErrAuthorizationUnavailable and obligations-svc failed
	// closed on it — every write was refused, on a caller who was in fact
	// authorized, because the internal call never identified itself.
	// tenant_id/legal_entity_id are also sent in the body for the pre-header
	// calling convention authorization-svc still accepts, but the headers are
	// what its envelope check actually looks for.
	if tenantID != "" {
		req.Header.Set("X-Tenant-Id", tenantID)
	}
	if principalID != "" {
		req.Header.Set("X-Principal-Id", principalID)
	}
	if legalEntityID != "" {
		req.Header.Set("X-Legal-Entity-Id", legalEntityID)
	}
	req.Header.Set("X-Request-Id", uuid.NewString())
	req.Header.Set("X-Source-Channel", "system")
	// Each authorization check is its own evaluation, not a resubmission of a
	// prior one, so it gets its own key rather than reusing the caller's —
	// reusing it would make an unrelated authorize call collide with the
	// material write's own idempotency record.
	req.Header.Set("Idempotency-Key", uuid.NewString())

	// authorization-svc's resolveTenantScope prefers the verified X-Tenant-Id
	// header over the request body, and — per its own doc comment — silently
	// narrows to global-only SoD rules when neither is present, rather than
	// refusing. Forwarding it here is what makes this tenant's
	// segregation-of-duties rules actually apply to decisions made through
	// this service, not just the globally-applicable ones. Same defect,
	// same fix as board-resolutions-svc.
	req.Header.Set("X-Principal-Id", principalID)
	req.Header.Set("X-Legal-Entity-Id", legalEntityID)

	authzRequestID := middleware.GetReqID(ctx)
	authzSourceChannel := "system"
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
		if correlationID == "" && env.CorrelationID != "" {
			req.Header.Set("X-Correlation-ID", env.CorrelationID)
		}
		if env.CausationID != "" {
			req.Header.Set("X-Causation-Id", env.CausationID)
		}
	}
	req.Header.Set("X-Request-Id", authzRequestID)
	req.Header.Set("X-Source-Channel", authzSourceChannel)
	req.Header.Set("Idempotency-Key", authzRequestID+":"+actionType)

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
