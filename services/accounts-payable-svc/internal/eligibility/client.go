// Package eligibility is the client for AP-01 (supplier-financial-profile-svc)
// supplier eligibility -- contract #1:
//
//	GET /ap01/supplier-financial-profiles/eligibility?legal_entity_id=&supplier_ref=
//	200 {"profile_id","supplier_ref","legal_entity_id","status","version",
//	     "is_on_hold","eligible_for_new_commitments","reason"}; 404 when no profile.
//
// The owner implements the endpoint in parallel; until it exists every call
// fails closed (a 404 or an error refuses the supplier), never "eligible".
package eligibility

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"

	"zoiko.io/accounts-payable-svc/internal/svcclient"
)

var (
	// ErrNoProfile: AP-01 knows no profile for this supplier/entity (404).
	ErrNoProfile = errors.New("no supplier financial profile")
	// ErrUnavailable: AP-01 could not answer; fail closed.
	ErrUnavailable = errors.New("supplier eligibility unavailable")
)

type Eligibility struct {
	ProfileID                 string `json:"profile_id"`
	SupplierRef               string `json:"supplier_ref"`
	LegalEntityID             string `json:"legal_entity_id"`
	Status                    string `json:"status"`
	Version                   int    `json:"version"`
	IsOnHold                  bool   `json:"is_on_hold"`
	EligibleForNewCommitments bool   `json:"eligible_for_new_commitments"`
	Reason                    string `json:"reason"`

	// WithholdingRef is an OPTIONAL extension of contract #1: when the supplier
	// profile carries a withholding reference, AP requires a verified TAX
	// withholding result before approval. Absent means no withholding required.
	WithholdingRef string `json:"withholding_ref,omitempty"`
}

type Checker interface {
	Check(ctx context.Context, caller svcclient.Caller, legalEntityID, supplierRef string) (*Eligibility, error)
}

type HTTPClient struct{ c *svcclient.Client }

func NewHTTPClient(baseURL string) *HTTPClient { return &HTTPClient{c: svcclient.New(baseURL)} }

func (h *HTTPClient) Check(ctx context.Context, caller svcclient.Caller, legalEntityID, supplierRef string) (*Eligibility, error) {
	q := url.Values{"legal_entity_id": {legalEntityID}, "supplier_ref": {supplierRef}}
	status, body, err := h.c.Do(ctx, caller, "GET", "/ap01/supplier-financial-profiles/eligibility?"+q.Encode(), nil)
	if err != nil {
		return nil, ErrUnavailable
	}
	switch status {
	case 200:
	case 404:
		// A 404 from a service that has not shipped the endpoint yet is
		// indistinguishable from "no profile"; both refuse the supplier, which is
		// the fail-closed outcome.
		return nil, ErrNoProfile
	default:
		return nil, ErrUnavailable
	}
	var e Eligibility
	if err := json.Unmarshal(body, &e); err != nil {
		return nil, ErrUnavailable
	}
	return &e, nil
}

// Eligible reports whether new commitments (invoices) may be raised: ACTIVE,
// not held, and the service says so. Every condition must hold.
func (e *Eligibility) Eligible() bool {
	return e != nil && e.EligibleForNewCommitments && !e.IsOnHold && e.Status == "ACTIVE"
}
