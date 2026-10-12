// Package payee is a real HTTP client to payee-banking-identity-svc (ORG-10),
// the authoritative owner of controlled beneficiary banking identity. AP-01
// never stores or caches bank details: GetPayeeReference asks ORG-10 for the
// CURRENT active destination every time, fails closed when ORG-10 is
// unreachable or misbehaves, and reports "no active destination" distinctly.
// There is deliberately no fallback to a stored, stale or invoice-printed
// value (AP-01 failure/degradation semantics).
package payee

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"go.uber.org/zap"
)

var (
	// ErrUnavailable means ORG-10 could not give a trustworthy answer
	// (transport error, non-200/404 status, malformed body). Callers fail closed.
	ErrUnavailable = errors.New("payee-banking-identity-svc unavailable")
	// ErrNoActiveDestination means ORG-10 answered and has no active
	// destination for the party — a real, expected absence.
	ErrNoActiveDestination = errors.New("no active payee destination in ORG-10")
)

// Destination is the subset of ORG-10's PayeeDestination (PascalCase wire
// shape, no json tags on ORG-10's side) that AP-01 exposes. Bank details are
// intentionally not decoded.
type Destination struct {
	DestinationID string    `json:"DestinationID"`
	LegalEntityID string    `json:"LegalEntityID"`
	PartyRef      string    `json:"PartyRef"`
	Status        string    `json:"Status"`
	UpdatedAt     time.Time `json:"UpdatedAt"`
}

// Resolver resolves the active ORG-10 destination for a party.
type Resolver interface {
	GetActiveDestination(ctx context.Context, tenantID, principalID, legalEntityID, partyRef string) (*Destination, error)
}

type HTTPClient struct {
	baseURL string
	http    *http.Client
	log     *zap.Logger
}

func NewHTTPClient(baseURL string, log *zap.Logger) *HTTPClient {
	return &HTTPClient{baseURL: strings.TrimRight(baseURL, "/"), log: log, http: &http.Client{Timeout: 3 * time.Second}}
}

func (c *HTTPClient) GetActiveDestination(ctx context.Context, tenantID, principalID, legalEntityID, partyRef string) (*Destination, error) {
	q := url.Values{"scope": {"DEFAULT"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/org10/parties/"+url.PathEscape(partyRef)+"/active?"+q.Encode(), nil)
	if err != nil {
		return nil, ErrUnavailable
	}
	req.Header.Set("X-Tenant-Id", tenantID)
	// ORG-10 requires X-Principal-Id unconditionally (401 without it).
	req.Header.Set("X-Principal-Id", principalID)

	resp, err := c.http.Do(req)
	if err != nil {
		c.log.Error("payee-banking-identity-svc unreachable — failing closed", zap.Error(err))
		return nil, ErrUnavailable
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrNoActiveDestination
	}
	if resp.StatusCode != http.StatusOK {
		c.log.Error("unexpected response from payee-banking-identity-svc — failing closed", zap.Int("status", resp.StatusCode))
		return nil, ErrUnavailable
	}
	var d Destination
	if err := json.NewDecoder(resp.Body).Decode(&d); err != nil {
		return nil, ErrUnavailable
	}
	// A destination for another legal entity, or one that is not ACTIVE, is
	// not usable for this profile.
	if d.DestinationID == "" || d.LegalEntityID != legalEntityID || (d.Status != "" && d.Status != "ACTIVE") {
		return nil, ErrNoActiveDestination
	}
	return &d, nil
}
