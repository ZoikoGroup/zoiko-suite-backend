package ar10

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"

	"zoiko.io/accounts-receivable-svc/internal/kit"
	"zoiko.io/accounts-receivable-svc/internal/svcclient"
)

// DestinationInfo is the verification record of a refund destination. AR stores the reference, the fingerprint and this evidence;
// raw bank details (account identifier, institution) are never read into or stored by AR.
type DestinationInfo struct {
	Ref         string
	PartyRef    string
	Currency    string
	Fingerprint string
	Status      string // payee-banking-identity-svc: CANDIDATE|VERIFICATION_PENDING|VERIFIED|APPROVAL_PENDING|ACTIVE|SUSPENDED|SUPERSEDED
	Evidence    map[string]any
}

// Active: only an ACTIVE (verified and approved) destination can receive money.
func (d *DestinationInfo) Active() bool { return d.Status == "ACTIVE" }

// DestinationVerifier validates a refund destination reference against its owner. A nil verifier fails closed (503).
type DestinationVerifier interface {
	// Verify returns the destination, or a *kit.Problem (404 NOT_FOUND when it does not exist, 503 when unavailable).
	Verify(ctx context.Context, c kit.Caller, destinationRef string) (*DestinationInfo, error)
}

// HTTPDestinationVerifier is the payee-banking-identity-svc (ORG-10) client: GET /org10/destinations/{id}. The response's account
// identifier is deliberately not decoded.
type HTTPDestinationVerifier struct{ c *svcclient.Client }

func NewHTTPDestinationVerifier(baseURL string) *HTTPDestinationVerifier {
	return &HTTPDestinationVerifier{c: svcclient.New(baseURL)}
}

type destinationWire struct {
	DestinationID           string
	PartyRef                string
	Currency                string
	Fingerprint             string
	Status                  string
	VerificationMethod      string
	VerificationEvidenceRef string
	VerifiedByPrincipalID   string
	VerifiedAt              *string
	ApprovedByPrincipalID   string
}

func (v *HTTPDestinationVerifier) Verify(ctx context.Context, c kit.Caller, ref string) (*DestinationInfo, error) {
	status, body, err := v.c.Do(ctx, c.SvcCaller(), "GET", "/org10/destinations/"+url.PathEscape(ref), nil)
	if err != nil {
		return nil, kit.Unavailable("the payee banking identity service is unavailable: the refund destination cannot be verified")
	}
	switch {
	case status == 404:
		return nil, kit.NotFound("refund destination not found")
	case status != 200:
		return nil, kit.Unavailable("the payee banking identity service could not answer for the refund destination")
	}
	var w destinationWire
	if json.Unmarshal(body, &w) != nil {
		return nil, kit.Unavailable("the payee banking identity service returned an unreadable answer")
	}
	ev := map[string]any{"source": "payee-banking-identity-svc", "status": w.Status, "verification_method": w.VerificationMethod,
		"verification_evidence_ref": w.VerificationEvidenceRef, "verified_by": w.VerifiedByPrincipalID, "approved_by": w.ApprovedByPrincipalID}
	if w.VerifiedAt != nil {
		ev["verified_at"] = *w.VerifiedAt
	}
	return &DestinationInfo{Ref: ref, PartyRef: w.PartyRef, Currency: w.Currency, Fingerprint: w.Fingerprint, Status: w.Status, Evidence: ev}, nil
}

// checkDestination validates a destination for a refund of the given customer and currency.
func checkDestination(d *DestinationInfo, customerPartyID, currency string) error {
	switch {
	case !d.Active():
		return kit.Unprocessable(kit.CodeValidationFailed, "the refund destination is "+d.Status+": only an ACTIVE verified destination can receive a refund")
	case d.Fingerprint == "":
		return kit.Unprocessable(kit.CodeValidationFailed, "the refund destination has no verification fingerprint")
	case customerPartyID != "" && d.PartyRef != "" && !strings.EqualFold(d.PartyRef, customerPartyID):
		return kit.Unprocessable(kit.CodeValidationFailed, "the refund destination does not belong to the customer who paid")
	case d.Currency != "" && !strings.EqualFold(d.Currency, currency):
		return kit.Unprocessable(kit.CodeValidationFailed, "the refund destination currency does not match the refund currency")
	}
	return nil
}
