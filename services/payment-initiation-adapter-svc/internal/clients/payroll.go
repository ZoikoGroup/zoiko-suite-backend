package clients

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"zoiko.io/payment-initiation-adapter-svc/internal/domain"
)

// ErrPayrollFingerprintUnavailable is VerifyInstructionFingerprint's analogue of
// ErrTransferFingerprintUnavailable: any network error, non-200 response or decode failure.
var ErrPayrollFingerprintUnavailable = errors.New("payroll-run-svc instruction fingerprint lookup unavailable")

// PayrollClient verifies a WFP-07 payroll payment instruction against payroll-run-svc's live
// record rather than trusting the caller-supplied fingerprint.
type PayrollClient interface {
	VerifyInstructionFingerprint(ctx context.Context, tenantID, instructionID, fingerprint string) error
}

type PayrollHTTPClient struct {
	baseURL string
	http    *http.Client
}

func NewPayrollHTTPClient(baseURL string) *PayrollHTTPClient {
	return &PayrollHTTPClient{baseURL: strings.TrimRight(baseURL, "/"), http: &http.Client{Timeout: 3 * time.Second}}
}

// VerifyInstructionFingerprint requires the instruction to be APPROVED (payroll-run-svc moves it to
// SUBMITTED_TO_BANKING before it calls the adapter, and a retry of the same attempt arrives in that
// state too) and its approved fingerprint to equal the supplied one. Anything else is a mismatch
// regardless of whether the strings happen to match: the instruction moved on, or was never approved.
func (c *PayrollHTTPClient) VerifyInstructionFingerprint(ctx context.Context, tenantID, instructionID, fingerprint string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.baseURL+"/v1/payroll/wfp/payment-instructions/"+url.PathEscape(instructionID)+"/fingerprint", nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-Tenant-Id", tenantID)
	resp, err := c.http.Do(req)
	if err != nil {
		return ErrPayrollFingerprintUnavailable
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return domain.ErrPayrollFingerprintMismatch
	}
	if resp.StatusCode != http.StatusOK {
		return ErrPayrollFingerprintUnavailable
	}
	var live struct {
		State       string `json:"state"`
		Fingerprint string `json:"fingerprint"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&live); err != nil {
		return ErrPayrollFingerprintUnavailable
	}
	if live.State != "SUBMITTED_TO_BANKING" && live.State != "APPROVED" {
		return domain.ErrPayrollFingerprintMismatch
	}
	if live.Fingerprint == "" || live.Fingerprint != fingerprint {
		return domain.ErrPayrollFingerprintMismatch
	}
	return nil
}
