package evidencereq

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func TestEvaluateSufficient_SendsCanonicalEnvelopeAndIsAllowListed(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		outcome string
		wantErr error
		wantOK  bool
	}{
		{name: "satisfied", status: http.StatusOK, outcome: "SATISFIED", wantOK: true},
		{name: "no requirements", status: http.StatusOK, outcome: "NO_REQUIREMENTS_DEFINED", wantOK: true},
		{name: "missing", status: http.StatusOK, outcome: "MISSING", wantErr: ErrEvidenceMissing},
		{name: "unknown outcome", status: http.StatusOK, outcome: "SOMETHING_NEW", wantErr: ErrServiceUnavailable},
		{name: "non-200", status: http.StatusInternalServerError, wantErr: ErrServiceUnavailable},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var got evaluateRequest
			var headers http.Header
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				headers = r.Header.Clone()
				if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
					t.Errorf("decode request: %v", err)
				}
				w.WriteHeader(tc.status)
				_ = json.NewEncoder(w).Encode(map[string]any{"outcome": tc.outcome})
			}))
			defer srv.Close()

			err := NewClient(srv.URL).EvaluateSufficient(context.Background(), "tenant-1", "entity-1",
				"TAX", "TAX_RETURN_SUBMIT", "corr-1", "principal-1", []string{"doc-1", " ", "doc-2"})

			if tc.wantOK && err != nil {
				t.Fatalf("err = %v, want nil", err)
			}
			if !tc.wantOK && !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}

			for k, want := range map[string]string{
				"X-Tenant-Id":       "tenant-1",
				"X-Principal-Id":    "principal-1",
				"X-Legal-Entity-Id": "entity-1",
				"X-Correlation-ID":  "corr-1",
				"X-Request-Id":      "corr-1",
				"X-Source-Channel":  "system",
				"Idempotency-Key":   "corr-1",
			} {
				if got := headers.Get(k); got != want {
					t.Errorf("%s = %q, want %q", k, got, want)
				}
			}
			if got.LegalEntityID != "entity-1" || got.DomainCode != "TAX" ||
				got.ActionType != "TAX_RETURN_SUBMIT" || got.CorrelationID != "corr-1" ||
				len(got.PresentArtifacts) != 2 {
				t.Errorf("unexpected body: %+v", got)
			}
		})
	}
}

// D-ER-5: NO_REQUIREMENTS_DEFINED still lets the action proceed — the caller
// decides — but it is logged as a warning naming the action, so an
// unconfigured gate is visible rather than indistinguishable from a satisfied
// one. SATISFIED logs nothing.
func TestEvaluate_NoRequirementsDefinedPassesWithAWarning(t *testing.T) {
	for _, tc := range []struct {
		outcome  string
		wantWarn int
	}{{"NO_REQUIREMENTS_DEFINED", 1}, {"SATISFIED", 0}} {
		t.Run(tc.outcome, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{"outcome": tc.outcome, "evaluation_id": "eval-1"})
			}))
			defer srv.Close()
			core, logs := observer.New(zap.WarnLevel)
			err := NewClient(srv.URL).WithLogger(zap.New(core)).EvaluateSufficient(context.Background(),
				"tenant-1", "entity-1", "FINANCE", "JOURNAL_POST", "corr-1", "principal-1", nil)
			if err != nil {
				t.Fatalf("err = %v, want nil", err)
			}
			if got := logs.Len(); got != tc.wantWarn {
				t.Fatalf("warnings = %d, want %d", got, tc.wantWarn)
			}
			if tc.wantWarn == 0 {
				return
			}
			fields := logs.All()[0].ContextMap()
			for k, want := range map[string]string{"tenant_id": "tenant-1", "legal_entity_id": "entity-1",
				"domain_code": "FINANCE", "action_type": "JOURNAL_POST", "evaluation_id": "eval-1"} {
				if fields[k] != want {
					t.Errorf("%s = %v, want %q", k, fields[k], want)
				}
			}
		})
	}
}
