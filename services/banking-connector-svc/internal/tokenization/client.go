// Package tokenization is this service's client for tokenizing S3
// Restricted values (bank account numbers) before they are stored, and for
// detokenizing them behind separate elevated authorization.
//
// The standard it implements is ZoikoSuite Security, Privacy, Residency &
// Cryptographic Architecture Standard §14 ("Business services use token
// whenever raw value is unnecessary"), its S3 Restricted class (bank details:
// "tokenization where practical; detokenization/decrypt evidence"), SEC-T15
// (detokenization requires additional scoped approval and evidence), SEC-T29
// (a detokenization API given another tenant's token denies it — tenant /
// context binding) and SEC-INV-15 (detokenization creates evidence).
//
// ── PROPOSED CONTRACT — NOT VERIFIED AGAINST key-management-svc ──────────
//
// key-management-svc (owned by another developer) has no tokenize or
// detokenize endpoint today; it only manages key metadata. Everything below
// is this service's PROPOSAL of what that endpoint should look like, written
// so the day it exists, wiring it in is configuration (TOKENIZATION_ENFORCED,
// TOKENIZATION_SERVICE_URL), not a rewrite. Nothing here has been confirmed by
// key-management-svc's owner; if the real contract differs, this file is the
// one place to change. See SPEC_DEVIATIONS.
//
//	POST {TOKENIZATION_SERVICE_URL}/v1/tokenize
//	  headers: X-Tenant-Id (required), X-Principal-Id, X-Correlation-ID
//	  body:    {"value": "<raw>", "classification": "S3_RESTRICTED"}
//	  200:     {"token": "<opaque token>"}
//
//	POST {TOKENIZATION_SERVICE_URL}/v1/detokenize
//	  headers: X-Tenant-Id (required), X-Principal-Id (required), X-Correlation-ID
//	  body:    {"token": "<opaque token>"}
//	  200:     {"value": "<raw>"}
//	  403:     denied — elevated authorization missing (SEC-T15) or the token
//	           is not bound to this tenant/context (SEC-T29)
//
// The tenant travels as a header, not in the body, because SEC-T29 makes it
// the binding the service must check, the same way every other call in this
// platform carries tenant scope. That header is part of the proposal.
//
// ── Fail closed ─────────────────────────────────────────────────────────
//
// Every failure is an error, never a pass-through: an unreachable service, a
// non-200, an undecodable body, an empty token, or a "token" equal to the raw
// value all return ErrTokenizationServiceUnavailable. The caller must refuse
// the write rather than store the raw value — the same posture as
// internal/authz's ErrAuthzServiceUnavailable, and for the same reason: a
// client that degrades to "store it raw" when the service is down turns
// every outage into exactly the exposure tokenization exists to prevent, and
// does so silently.
package tokenization

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
)

// ErrTokenizationServiceUnavailable is returned when the tokenization
// service cannot be reached, or responds with anything other than a
// well-formed 200 carrying a usable result. Callers must treat it as a
// refusal — fail closed, never store or return the raw value instead.
var ErrTokenizationServiceUnavailable = errors.New("tokenization service (key-management-svc) unavailable")

// ErrDetokenizationDenied is returned when the service explicitly refuses a
// detokenization (403): missing elevated authorization (SEC-T15) or a token
// not bound to the caller's tenant/context (SEC-T29).
var ErrDetokenizationDenied = errors.New("detokenization denied")

// ErrInvalidRequest is returned, without calling the service, for a request
// that could never be answered safely: no tenant to bind the token to, or no
// value/token to act on.
var ErrInvalidRequest = errors.New("tokenization request invalid")

// ClassificationRestricted is the classification sent with every value this
// service tokenizes: bank details are S3 Restricted in the standard's
// classification table. The wire spelling is part of the proposal.
const ClassificationRestricted = "S3_RESTRICTED"

// Scope is who is asking, for which tenant: the tenant the token is bound to
// (SEC-T29) and the principal/correlation the service records as evidence
// (SEC-INV-15).
type Scope struct {
	TenantID      string
	PrincipalID   string
	CorrelationID string
}

// Tokenizer is what the store depends on. Client is the real implementation;
// Fake is the deterministic one for tests and local development.
type Tokenizer interface {
	Tokenize(ctx context.Context, scope Scope, classification, value string) (string, error)
	Detokenize(ctx context.Context, scope Scope, token string) (string, error)
}

type Client struct {
	baseURL string
	client  *http.Client
}

var _ Tokenizer = (*Client)(nil)

func NewClient(baseURL string) *Client {
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), client: &http.Client{Timeout: 5 * time.Second}}
}

// NewClientWithHTTPClient is NewClient with a caller-supplied *http.Client —
// for an mTLS transport, as internal/authz does for authorization-svc.
func NewClientWithHTTPClient(baseURL string, httpClient *http.Client) *Client {
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), client: httpClient}
}

type tokenizeRequest struct {
	Value          string `json:"value"`
	Classification string `json:"classification"`
}

type tokenizeResponse struct {
	Token string `json:"token"`
}

type detokenizeRequest struct {
	Token string `json:"token"`
}

type detokenizeResponse struct {
	Value string `json:"value"`
}

// Tokenize returns the token for value, bound to scope.TenantID. It fails
// closed: see the package doc.
func (c *Client) Tokenize(ctx context.Context, scope Scope, classification, value string) (string, error) {
	if scope.TenantID == "" || value == "" || classification == "" {
		return "", ErrInvalidRequest
	}
	var out tokenizeResponse
	if err := c.post(ctx, "/v1/tokenize", scope, tokenizeRequest{Value: value, Classification: classification}, &out); err != nil {
		return "", err
	}
	// A "token" that is the raw value would be stored as if it were safe.
	if out.Token == "" || out.Token == value {
		return "", ErrTokenizationServiceUnavailable
	}
	return out.Token, nil
}

// Detokenize returns the raw value behind token. The service decides whether
// scope may see it (SEC-T15, SEC-T29); a refusal is ErrDetokenizationDenied.
func (c *Client) Detokenize(ctx context.Context, scope Scope, token string) (string, error) {
	if scope.TenantID == "" || scope.PrincipalID == "" || token == "" {
		return "", ErrInvalidRequest
	}
	var out detokenizeResponse
	if err := c.post(ctx, "/v1/detokenize", scope, detokenizeRequest{Token: token}, &out); err != nil {
		return "", err
	}
	if out.Value == "" {
		return "", ErrTokenizationServiceUnavailable
	}
	return out.Value, nil
}

func (c *Client) post(ctx context.Context, path string, scope Scope, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return ErrTokenizationServiceUnavailable
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return ErrTokenizationServiceUnavailable
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-Id", scope.TenantID)
	if scope.PrincipalID != "" {
		req.Header.Set("X-Principal-Id", scope.PrincipalID)
	}
	if scope.CorrelationID != "" {
		req.Header.Set("X-Correlation-ID", scope.CorrelationID)
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return ErrTokenizationServiceUnavailable
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusForbidden && path == "/v1/detokenize" {
		return ErrDetokenizationDenied
	}
	if resp.StatusCode != http.StatusOK {
		return ErrTokenizationServiceUnavailable
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return ErrTokenizationServiceUnavailable
	}
	return nil
}
