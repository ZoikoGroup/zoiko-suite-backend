// Package paydeps holds the narrow, fail-closed clients WFP-07 uses to resolve everything
// about a payment from its authority instead of from payroll data: the beneficiary's ACTIVE
// destination (payee-banking-identity-svc), the worker's payee reference (employee-master-svc),
// the paying account (treasury-svc), the approval threshold (policy-svc) and the Banking
// handoff (payment-initiation-adapter-svc). An unreachable answer is "unknown", never "valid".
package paydeps

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"

	"zoiko.io/payroll-run-svc/internal/domain"
)

func newHTTP(c *http.Client) *http.Client {
	if c == nil {
		return &http.Client{Timeout: 5 * time.Second}
	}
	return c
}

// ── payee master (ORG-10) ────────────────────────────────────────────────────

type PayeeMaster struct {
	baseURL string
	http    *http.Client
}

func NewPayeeMaster(baseURL string, c *http.Client) *PayeeMaster {
	return &PayeeMaster{baseURL: strings.TrimRight(baseURL, "/"), http: newHTTP(c)}
}

// destinationWire is payee-banking-identity-svc's PayeeDestination (PascalCase: it declares
// no JSON tags). Only identities and digests are read; the account number is never used.
type destinationWire struct {
	DestinationID         string
	LegalEntityID         string
	PartyRef              string
	Status                string
	Currency              string
	Fingerprint           string
	UpdatedAt             time.Time
	ProposedByPrincipalID string
	VerifiedByPrincipalID string
	ApprovedByPrincipalID string
}

// ActiveDestination returns the payee's ACTIVE DEFAULT destination, pinned by id and version.
func (p *PayeeMaster) ActiveDestination(ctx context.Context, tenantID, principalID, legalEntityID, payeeRef string) (*domain.PayeeDestination, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		p.baseURL+"/org10/parties/"+url.PathEscape(payeeRef)+"/active?scope=DEFAULT", nil)
	if err != nil {
		return nil, domain.ErrPayeeUnavailable
	}
	req.Header.Set("X-Tenant-Id", tenantID)
	req.Header.Set("X-Principal-Id", principalID)
	req.Header.Set("X-Legal-Entity-Id", legalEntityID)
	resp, err := p.http.Do(req)
	if err != nil {
		return nil, domain.ErrPayeeUnavailable
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, domain.ErrNoActivePayee
	case resp.StatusCode != http.StatusOK:
		return nil, domain.ErrPayeeUnavailable
	}
	var d destinationWire
	if err := json.NewDecoder(resp.Body).Decode(&d); err != nil {
		return nil, domain.ErrPayeeUnavailable
	}
	if d.DestinationID == "" || d.Status != "ACTIVE" || d.UpdatedAt.IsZero() {
		return nil, domain.ErrNoActivePayee
	}
	return &domain.PayeeDestination{
		PayeeRef: payeeRef, DestinationID: d.DestinationID, Version: d.UpdatedAt.UTC().Format(time.RFC3339Nano),
		Fingerprint: d.Fingerprint, LegalEntityID: d.LegalEntityID, Currency: d.Currency,
		ChangedBy: []string{d.ProposedByPrincipalID, d.VerifiedByPrincipalID, d.ApprovedByPrincipalID},
	}, nil
}

// ── worker payee reference (WFP-01) ──────────────────────────────────────────

type WorkerPayee struct {
	baseURL string
	http    *http.Client
}

func NewWorkerPayee(baseURL string, c *http.Client) *WorkerPayee {
	return &WorkerPayee{baseURL: strings.TrimRight(baseURL, "/"), http: newHTTP(c)}
}

