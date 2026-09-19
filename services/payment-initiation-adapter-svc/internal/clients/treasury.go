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
)

// ErrPayerAccountUnavailable is returned on any network error, non-200
// response, or a tenant mismatch — fail closed, PrepareAttempt must
// never treat this as "the account is fine."
var ErrPayerAccountUnavailable = errors.New("treasury-svc payer account lookup unavailable")

// ErrPayerAccountNotEligible is returned when the account resolves but
// isn't eligible to be a payment source: not ACTIVE, or ownership never
// verified.
var ErrPayerAccountNotEligible = errors.New("payer account is not ACTIVE and ownership-verified")

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
// check.
type TreasuryClient interface {
	VerifyPayerAccount(ctx context.Context, tenantID, bankAccountID, principalID, correlationID string) error
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
