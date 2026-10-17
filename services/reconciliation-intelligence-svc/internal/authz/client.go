package authz

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
	"go.uber.org/zap"

	svcenvelope "zoiko.io/reconciliation-intelligence-svc/internal/envelope"
)

// ErrAuthzServiceUnavailable is returned when authorization-svc cannot be
// reached, or responds with anything other than a well-formed 200. Callers
// must treat this as a denial — fail closed, never silently allow.
var ErrAuthzServiceUnavailable = errors.New("authorization-svc unavailable")

// ErrAuthorizationDenied is returned when authorization-svc explicitly
// denies the requested action.
var ErrAuthorizationDenied = errors.New("authorization denied")

// ErrTenantMissing is returned when no verified tenant was supplied. The
// check is refused rather than made under a made-up tenant (b75655a2): a
// decision evaluated against a tenant the caller was never verified to
// belong to could be GRANTED.
var ErrTenantMissing = errors.New("tenant is required for an authorization check")

// decisionCacheTTL bounds how long a GRANTED/DENIED decision from
// authorization-svc may be reused locally before it is asked again.
//
// Doc 05 (Security Architecture Specification) §6.5 anticipates exactly
// this cost: "For Tier 0 and latency-sensitive services, policy and
// authorization evaluation may use high-speed distributed enforcement
// patterns, including local policy caches... provided policy source
// remains centralized, policy provenance is auditable, stale decision
// risk is bounded, fail-safe behavior is defined." This constant is that
// bound — short enough that a permission revocation or role change
// propagates within one cache generation, long enough to absorb the
// repeat checks a single user action or request burst produces.
//
// Only real GRANTED/DENIED decisions are ever cached. An unreachable or
// misbehaving authorization-svc is never cached — that would turn one
// transient outage into a standing permit-or-deny for every subsequent
// caller on this instance, which defeats fail-closed.
const decisionCacheTTL = 5 * time.Second

type cachedDecision struct {
	deniedErr error
	expiresAt time.Time
}

type Client struct {
	authzURL   string
	httpClient *http.Client
	logger     *zap.Logger

	cacheMu     sync.Mutex
	cache       map[string]cachedDecision
	cacheWrites int
}

func NewClient(authzURL string, logger *zap.Logger) *Client {
	return &Client{
		authzURL:   authzURL,
		httpClient: &http.Client{Timeout: 5 * time.Second},
		logger:     logger,
		cache:      make(map[string]cachedDecision),
	}
}

// NewClientWithHTTPClient is NewClient but with a caller-supplied
// *http.Client — used for the mTLS pilot, where the client's Transport
// already carries this service's leaf certificate and trusts
// authorization-svc's CA (see internal/mtls.NewClientHTTPClient).
func NewClientWithHTTPClient(authzURL string, httpClient *http.Client, logger *zap.Logger) *Client {
	return &Client{
		authzURL:   authzURL,
		httpClient: httpClient,
		logger:     logger,
		cache:      make(map[string]cachedDecision),
	}
}

// Authorize is retained for backward compatibility but is unused anywhere
// in this service — it never called authorization-svc and always granted.
func (c *Client) Authorize(ctx context.Context, tenantID, userID, action, resource string) (bool, error) {
	// Delegated to Governance Plane Authorization Service
	return true, nil
}

type authorizeRequest struct {
	PrincipalID   string `json:"principal_id"`
	LegalEntityID string `json:"legal_entity_id"`
	ActionType    string `json:"action_type"`
}

type authorizeResponse struct {
	DecisionOutcome string `json:"decision_outcome"`
}

// CheckAllowed asks authorization-svc whether principalID may perform
// actionType within legalEntityID, in tenantID. It fails closed: any
// transport error, non-200 status, or undecodable body is treated as
// ErrAuthzServiceUnavailable, never as an implicit grant.
func (c *Client) CheckAllowed(ctx context.Context, tenantID, principalID, legalEntityID, actionType string) error {
	if tenantID == "" {
		return ErrTenantMissing
	}
	// The tenant is part of the key: the same principal, entity and action
	// in two tenants are two different questions and must never share a
	// cached decision.
	key := tenantID + "|" + principalID + "|" + legalEntityID + "|" + actionType

	if decision, hit := c.lookupCache(key); hit {
		return decision
	}

	err := c.checkAllowedLive(ctx, tenantID, principalID, legalEntityID, actionType)

	// Cache the decision itself (GRANTED or DENIED), never an unavailable
	// outcome — see the doc comment on decisionCacheTTL.
	if err == nil || errors.Is(err, ErrAuthorizationDenied) {
		c.storeCache(key, err)
	}

	return err
}

