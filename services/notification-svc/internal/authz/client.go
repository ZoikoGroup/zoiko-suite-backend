// Package authz calls authorization-svc and fails closed.
//
// It lives here rather than inline in cmd/server because a client that only
// exists in package main cannot be tested: every handler test replaces it with
// a double, so nothing ever exercised the parsing. That is precisely how
// financial-close-svc shipped a client decoding an "allowed" boolean field
// authorization-svc has never sent — the field was always absent, always
// false, and every authorization check denied. A fail-closed check that always
// fails closed looks exactly like a permission nobody granted, from both
// sides. See client_test.go, which drives a real server with the real shape.
package authz

import (
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

	"zoiko.io/notification-svc/internal/domain"
)

// decisionCacheTTL bounds how long a GRANTED/DENIED decision from
// authorization-svc may be reused locally before it is asked again.
//
// Doc 05 (Security Architecture Specification) §6.5 anticipates exactly this
// cost: "For Tier 0 and latency-sensitive services, policy and authorization
// evaluation may use high-speed distributed enforcement patterns, including
// local policy caches... provided policy source remains centralized, policy
// provenance is auditable, stale decision risk is bounded, fail-safe behavior
// is defined." This constant is that bound — short enough that a permission
// revocation or role change propagates within one cache generation, long
// enough to absorb the repeat checks a single user action produces.
//
// Only real GRANTED/DENIED decisions are ever cached. An unreachable or
// misbehaving authorization-svc is never cached — that would turn one transient
// outage into a standing permit-or-deny for every subsequent caller on this
// instance, which defeats fail-closed.
const decisionCacheTTL = 5 * time.Second

type cachedDecision struct {
	deniedErr error
	expiresAt time.Time
}

type Client struct {
	baseURL string
	client  *http.Client
	log     *zap.Logger

	cacheMu     sync.Mutex
	cache       map[string]cachedDecision
	cacheWrites int
}

func NewClient(baseURL string, log *zap.Logger) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		client:  &http.Client{Timeout: 5 * time.Second},
		log:     log,
		cache:   make(map[string]cachedDecision),
	}
}

// NewClientWithHTTPClient is NewClient but with a caller-supplied
// *http.Client — used for the mTLS pilot, where the client's Transport
// already carries this service's leaf certificate and trusts
// authorization-svc's CA (see internal/mtls.NewClientHTTPClient).
func NewClientWithHTTPClient(baseURL string, httpClient *http.Client, log *zap.Logger) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		client:  httpClient,
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
//
// Decisions are cached for decisionCacheTTL; see the constant for why, and for
// why only real decisions are eligible.
func (c *Client) CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error {
	key := principalID + "|" + legalEntityID + "|" + actionType

	if decision, hit := c.lookupCache(key); hit {
		return decision
	}

	err := c.checkAllowedLive(ctx, principalID, legalEntityID, actionType)

	// Cache the decision itself (GRANTED or DENIED), never an unavailable
	// outcome. Note this deliberately excludes the unrecognised-decision case
	// below: that returns ErrAuthzServiceUnavailable precisely because it is
	// not a decision this service was given, so it must not be remembered as
	// one.
	if err == nil || errors.Is(err, domain.ErrAuthorizationDenied) {
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

// storeCache records a real GRANTED/DENIED decision. Every 1000th write sweeps
// expired entries so a long-lived instance with many distinct
// (principal, entity, action) combinations does not grow the map unboundedly
// between reads of the same key.
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
func (c *Client) checkAllowedLive(ctx context.Context, principalID, legalEntityID, actionType string) error {
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
	// A decode error used to be returned raw, which the handler's error
	// mapping did not recognise as either a denial or an outage — so a
	// malformed response produced a 503 with the JSON parser's message in it.
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
		// unusable answer rather than reported as a denial, because it is not
		// a decision this service was given.
		return fmt.Errorf("%w: unrecognised decision_outcome %q",
			domain.ErrAuthzServiceUnavailable, res.DecisionOutcome)
	}
}
