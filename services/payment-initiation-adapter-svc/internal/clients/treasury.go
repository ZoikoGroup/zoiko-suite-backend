// Package clients holds payment-initiation-adapter-svc's narrow,
// fail-closed HTTP clients to peer services — same shape as
// banking-connector-svc's internal/clients/bankaccount.go and
// workflow-svc's internal/documentvault: an interface, a short-timeout
// http.Client, sentinel errors, no silent success on a transport/decode
// failure.
package clients

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"zoiko.io/payment-initiation-adapter-svc/internal/domain"
)

// ErrPayerAccountUnavailable is returned on any network error, non-200
// response, or a tenant mismatch — fail closed, PrepareAttempt must
// never treat this as "the account is fine."
var ErrPayerAccountUnavailable = errors.New("treasury-svc payer account lookup unavailable")

// ErrPayerAccountNotEligible is returned when the account resolves but
// isn't eligible to be a payment source: not ACTIVE, or ownership never
// verified.
var ErrPayerAccountNotEligible = errors.New("payer account is not ACTIVE and ownership-verified")

// ErrTransferFingerprintUnavailable (Wave 11b) is VerifyTransferFingerprint's
// analogue of ErrPayerAccountUnavailable above — any network error,
// non-200 response, or decode failure fetching the live fingerprint.
var ErrTransferFingerprintUnavailable = errors.New("treasury-svc transfer fingerprint lookup unavailable")

type payerAccountRef struct {
	BankAccountID       string `json:"bank_account_id"`
	TenantID            string `json:"tenant_id"`
	AccountStatus       string `json:"account_status"`
	IsOwnershipVerified bool   `json:"is_ownership_verified"`
}

// TreasuryClient is the narrow interface PrepareAttempt depends on to
// verify PayerAccountRef against treasury-svc's real BNK-01 record,
// rather than trusting the caller-supplied PayerAccountVerified flag —
// see domain.go's own doc comment on why that flag alone isn't a real
// check. VerifyTransferFingerprint (Wave 11b) is the BNK-09 analogue of
// clients/authorization.go's VerifyFingerprint, against the same
// treasury-svc dependency this client already has.
type TreasuryClient interface {
	VerifyPayerAccount(ctx context.Context, tenantID, bankAccountID, principalID, correlationID string) error
	VerifyTransferFingerprint(ctx context.Context, tenantID, transferID, fingerprint string) error
}

type liveTreasuryTransferFingerprint struct {
	TransferID  string `json:"transfer_id"`
	Status      string `json:"status"`
	Fingerprint string `json:"fingerprint"`
}

type TreasuryHTTPClient struct {
	baseURL string
	http    *http.Client
}

func NewTreasuryHTTPClient(baseURL string) *TreasuryHTTPClient {
	return &TreasuryHTTPClient{baseURL: strings.TrimRight(baseURL, "/"), http: &http.Client{Timeout: 3 * time.Second}}
}

func (c *TreasuryHTTPClient) VerifyPayerAccount(ctx context.Context, tenantID, bankAccountID, principalID, correlationID string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/v1/treasury/accounts/"+bankAccountID, nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-Tenant-Id", tenantID)
	req.Header.Set("X-Principal-Id", principalID)
	req.Header.Set("X-Correlation-ID", correlationID)

	resp, err := c.http.Do(req)
	if err != nil {
		return ErrPayerAccountUnavailable
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return ErrPayerAccountUnavailable
	}
	var ref payerAccountRef
	if err := json.NewDecoder(resp.Body).Decode(&ref); err != nil {
		return ErrPayerAccountUnavailable
	}
	if ref.TenantID != tenantID {
		return ErrPayerAccountNotEligible
	}
	if ref.AccountStatus != "ACTIVE" || !ref.IsOwnershipVerified {
		return ErrPayerAccountNotEligible
	}
	return nil
}

// VerifyTransferFingerprint (Wave 11b) fetches the live fingerprint for a
// BNK-09 treasury transfer and compares it against the caller-supplied
// one — never trusting it on its own, same fail-closed posture as
// VerifyPayerAccount and clients/authorization.go's VerifyFingerprint.
//
// Status check: SubmitTreasuryPayment computes the fingerprint from the
// transfer while it is still APPROVED (MarkTransferSubmitted runs only
// after this call returns — see treasury-svc's ExecuteTreasuryTransfer),
// so APPROVED is the only live status a legitimate comparison happens
// against; anything else means the transfer moved on (or never was
// approved) and this attempt must not proceed regardless of whether the
// fingerprint string still happens to match.
func (c *TreasuryHTTPClient) VerifyTransferFingerprint(ctx context.Context, tenantID, transferID, fingerprint string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/v1/treasury/transfers/"+transferID+"/fingerprint", nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-Tenant-Id", tenantID)

	resp, err := c.http.Do(req)
	if err != nil {
		return ErrTransferFingerprintUnavailable
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return ErrTransferFingerprintUnavailable
	}
	var live liveTreasuryTransferFingerprint
	if err := json.NewDecoder(resp.Body).Decode(&live); err != nil {
		return ErrTransferFingerprintUnavailable
	}
	if live.Status != "APPROVED" {
		return domain.ErrTreasuryFingerprintMismatch
	}
	if live.Fingerprint != fingerprint {
		return domain.ErrTreasuryFingerprintMismatch
	}
	return nil
}
