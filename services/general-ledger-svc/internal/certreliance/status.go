package certreliance

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// The states a certification can be in, as general-ledger-svc sees them. The two issuing services say it differently
// (data-quality-svc: ACTIVE, STALE, REVOKED; reconciliation-engine-svc: CERTIFIED, SUPERSEDED, REVOKED) and the client folds
// them into one vocabulary. Only REVOKED is a withdrawal: STALE and SUPERSEDED are not (a superseded certification was replaced
// by a corrected one, a stale one is past its window), so they are recorded and never flagged.
const (
	StateActive     = "ACTIVE"
	StateStale      = "STALE"
	StateSuperseded = "SUPERSEDED"
	StateRevoked    = "REVOKED"
)

// Status is what the issuing service says about a certification now.
type Status struct {
	State        string
	RevocationID string // set when State is REVOKED
	Reason       string // the revocation's reason, set when State is REVOKED
}

type statusBody struct {
	// data-quality-svc
	Status string `json:"status"`
	// reconciliation-engine-svc
	CertificationState string `json:"certification_state"`
	Revocation         *struct {
		RevocationID string `json:"revocation_id"`
		Reason       string `json:"reason"`
	} `json:"revocation"`
}

// CertificationStatus asks the issuing service for the certification's current state (its blast-radius read, which both
// services serve and which is authorized as a plain read of the run). The principal is the caller's, forwarded. An answer this
// client does not recognise is an error: a state is never guessed, because guessing "active" is how a revoked certification
// would go unflagged.
func (c *Client) CertificationStatus(ctx context.Context, tenantID, principalID, correlationID string, ref Ref) (Status, error) {
	base := c.urls[ref.Service]
	if base == "" {
		return Status{}, fmt.Errorf("%w: %s is not configured", ErrUnavailable, ref.Service)
	}
	if !runIDRe.MatchString(ref.RunID) {
		return Status{}, fmt.Errorf("%w: %q", ErrInvalidRef, ref.RunID)
	}
	var path string
	switch ref.Service {
	case ServiceDataQuality:
		path = "/v1/data-quality/runs/" + url.PathEscape(ref.RunID) + "/certification/blast-radius"
	case ServiceReconciliation:
		path = "/v1/reconciliation/runs/" + url.PathEscape(ref.RunID) + "/certification/blast-radius"
	default:
		return Status{}, fmt.Errorf("%w: %q", ErrInvalidRef, ref.Service)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
	if err != nil {
		return Status{}, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Tenant-Id", tenantID)
	req.Header.Set("X-Principal-Id", principalID)
	req.Header.Set("X-Correlation-ID", correlationID)
	resp, err := c.http.Do(req)
	if err != nil {
		return Status{}, fmt.Errorf("%w: %s: %v", ErrUnavailable, ref.Service, err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return Status{}, fmt.Errorf("%w: %s", ErrUnknown, ref)
	case http.StatusUnauthorized, http.StatusForbidden:
		return Status{}, fmt.Errorf("%w: %s", ErrDenied, ref)
	default:
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return Status{}, fmt.Errorf("%w: %s answered HTTP %d", ErrUnavailable, ref.Service, resp.StatusCode)
	}
	var b statusBody
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&b); err != nil {
		return Status{}, fmt.Errorf("%w: %s returned an unreadable answer: %v", ErrUnavailable, ref.Service, err)
	}
	raw := b.Status
	if ref.Service == ServiceReconciliation {
		raw = b.CertificationState
	}
	st := Status{}
	switch raw {
	case "ACTIVE", "CERTIFIED":
		st.State = StateActive
	case "STALE":
		st.State = StateStale
	case "SUPERSEDED":
		st.State = StateSuperseded
	case "REVOKED":
		st.State = StateRevoked
		if b.Revocation == nil || b.Revocation.RevocationID == "" || b.Revocation.Reason == "" {
			return Status{}, fmt.Errorf("%w: %s says REVOKED without saying which revocation or why", ErrUnavailable, ref)
		}
		st.RevocationID, st.Reason = b.Revocation.RevocationID, b.Revocation.Reason
	default:
		return Status{}, fmt.Errorf("%w: %s answered state %q, which is not recognised", ErrUnavailable, ref.Service, raw)
	}
	return st, nil
}