// PayeeRefFor returns the worker's payee reference from the ACTIVE financial profile(s) of this
// legal entity on the pay date. It must be VERIFIED and unambiguous (one distinct reference).
func (w *WorkerPayee) PayeeRefFor(ctx context.Context, tenantID, principalID, legalEntityID, workerID, asOf string) (string, error) {
	q := url.Values{"worker_id": {workerID}, "as_of": {asOf}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, w.baseURL+"/v1/wfp/worker-financial-profiles/as-of?"+q.Encode(), nil)
	if err != nil {
		return "", domain.ErrWorkerProfileUnavailable
	}
	req.Header.Set("X-Tenant-Id", tenantID)
	req.Header.Set("X-Principal-Id", principalID)
	req.Header.Set("X-Legal-Entity-Id", legalEntityID)
	resp, err := w.http.Do(req)
	if err != nil {
		return "", domain.ErrWorkerProfileUnavailable
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return "", domain.ErrPayeeMissing
	case resp.StatusCode != http.StatusOK:
		return "", domain.ErrWorkerProfileUnavailable
	}
	var body struct {
		Profiles []struct {
			LegalEntityID  string  `json:"legal_entity_id"`
			PayeeRef       *string `json:"payee_ref"`
			PayeeRefStatus string  `json:"payee_ref_status"`
		} `json:"profiles"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", domain.ErrWorkerProfileUnavailable
	}
	ref := ""
	for _, p := range body.Profiles {
		if p.LegalEntityID != legalEntityID {
			continue
		}
		if p.PayeeRef == nil || strings.TrimSpace(*p.PayeeRef) == "" || p.PayeeRefStatus != "VERIFIED" {
			return "", domain.ErrPayeeMissing
		}
		if ref != "" && ref != *p.PayeeRef {
			return "", domain.ErrPayeeMissing // two assignments pay to different payees: not guessed
		}
		ref = *p.PayeeRef
	}
	if ref == "" {
		return "", domain.ErrPayeeMissing
	}
	return ref, nil
}

// ── paying account (treasury-svc) ────────────────────────────────────────────

type PayerAccounts struct {
	baseURL string
	http    *http.Client
}

func NewPayerAccounts(baseURL string, c *http.Client) *PayerAccounts {
	return &PayerAccounts{baseURL: strings.TrimRight(baseURL, "/"), http: newHTTP(c)}
}

// Verify requires the account to belong to the legal entity, be ACTIVE, ownership-verified and in
// the payment currency. The caller's identity is forwarded so treasury authorizes the real caller.
func (a *PayerAccounts) Verify(ctx context.Context, tenantID, principalID, legalEntityID, accountID, currency string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.baseURL+"/v1/treasury/accounts/"+url.PathEscape(accountID), nil)
	if err != nil {
		return domain.ErrPayerUnavailable
	}
	req.Header.Set("X-Tenant-Id", tenantID)
	req.Header.Set("X-Principal-Id", principalID)
	req.Header.Set("X-Legal-Entity-Id", legalEntityID)
	req.Header.Set("X-Correlation-ID", uuid.NewString())
	resp, err := a.http.Do(req)
	if err != nil {
		return domain.ErrPayerUnavailable
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return domain.ErrPayerAccountInvalid
	case resp.StatusCode != http.StatusOK:
		return domain.ErrPayerUnavailable
	}
	var acct struct {
		BankAccountID       string `json:"bank_account_id"`
		TenantID            string `json:"tenant_id"`
		LegalEntityID       string `json:"legal_entity_id"`
		CurrencyCode        string `json:"currency_code"`
		AccountStatus       string `json:"account_status"`
		IsOwnershipVerified bool   `json:"is_ownership_verified"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&acct); err != nil {
		return domain.ErrPayerUnavailable
	}
	if acct.BankAccountID != accountID || acct.TenantID != tenantID || acct.LegalEntityID != legalEntityID ||
		acct.CurrencyCode != currency || acct.AccountStatus != "ACTIVE" || !acct.IsOwnershipVerified {
		return domain.ErrPayerAccountInvalid
	}
	return nil
}

// ── approval threshold (policy-svc) ──────────────────────────────────────────

type ApprovalPolicy struct {
	baseURL string
	http    *http.Client
}

func NewApprovalPolicy(baseURL string, c *http.Client) *ApprovalPolicy {
	return &ApprovalPolicy{baseURL: strings.TrimRight(baseURL, "/"), http: newHTTP(c)}
}

