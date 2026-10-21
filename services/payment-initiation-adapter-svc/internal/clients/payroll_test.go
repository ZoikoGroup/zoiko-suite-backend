package clients

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"zoiko.io/payment-initiation-adapter-svc/internal/domain"
)

func payrollServer(t *testing.T, status int, body string) *PayrollHTTPClient {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/payroll/wfp/payment-instructions/i-1/fingerprint" || r.Header.Get("X-Tenant-Id") != "t-1" {
			t.Errorf("unexpected request %s tenant %q", r.URL.Path, r.Header.Get("X-Tenant-Id"))
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return NewPayrollHTTPClient(srv.URL)
}

func TestVerifyInstructionFingerprint(t *testing.T) {
	ctx := context.Background()
	if err := payrollServer(t, 200, `{"state":"SUBMITTED_TO_BANKING","fingerprint":"sha256:x"}`).VerifyInstructionFingerprint(ctx, "t-1", "i-1", "sha256:x"); err != nil {
		t.Fatalf("a matching approved fingerprint must verify: %v", err)
	}
	if err := payrollServer(t, 200, `{"state":"APPROVED","fingerprint":"sha256:x"}`).VerifyInstructionFingerprint(ctx, "t-1", "i-1", "sha256:x"); err != nil {
		t.Fatalf("APPROVED must verify: %v", err)
	}
	for name, c := range map[string]struct {
		status int
		body   string
		want   error
	}{
		"different fingerprint": {200, `{"state":"APPROVED","fingerprint":"sha256:y"}`, domain.ErrPayrollFingerprintMismatch},
		"not approved":          {200, `{"state":"VALIDATED","fingerprint":""}`, domain.ErrPayrollFingerprintMismatch},
		"empty fingerprint":     {200, `{"state":"APPROVED","fingerprint":""}`, domain.ErrPayrollFingerprintMismatch},
		"superseded":            {200, `{"state":"SUPERSEDED","fingerprint":"sha256:x"}`, domain.ErrPayrollFingerprintMismatch},
		"unknown instruction":   {404, ``, domain.ErrPayrollFingerprintMismatch},
		"server error":          {503, ``, ErrPayrollFingerprintUnavailable},
		"garbage":               {200, `nope`, ErrPayrollFingerprintUnavailable},
	} {
		err := payrollServer(t, c.status, c.body).VerifyInstructionFingerprint(ctx, "t-1", "i-1", "sha256:x")
		if !errors.Is(err, c.want) {
			t.Errorf("%s: want %v, got %v", name, c.want, err)
		}
	}
}
