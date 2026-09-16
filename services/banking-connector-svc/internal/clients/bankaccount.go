// Package clients holds banking-connector-svc's narrow, fail-closed HTTP
// clients to peer services — same shape as workflow-svc's
// internal/documentvault and asset-management-svc's internal/clients:
// an interface, a short-timeout http.Client, sentinel errors, no silent
// success on a transport/decode failure.
package clients

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"context"
)

// ErrBankAccountUnavailable is returned on any network error, non-200
// response, or a tenant mismatch in the decoded body — fail closed, a
// caller must never treat this as "the account is fine."
var ErrBankAccountUnavailable = errors.New("treasury-svc bank account lookup unavailable")

// ErrBankAccountInvalid is returned when the referenced bank_account_id
// resolves to a real row that nonetheless doesn't belong to the
// requesting tenant.
var ErrBankAccountInvalid = errors.New("bank_account_id does not resolve to a valid account for this tenant")

type BankAccountRef struct {
	BankAccountID     string `json:"bank_account_id"`
	TenantID          string `json:"tenant_id"`
	LegalEntityID     string `json:"legal_entity_id"`
	OperationalStatus string `json:"account_status"`
}

// BankAccountClient is the narrow interface banking-connector-svc depends
// on for BNK-02's link to BNK-01.
type BankAccountClient interface {
	// VerifyBankAccount resolves bankAccountID via treasury-svc's real
	// GET /v1/treasury/accounts/{id} and confirms it belongs to
	// tenantID — the concrete check behind CompleteConnectionAuthorization
	// binding a connection to a real, owned account rather than an
	// arbitrary caller-supplied string.
	VerifyBankAccount(ctx context.Context, tenantID, bankAccountID, principalID, correlationID string) (*BankAccountRef, error)
}

type BankAccountHTTPClient struct {
	baseURL string
	http    *http.Client
}

func NewBankAccountHTTPClient(baseURL string) *BankAccountHTTPClient {
	return &BankAccountHTTPClient{baseURL: strings.TrimRight(baseURL, "/"), http: &http.Client{Timeout: 3 * time.Second}}
}

func (c *BankAccountHTTPClient) VerifyBankAccount(ctx context.Context, tenantID, bankAccountID, principalID, correlationID string) (*BankAccountRef, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/v1/treasury/accounts/"+bankAccountID, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Tenant-Id", tenantID)
	req.Header.Set("X-Principal-Id", principalID)
	req.Header.Set("X-Correlation-ID", correlationID)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, ErrBankAccountUnavailable
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, ErrBankAccountUnavailable
	}
	var ref BankAccountRef
	if err := json.NewDecoder(resp.Body).Decode(&ref); err != nil {
		return nil, ErrBankAccountUnavailable
	}
	if ref.TenantID != tenantID {
		return nil, ErrBankAccountInvalid
	}
	return &ref, nil
}