// Evaluate asks policy-svc for the APPROVAL_THRESHOLD of an amount, as payment-authorization-svc does
// for AP payments. "No applicable policy" (404) is WITHIN_THRESHOLD, policy-svc's own documented stance;
// an unreachable policy service is an error (validation is blocked), never a pass.
func (p *ApprovalPolicy) Evaluate(ctx context.Context, tenantID, principalID, legalEntityID string, amount float64) (result, versionID string, err error) {
	actionContext, _ := json.Marshal(map[string]float64{"amount": amount})
	payload, err := json.Marshal(map[string]any{
		"policy_type": "APPROVAL_THRESHOLD", "tenant_id": tenantID, "legal_entity_id": legalEntityID,
		"action_context": json.RawMessage(actionContext), "evaluated_by_principal_id": principalID, "decision_id": uuid.NewString(),
	})
	if err != nil {
		return "", "", domain.ErrPolicyUnavailable
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/v1/policies/evaluate", bytes.NewReader(payload))
	if err != nil {
		return "", "", domain.ErrPolicyUnavailable
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-Id", tenantID)
	resp, err := p.http.Do(req)
	if err != nil {
		return "", "", domain.ErrPolicyUnavailable
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return "WITHIN_THRESHOLD", "", nil
	}
	if resp.StatusCode != http.StatusOK {
		return "", "", domain.ErrPolicyUnavailable
	}
	var out struct {
		Result          string `json:"result"`
		PolicyVersionID string `json:"policy_version_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || out.Result == "" {
		return "", "", domain.ErrPolicyUnavailable
	}
	return out.Result, out.PolicyVersionID, nil
}

// ── Banking handoff (payment-initiation-adapter-svc, BNK-06) ─────────────────

type BankingHandoff struct {
	baseURL string
	http    *http.Client
}

func NewBankingHandoff(baseURL string, c *http.Client) *BankingHandoff {
	return &BankingHandoff{baseURL: strings.TrimRight(baseURL, "/"), http: newHTTP(c)}
}

// HandoffRequest is what an approved instruction hands to Banking. The adapter re-fetches the
// fingerprint from this service and refuses a mismatch; PayeeRef is the party reference (the adapter
// resolves the beneficiary), never bank details.
type HandoffRequest struct {
	TenantID        string
	PrincipalID     string
	CorrelationID   string
	InstructionID   string
	LegalEntityID   string
	Fingerprint     string
	PayerAccountRef string
	PayeeRef        string
	Amount          float64
	Currency        string
	ValueDate       time.Time
	Reference       string
}

type Attempt struct {
	AttemptID string
	Status    string
}

// ErrRefused is a definitive refusal by Banking before any attempt exists (4xx): nothing reached
// the provider and the instruction can be released back to APPROVED.
type ErrRefused struct{ Reason string }

func (e *ErrRefused) Error() string { return "Banking refused the instruction: " + e.Reason }

func (b *BankingHandoff) do(ctx context.Context, method, path string, r HandoffRequest, body any) (int, []byte, error) {
	var rd io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		rd = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, b.baseURL+path, rd)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-Id", r.TenantID)
	req.Header.Set("X-Principal-Id", r.PrincipalID)
	req.Header.Set("X-Legal-Entity-Id", r.LegalEntityID)
	req.Header.Set("X-Correlation-ID", r.CorrelationID)
	resp, err := b.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, out, nil
}

// Handoff prepares the attempt (idempotent on the instruction's deterministic key, so a retry
// returns the same attempt) and submits it if it is still PREPARED. Unreachable or a 5xx is
// domain.ErrHandoffUnavailable (the instruction stays pending); a 4xx before an attempt exists is *ErrRefused.
func (b *BankingHandoff) Handoff(ctx context.Context, r HandoffRequest) (*Attempt, error) {
	status, body, err := b.do(ctx, http.MethodPost, "/bnk06/attempts", r, map[string]any{
		"LegalEntityID": r.LegalEntityID, "SourceReference": r.InstructionID, "AuthorizationFingerprint": r.Fingerprint,
		"AuthorizationID": r.InstructionID, "AuthorizationSource": domain.AuthorizationSourcePayroll,
		"PayerAccountRef": r.PayerAccountRef, "PayeeRef": r.PayeeRef, "Amount": r.Amount, "Currency": r.Currency,
		"ExecutionDate": r.ValueDate.UTC().Format(time.RFC3339), "PaymentReference": r.Reference, "PayerAccountVerified": true,
		"IdempotencyKey": domain.BankingIdempotencyKey(r.InstructionID),
	})
	if err != nil || status == http.StatusTooManyRequests || status >= 500 {
		return nil, domain.ErrHandoffUnavailable
	}
	if status != http.StatusOK && status != http.StatusCreated {
		reason := strings.TrimSpace(string(body))
		if len(reason) > 300 {
			reason = reason[:300]
		}
		return nil, &ErrRefused{Reason: reason}
	}
	var a Attempt
	if err := json.Unmarshal(body, &a); err != nil || a.AttemptID == "" {
		return nil, domain.ErrHandoffUnavailable
	}
	if a.Status != "PREPARED" {
		return &a, nil // already submitted, or pending/unknown: Banking's state is reported as it is
	}
	status, body, err = b.do(ctx, http.MethodPost, "/bnk06/attempts/"+url.PathEscape(a.AttemptID)+"/submit", r, struct{}{})
	if err != nil || status == http.StatusTooManyRequests || status >= 500 {
		return &a, nil // the attempt exists; whether the submit landed is unknown: stays PREPARED/pending
	}
	var sub Attempt
	if status == http.StatusOK && json.Unmarshal(body, &sub) == nil && sub.Status != "" {
		a.Status = sub.Status
	}
	return &a, nil
}

// AttemptStatus reads the adapter's status of an attempt (BNK-06): PREPARED, SUBMITTED,
// PENDING_UNKNOWN, REJECTED_BEFORE_SUBMISSION, CANCELLED, QUARANTINED. Unreachable or any
// non-200 answer is domain.ErrHandoffUnavailable: an unknown is never reported as a status.
func (b *BankingHandoff) AttemptStatus(ctx context.Context, tenantID, principalID, legalEntityID, attemptID string) (string, error) {
	status, body, err := b.do(ctx, http.MethodGet, "/bnk06/attempts/"+url.PathEscape(attemptID), HandoffRequest{
		TenantID: tenantID, PrincipalID: principalID, LegalEntityID: legalEntityID, CorrelationID: uuid.NewString(),
	}, nil)
	if err != nil || status != http.StatusOK {
		return "", domain.ErrHandoffUnavailable
	}
	var a Attempt
	if err := json.Unmarshal(body, &a); err != nil || a.Status == "" {
		return "", domain.ErrHandoffUnavailable
	}
	return a.Status, nil
}
