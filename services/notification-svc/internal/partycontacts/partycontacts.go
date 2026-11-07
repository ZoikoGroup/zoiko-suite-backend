// Package partycontacts reads a party's verified contact points from tenant-entity-registry-svc (ORG-5 / MDM-01),
// so a communication can be addressed to an external contact that is not a platform principal (S7-C1).
//
// It reads same-tenant data only: the call is made under the sender's own tenant, so another tenant's party is
// not found, and no federation between tenants exists (MDM-I-05, MDM-I-29). Only VERIFIED, in-force contact points
// are asked for; the registry owns verification (maker-checker, evidence). The address is never logged.
package partycontacts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"go.uber.org/zap"
)

// ErrNotFound means the registry has no such party in the tenant.
var ErrNotFound = errors.New("party not found")

// ErrUnavailable means the registry could not answer.
var ErrUnavailable = errors.New("party registry unavailable")

// Contact is one verified, in-force contact point.
type Contact struct {
	ContactPointID string     `json:"contact_point_id"`
	PartyID        string     `json:"party_id"`
	Channel        string     `json:"channel"`
	Value          string     `json:"value"`
	Purpose        string     `json:"purpose"`
	Status         string     `json:"status"`
	ValidFrom      time.Time  `json:"valid_from"`
	ValidTo        *time.Time `json:"valid_to"`
	VerifiedAt     *time.Time `json:"verified_at"`
	EvidenceRef    string     `json:"evidence_ref"`
}

// Client calls tenant-entity-registry-svc.
type Client struct {
	baseURL   string
	principal string
	http      *http.Client
	log       *zap.Logger
}

// NewClient returns a client that asks as the given workload identity, which needs the registry's
// PARTY_CONTACT_READ action in the tenant's scope (an onboarding grant).
func NewClient(baseURL, principal string, timeout time.Duration, log *zap.Logger) *Client {
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	if log == nil {
		log = zap.NewNop()
	}
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), principal: principal, http: &http.Client{Timeout: timeout}, log: log}
}

// VerifiedEmails returns the party's VERIFIED email contact points in force at `at`, newest verification first.
func (c *Client) VerifiedEmails(ctx context.Context, tenantID, correlationID, partyID string, at time.Time) ([]Contact, error) {
	q := url.Values{"channel": {"EMAIL"}, "status": {"VERIFIED"}, "as_of": {at.UTC().Format(time.RFC3339Nano)}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.baseURL+"/v1/parties/"+url.PathEscape(partyID)+"/contact-points?"+q.Encode(), nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Tenant-Id", tenantID)
	req.Header.Set("X-Principal-Id", c.principal)
	req.Header.Set("X-Workload-Id", c.principal)
	req.Header.Set("X-Source-Channel", "system")
	if correlationID != "" {
		req.Header.Set("X-Correlation-ID", correlationID)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		c.log.Warn("tenant-entity-registry-svc unreachable while resolving a party contact", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return nil, ErrNotFound
	default:
		// 401 and 403 are this service lacking the grant: not an answer about the party.
		return nil, fmt.Errorf("%w: registry answered %d", ErrUnavailable, resp.StatusCode)
	}
	var body struct {
		ContactPoints []Contact `json:"contact_points"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return nil, fmt.Errorf("%w: unreadable answer", ErrUnavailable)
	}
	// Belt and braces: the registry is asked for VERIFIED email in force, and this service does not take its word for
	// it. A contact that is not, is dropped rather than addressed.
	out := make([]Contact, 0, len(body.ContactPoints))
	for _, cp := range body.ContactPoints {
		if cp.Status != "VERIFIED" || !strings.EqualFold(cp.Channel, "EMAIL") || cp.Value == "" {
			continue
		}
		if cp.ValidFrom.After(at) || (cp.ValidTo != nil && !at.Before(*cp.ValidTo)) {
			continue
		}
		out = append(out, cp)
	}
	// Newest verification first, so the most recently confirmed address is the one used.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && verifiedAfter(out[j], out[j-1]); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out, nil
}

func verifiedAfter(a, b Contact) bool {
	switch {
	case a.VerifiedAt == nil:
		return false
	case b.VerifiedAt == nil:
		return true
	default:
		return a.VerifiedAt.After(*b.VerifiedAt)
	}
}
