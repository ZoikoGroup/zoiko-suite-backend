// Wave 11a's real fingerprint verification: for an attempt whose
// AuthorizationSource is domain.AuthorizationSourcePaymentAuthorization,
// PrepareAttempt no longer trusts the caller-supplied
// AuthorizationFingerprint outright — it independently re-fetches the
// live ProposalFingerprint from payment-authorization-svc (AP-10) by
// AuthorizationID and compares, mirroring the exact live-refetch-and-
// compare idiom payment-authorization-svc already uses internally
// (ApprovePayment/ConsumePaymentAuthorization re-checking against
// payment-proposal-svc). Same narrow-client shape as treasury.go.
package clients

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"zoiko.io/payment-initiation-adapter-svc/internal/domain"
)

type liveAuthorization struct {
	AuthorizationID     string `json:"AuthorizationID"`
	Status              string `json:"Status"`
	ProposalFingerprint string `json:"ProposalFingerprint"`
}

type getAuthorizationResponse struct {
	Authorization liveAuthorization `json:"authorization"`
}

// AuthorizationClient is the narrow interface PrepareAttempt depends on
// to verify an AuthorizationSourcePaymentAuthorization fingerprint.
type AuthorizationClient interface {
	VerifyFingerprint(ctx context.Context, tenantID, authorizationID, fingerprint string) error
}

type AuthorizationHTTPClient struct {
	baseURL string
	http    *http.Client
}

func NewAuthorizationHTTPClient(baseURL string) *AuthorizationHTTPClient {
	return &AuthorizationHTTPClient{baseURL: strings.TrimRight(baseURL, "/"), http: &http.Client{Timeout: 3 * time.Second}}
}

// VerifyFingerprint fetches the live authorization by ID and compares its
// current ProposalFingerprint against fingerprint — never trusting the
// caller-supplied value on its own. Fails closed on any transport error,
// non-200 response, or decode failure.
//
// Status check: payment-run-svc's real sequence is CreateRun (captures
// the fingerprint) -> LockPaymentRun (consumes the authorization,
// APPROVED->CONSUMED) -> SubmitPaymentRun (calls PrepareAttempt) — so by
// the time this runs, the authorization is legitimately CONSUMED, not
// APPROVED. Only REJECTED/REVOKED/EXPIRED/INVALIDATED are treated as a
// mismatch: each means the authorization is dead (INVALIDATED
// specifically means a protected-field mismatch was already detected
// upstream), and this attempt must not proceed regardless of whether the
// fingerprint string still happens to match.
func (c *AuthorizationHTTPClient) VerifyFingerprint(ctx context.Context, tenantID, authorizationID, fingerprint string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/ap10/authorizations/"+authorizationID, nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-Tenant-Id", tenantID)

	resp, err := c.http.Do(req)
	if err != nil {
		return domain.ErrAuthorizationServiceUnavailable
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return domain.ErrAuthorizationServiceUnavailable
	}
	var parsed getAuthorizationResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return domain.ErrAuthorizationServiceUnavailable
	}
	live := parsed.Authorization
	switch live.Status {
	case "APPROVED", "CONSUMED":
		// ok — a legitimate live state to verify a fingerprint against.
	default:
		return domain.ErrAuthorizationFingerprintMismatch
	}
	if live.ProposalFingerprint != fingerprint {
		return domain.ErrAuthorizationFingerprintMismatch
	}
	return nil
}