// lookupCache returns the cached decision for key and whether it is still
// within decisionCacheTTL. An expired entry is evicted on read.
func (c *Client) lookupCache(key string) (error, bool) {
	c.cacheMu.Lock()
	defer c.cacheMu.Unlock()

	d, ok := c.cache[key]
	if !ok {
		return nil, false
	}
	if time.Now().After(d.expiresAt) {
		delete(c.cache, key)
		return nil, false
	}
	return d.deniedErr, true
}

// storeCache records a real GRANTED/DENIED decision. Every 1000th write
// sweeps expired entries so a long-lived instance with many distinct
// (principal, entity, action) combinations doesn't grow the map
// unboundedly between reads of the same key.
func (c *Client) storeCache(key string, decision error) {
	c.cacheMu.Lock()
	defer c.cacheMu.Unlock()

	c.cache[key] = cachedDecision{deniedErr: decision, expiresAt: time.Now().Add(decisionCacheTTL)}

	c.cacheWrites++
	if c.cacheWrites%1000 == 0 {
		now := time.Now()
		for k, v := range c.cache {
			if now.After(v.expiresAt) {
				delete(c.cache, k)
			}
		}
	}
}

// checkAllowedLive is the real, uncached call to authorization-svc.
func (c *Client) checkAllowedLive(ctx context.Context, tenantID, principalID, legalEntityID, actionType string) error {
	body, err := json.Marshal(authorizeRequest{
		PrincipalID:   principalID,
		LegalEntityID: legalEntityID,
		ActionType:    actionType,
	})
	if err != nil {
		return ErrAuthzServiceUnavailable
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.authzURL+"/v1/authorize", bytes.NewReader(body))
	if err != nil {
		return ErrAuthzServiceUnavailable
	}
	req.Header.Set("Content-Type", "application/json")
	setEnvelopeHeaders(ctx, req, tenantID, principalID, legalEntityID)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		if c.logger != nil {
			c.logger.Error("authorization-svc unreachable — failing closed", zap.Error(err))
		}
		return ErrAuthzServiceUnavailable
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return ErrAuthzServiceUnavailable
	}

	var out authorizeResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return ErrAuthzServiceUnavailable
	}
	if out.DecisionOutcome != "GRANTED" {
		return ErrAuthorizationDenied
	}
	return nil
}

// setEnvelopeHeaders forwards the canonical service input envelope
// (ZS-ARCH-SVC-001 §4) that authorization-svc validates: tenant, actor,
// request id, correlation id and source channel are mandatory on every
// request, and the legal entity names what the decision is about. Without
// them each check was admitted only as a reported contract violation, and its
// access_decision_log row could not be traced to the request that caused it.
//
// Same pattern as internal/bankrec (and financial-close-svc's
// checkAllowedLive, 7abd0f8a / d6c2cc38): the values are the CALLER's, taken
// from the envelope the middleware parsed into this request's context; an id
// is minted only when neither the request-id middleware nor the envelope
// supplied one.
//
// No Idempotency-Key: authorization-svc deliberately does not treat
// /v1/authorize as a material write (its handler.MaterialWrite) — two
// identical questions must produce two decision-log entries.
func setEnvelopeHeaders(ctx context.Context, req *http.Request, tenantID, principalID, legalEntityID string) {
	req.Header.Set(svcenvelope.HeaderTenantID, tenantID)
	req.Header.Set(svcenvelope.HeaderActorSubjectID, principalID)
	if legalEntityID != "" {
		req.Header.Set(svcenvelope.HeaderLegalEntityID, legalEntityID)
	}

	requestID := middleware.GetReqID(ctx)
	// Service-to-service. "system" is in the contract's accepted set; the
	// caller's own channel replaces it when the envelope carries one.
	sourceChannel := string(svcenvelope.ChannelSystem)
	correlationID := ""
	if env, ok := svcenvelope.FromContext(ctx); ok {
		if env.RequestID != "" {
			requestID = env.RequestID
		}
		if env.SourceChannel != "" {
			sourceChannel = string(env.SourceChannel)
		}
		// An external channel (import, integration) obliges source_system;
		// forwarding the channel without it would violate the contract.
		if env.SourceSystem != "" {
			req.Header.Set(svcenvelope.HeaderSourceSystem, env.SourceSystem)
		}
		if env.CorrelationID != "" {
			correlationID = env.CorrelationID
		}
		if env.CausationID != "" {
			req.Header.Set(svcenvelope.HeaderCausationID, env.CausationID)
		}
	}
	if requestID == "" {
		requestID = uuid.NewString()
	}
	if correlationID == "" {
		correlationID = uuid.NewString()
	}
	req.Header.Set(svcenvelope.HeaderRequestID, requestID)
	req.Header.Set(svcenvelope.HeaderCorrelationID, correlationID)
	req.Header.Set(svcenvelope.HeaderSourceChannel, sourceChannel)
}
