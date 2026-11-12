// Package tokenization is this service's client for tokenizing S3
// Restricted values (payee account identifiers) before they are stored, and
// for detokenizing them behind separate elevated authorization.
//
// The standard it implements is ZoikoSuite Security, Privacy, Residency &
// Cryptographic Architecture Standard §14 ("Business services use token
// whenever raw value is unnecessary"), its S3 Restricted class (bank details:
// "tokenization where practical; detokenization/decrypt evidence"), SEC-T15
// (detokenization requires additional scoped approval and evidence), SEC-T29
// (a detokenization API given another tenant's token denies it — tenant /
// context binding) and SEC-INV-15 (detokenization creates evidence).
//
// ── CONTRACT — implemented by key-management-svc ─────────────────────────
//
// This file was first written against a PROPOSED contract, before
// key-management-svc had a vault. key-management-svc now implements it
// (internal/handler/tokenization.go, migration 000001 there), and this client
// matches that implementation:
//
//	POST {TOKENIZATION_SERVICE_URL}/v1/tokenize
//	  headers: X-Tenant-Id, X-Principal-Id, X-Legal-Entity-Id (all required),
//	           X-Correlation-ID, plus the platform envelope (below)
//	  body:    {"value": "<raw>", "classification": "S3_RESTRICTED"}
//	  200:     {"token": "<opaque token>"}  — a random UUID, the vault row id
//
//	POST {TOKENIZATION_SERVICE_URL}/v1/detokenize
//	  headers: as tokenize
//	  body:    {"token": "<opaque token>"}
//	  200:     {"value": "<raw>"}
//	  403:     denied — no TOKEN_DETOKENIZE grant (SEC-T15), or the token is
//	           not this tenant's (SEC-T29). A token that does not exist gets
//	           the same 403, so a refusal never reveals another tenant's tokens.
//
// Paths, bodies, the tenant/principal/correlation headers and the meaning of a
// detokenize 403 are exactly what the proposal said. What changed, all on this
// side:
//
//   - The platform envelope. key-management-svc, like every service here,
//     refuses a POST that lacks the ZS-ARCH-SVC-001 §4 envelope while in its
//     default write-strict mode — the proposal's three headers alone were
//     refused with 400 before reaching the vault. So the client also sends
//     X-Request-Id, X-Source-Channel: system, X-Purpose-Context and an
//     Idempotency-Key, as this platform's other service-to-service clients do.
//   - Legal entity. The vault stores it with each token and authorizes
//     TOKEN_CREATE at it, and later TOKEN_DETOKENIZE at the stored value, so
//     Scope carries it and the client refuses a request without one.
//   - Principal on tokenize. The vault requires X-Principal-Id on both
//     routes (it records who created each token), so tokenize now refuses a
//     scope with no principal instead of sending one optionally.
//
// The tenant travels as a header, not in the body, because SEC-T29 makes it
// the binding the service must check, the same way every other call in this
// platform carries tenant scope. The vault refuses a tenant that is not a
// UUID; every tenant the platform issues is one.
//
// Enforcement stays off (TOKENIZATION_ENFORCED=false) until the enablement
// prerequisites in SPEC_DEVIATIONS are done.
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

	"github.com/google/uuid"
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
// that could never be answered safely: no tenant to bind the token to, no
// principal or legal entity to authorize, or no value/token to act on.
var ErrInvalidRequest = errors.New("tokenization request invalid")

// ClassificationRestricted is the classification sent with every value this
// service tokenizes: bank details are S3 Restricted in the standard's
// classification table. key-management-svc accepts S2_CONFIDENTIAL and
// S3_RESTRICTED and refuses anything else.
const ClassificationRestricted = "S3_RESTRICTED"

// Scope is who is asking, for which tenant: the tenant the token is bound to
// (SEC-T29), the principal/correlation the service records as evidence
// (SEC-INV-15), and the legal entity the value belongs to — the scope the
// vault authorizes TOKEN_CREATE and TOKEN_DETOKENIZE at.
type Scope struct {
	TenantID      string
	PrincipalID   string
	LegalEntityID string
	CorrelationID string
}

// Purpose contexts sent as X-Purpose-Context (§4 "governed sensitive access").
const (
	purposeTokenize   = "bank_detail_tokenization"
	purposeDetokenize = "bank_detail_detokenization"
)

// bindable reports whether scope names everything the vault needs to bind,
// authorize and attribute a request.
func (s Scope) bindable() bool {
	return s.TenantID != "" && s.PrincipalID != "" && s.LegalEntityID != ""
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
// for example one whose Transport carries an mTLS client certificate.
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
	if !scope.bindable() || value == "" || classification == "" {
		return "", ErrInvalidRequest
	}
	var out tokenizeResponse
	if err := c.post(ctx, "/v1/tokenize", purposeTokenize, scope, tokenizeRequest{Value: value, Classification: classification}, &out); err != nil {
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
	if !scope.bindable() || token == "" {
		return "", ErrInvalidRequest
	}
	var out detokenizeResponse
	if err := c.post(ctx, "/v1/detokenize", purposeDetokenize, scope, detokenizeRequest{Token: token}, &out); err != nil {
		return "", err
	}
	if out.Value == "" {
		return "", ErrTokenizationServiceUnavailable
	}
	return out.Value, nil
}

func (c *Client) post(ctx context.Context, path, purpose string, scope Scope, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return ErrTokenizationServiceUnavailable
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return ErrTokenizationServiceUnavailable
	}
	// A fresh request id per call doubles as the Idempotency-Key: the client
	// does not retry, so each call is its own operation.
	requestID := uuid.NewString()
	correlationID := scope.CorrelationID
	if correlationID == "" {
		correlationID = requestID
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-Id", scope.TenantID)
	req.Header.Set("X-Principal-Id", scope.PrincipalID)
	req.Header.Set("X-Legal-Entity-Id", scope.LegalEntityID)
	req.Header.Set("X-Correlation-ID", correlationID)
	req.Header.Set("X-Request-Id", requestID)
	req.Header.Set("X-Source-Channel", "system")
	req.Header.Set("X-Purpose-Context", purpose)
	req.Header.Set("Idempotency-Key", requestID)

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
