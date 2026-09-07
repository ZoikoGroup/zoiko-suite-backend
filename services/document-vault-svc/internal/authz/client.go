// Package authz calls authorization-svc and fails closed.
//
// This service had no authorization of any kind. Every route — including
// GET /v1/documents/{id}/content, which returns the document bytes — answered
// anything that could reach the port, on a vault whose own schema classifies
// its contents PUBLIC / INTERNAL / CONFIDENTIAL / RESTRICTED.
//
// It lives in its own package rather than inline in cmd/server because a client
// that only exists in package main cannot be tested: every handler test
// replaces it with a double, so nothing ever exercises the parsing. That is how
// financial-close-svc shipped a client decoding an "allowed" boolean field
// authorization-svc has never sent — always absent, always false, every check
// denied. A fail-closed check that always fails closed looks exactly like a
// permission nobody granted, from both sides. See client_test.go, which drives
// a real server with the real shape.
package authz

import (
	svcenvelope "zoiko.io/document-vault-svc/internal/envelope"
	"github.com/go-chi/chi/v5/middleware"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"zoiko.io/document-vault-svc/internal/domain"
)

// Actions.
//
// READ and DOWNLOAD are deliberately separate, and the split is not cosmetic:
// the access log already records METADATA and DOWNLOAD as different access
// types, because knowing a document exists and reading its bytes are different
// disclosures. Authorization mirrors that distinction rather than collapsing
// both into one "may see this document" grant.
//
// ACCESS_LOG_READ is separate again. The log says who read what and when; on a
// governed vault that is itself sensitive, and it is the record an investigator
// consults — it should not fall out of ordinary read access to the document.
const (
	ActionDocumentCreate        = "DOCUMENT_CREATE"
	ActionDocumentRead          = "DOCUMENT_READ"
	ActionDocumentDownload      = "DOCUMENT_DOWNLOAD"
	ActionDocumentVersionCreate = "DOCUMENT_VERSION_CREATE"
	ActionDocumentAccessLogRead = "DOCUMENT_ACCESS_LOG_READ"
)

// Client is the interface the handler depends on, so tests can substitute a
// double without reaching for the network.
type Client interface {
	CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error
}

// decisionCacheTTL bounds how long a GRANTED/DENIED decision may be reused
// locally before authorization-svc is asked again.
//
// Doc 05 §6.5 anticipates this: "For Tier 0 and latency-sensitive services,
// policy and authorization evaluation may use high-speed distributed
// enforcement patterns, including local policy caches... provided policy source
// remains centralized, policy provenance is auditable, stale decision risk is
// bounded, fail-safe behavior is defined." This constant is that bound — short
// enough that a revocation propagates within one cache generation, long enough
// to absorb the repeat checks a single page load produces.
//
// Only real GRANTED/DENIED decisions are cached. An unreachable or misbehaving
// authorization-svc is never cached: that would turn one transient outage into
// a standing permit-or-deny for every later caller on this instance, which
// defeats fail-closed.
const decisionCacheTTL = 5 * time.Second

type cachedDecision struct {
	deniedErr error
	expiresAt time.Time
}

type HTTPClient struct {
	baseURL string
	client  *http.Client
	log     *zap.Logger

	cacheMu     sync.Mutex
	cache       map[string]cachedDecision
	cacheWrites int
}

func NewHTTPClient(baseURL string, log *zap.Logger) *HTTPClient {
	return &HTTPClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		client:  &http.Client{Timeout: 5 * time.Second},
		log:     log,
		cache:   make(map[string]cachedDecision),
	}
}

type authorizeRequest struct {
	PrincipalID   string `json:"principal_id"`
	LegalEntityID string `json:"legal_entity_id"`
	ActionType    string `json:"action_type"`
}

// authorizeResponse is the shape authorization-svc actually sends: it always
// answers 200 and signals the decision through decision_outcome
// ("GRANTED" | "DENIED"). There is no boolean field.
type authorizeResponse struct {
	DecisionOutcome string `json:"decision_outcome"`
}

