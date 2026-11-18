// Package governancelog verifies that a contract activation is backed by a
// real, GRANTED governance decision before the contract is allowed to go
// ACTIVE — closing the gap where activation previously required only a
// signature (signed_by), with no reference to any approving decision at
// all.
//
// How the decision log is called (D2-2.F07, 11 Nov 2026). The read used to
// carry X-Tenant-Id and nothing else. governance-decision-log-svc now runs
// IDENTITY_VERIFY=require and enforces read authorization, so a bare read is
// refused 401. The verification is the service's own integrity check, not a
// person's read of the ledger, so it reads as this service's principal
// (GOVERNANCE_LOG_PRINCIPAL_ID, default svc-contract-lifecycle, granted
// GOVERNANCE_DECISION_READ) over mutual TLS: the decision log's :8450
// listener verifies this service's platform certificate and admits it as a
// peer workload (IDENTITY_PEERS) vouching for that principal. Every read
// also carries the full input envelope (request id, correlation id, source
// channel, purpose), so it no longer violates the decision log's contract
// (register D-04).
package governancelog

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	svcenvelope "zoiko.io/contract-lifecycle-svc/internal/envelope"
)

// Sentinel errors. Callers must fail closed: any error returned means the
// activation must be refused, same doctrine as the authz.Client in this
// service.
var (
	// ErrDecisionNotFound means governance-decision-log-svc has no such
	// decision for this tenant.
	ErrDecisionNotFound = errors.New("governance decision not found")
	// ErrDecisionNotGranted means the decision exists but was not GRANTED,
	// or does not match the expected legal_entity_id/action_type.
	ErrDecisionNotGranted = errors.New("governance decision was not granted for this action")
	// ErrServiceUnavailable means governance-decision-log-svc could not be
	// reached, timed out, refused this service, or returned an unexpected
	// response.
	ErrServiceUnavailable = errors.New("governance decision log unavailable")
)

// DefaultPrincipalID is the principal this service reads the ledger as.
const DefaultPrincipalID = "svc-contract-lifecycle"

// PurposeContext is the purpose sent on every read (the decision log requires
// a purpose on reads, decision D-06).
const PurposeContext = "contract_activation_verification"

// Client verifies governance decisions against governance-decision-log-svc.
type Client struct {
	httpClient  *http.Client
	baseURL     string
	principalID string
}

// NewClient creates a plain-HTTP client (local runs where the decision log
// does not require a verified identity).
func NewClient(baseURL string) *Client {
	return NewClientWithHTTPClient(baseURL, &http.Client{Timeout: 5 * time.Second}, DefaultPrincipalID)
}

// NewClientWithHTTPClient uses httpClient — the mTLS client presenting this
// service's workload certificate — against baseURL (the :8450 listener), and
// reads as principalID.
func NewClientWithHTTPClient(baseURL string, httpClient *http.Client, principalID string) *Client {
	if strings.TrimSpace(principalID) == "" {
		principalID = DefaultPrincipalID
	}
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), httpClient: httpClient, principalID: principalID}
}

type decisionResponse struct {
	DecisionID    string `json:"decision_id"`
	LegalEntityID string `json:"legal_entity_id"`
	ActionType    string `json:"action_type"`
	Outcome       string `json:"outcome"`
}

func newID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// VerifyGranted fetches decisionID from governance-decision-log-svc scoped
// to tenantID, and confirms it was GRANTED for actionType against
// legalEntityID. Fails closed: unreachable service, non-200 response, a
// decode failure, or any field mismatch all result in a non-nil error —
// activation must never proceed on an ambiguous verification result.
func (c *Client) VerifyGranted(ctx context.Context, tenantID, decisionID, legalEntityID, actionType string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/v1/decisions/"+url.PathEscape(decisionID), nil)
	if err != nil {
		return err
	}
	h := req.Header
	h.Set("X-Tenant-Id", tenantID)
	h.Set("X-Principal-Id", c.principalID)
	h.Set("X-Request-Id", newID())
	correlation := ""
	if env, ok := svcenvelope.FromContext(ctx); ok {
		correlation = env.CorrelationID
	}
	if correlation == "" {
		correlation = newID()
	}
	h.Set("X-Correlation-ID", correlation)
	h.Set("X-Source-Channel", "system")
	h.Set("X-Purpose-Context", PurposeContext)
	if legalEntityID != "" {
		h.Set("X-Legal-Entity-Id", legalEntityID)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return ErrServiceUnavailable
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound {
		return ErrDecisionNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return ErrServiceUnavailable
	}

	var d decisionResponse
	if err := json.NewDecoder(resp.Body).Decode(&d); err != nil {
		return ErrServiceUnavailable
	}

	if d.Outcome != "GRANTED" || d.LegalEntityID != legalEntityID || d.ActionType != actionType {
		return ErrDecisionNotGranted
	}
	return nil
}