// CheckAllowed returns nil only on an explicit GRANTED. Every other outcome —
// a denial, a transport failure, a non-200, an undecodable body, or a decision
// string this client does not recognise — is an error, and callers must treat
// all of them as a refusal.
func (c *HTTPClient) CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error {
	key := principalID + "|" + legalEntityID + "|" + actionType

	if decision, hit := c.lookupCache(key); hit {
		return decision
	}

	err := c.checkAllowedLive(ctx, principalID, legalEntityID, actionType)

	// Cache the decision itself, never an unavailable outcome. This
	// deliberately excludes the unrecognised-decision case below: it returns
	// ErrAuthzServiceUnavailable precisely because it is not a decision this
	// service was given, so it must not be remembered as one.
	if err == nil || errors.Is(err, domain.ErrAuthorizationDenied) {
		c.storeCache(key, err)
	}

	return err
}

func (c *HTTPClient) lookupCache(key string) (error, bool) {
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

// storeCache records a real GRANTED/DENIED decision. Every 1000th write sweeps
// expired entries so a long-lived instance with many distinct
// (principal, entity, action) combinations does not grow the map unboundedly.
func (c *HTTPClient) storeCache(key string, decision error) {
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

func (c *HTTPClient) checkAllowedLive(ctx context.Context, principalID, legalEntityID, actionType string) error {
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
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	// authorization-svc validates the same canonical envelope contract this
	// service does and answers 400 envelope_incomplete without it. A non-200 is
	// treated as unavailable below, so an unforwarded envelope turned EVERY
	// gated write into a 503 that reads like an outage rather than a missing
	// header. Same defect, same fix as 3c618c2 (HR) and dbf6e45 (notification).
	//
	// The values are the CALLER's, taken from the envelope the middleware
	// already parsed into this request's context. Minting fresh ones would
	// satisfy the contract and lose the only thing it is for: a decision in
	// access_decision_log traceable to the request that caused it.
	req.Header.Set("X-Principal-Id", principalID)
	req.Header.Set("X-Legal-Entity-Id", legalEntityID)

	authzRequestID := middleware.GetReqID(ctx)
	// Service-to-service. "system" is in the contract's accepted set; the
	// caller's own channel replaces it when the envelope carries one.
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
		if env.CorrelationID != "" {
			req.Header.Set("X-Correlation-ID", env.CorrelationID)
		}
		if env.CausationID != "" {
			req.Header.Set("X-Causation-Id", env.CausationID)
		}
	}
	req.Header.Set("X-Request-Id", authzRequestID)
	req.Header.Set("X-Source-Channel", authzSourceChannel)
	// One decision per (request, action): an inbound request may authorize
	// several actions, and each is its own decision to record.
	req.Header.Set("Idempotency-Key", authzRequestID+":"+actionType)
	resp, err := c.client.Do(req)
	if err != nil {
		c.log.Error("failed to call authorization-svc", zap.Error(err))
		return domain.ErrAuthzServiceUnavailable
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return domain.ErrAuthzServiceUnavailable
	}

	var res authorizeResponse
	// A decode error used to be returned raw in the services this was modelled
	// on, which the handler's error mapping did not recognise as either a
	// denial or an outage — so a malformed response produced a 503 with the
	// JSON parser's message in it.
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return domain.ErrAuthzServiceUnavailable
	}

	switch res.DecisionOutcome {
	case "GRANTED":
		return nil
	case "DENIED":
		return domain.ErrAuthorizationDenied
	default:
		// Includes the empty string — the zero value, which is what a renamed
		// field or a changed envelope looks like from here. Refused as an
		// unusable answer rather than reported as a denial, because it is not a
		// decision this service was given.
		return fmt.Errorf("%w: unrecognised decision_outcome %q",
			domain.ErrAuthzServiceUnavailable, res.DecisionOutcome)
	}
}
