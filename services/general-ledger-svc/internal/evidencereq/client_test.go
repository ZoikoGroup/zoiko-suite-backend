package evidencereq

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestEvaluateSufficient_SendsCanonicalEnvelopeAndIsAllowListed(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		outcome    string
		wantErr    error
		wantOK     bool
		wantRefs   int
		wantDomain string
	}{
		{name: "satisfied", status: http.StatusOK, outcome: "SATISFIED", wantOK: true, wantRefs: 2, wantDomain: "FINANCE"},
		{name: "no requirements", status: http.StatusOK, outcome: "NO_REQUIREMENTS_DEFINED", wantOK: true, wantRefs: 2, wantDomain: "FINANCE"},
		{name: "missing", status: http.StatusOK, outcome: "MISSING", wantErr: ErrEvidenceMissing, wantRefs: 2, wantDomain: "FINANCE"},
		{name: "unknown outcome", status: http.StatusOK, outcome: "SOMETHING_NEW", wantErr: ErrServiceUnavailable, wantRefs: 2, wantDomain: "FINANCE"},
		{name: "non-200", status: http.StatusInternalServerError, wantErr: ErrServiceUnavailable, wantRefs: 2, wantDomain: "FINANCE"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var got evaluateRequest
			var headers http.Header
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/evidence/evaluate" {
					t.Errorf("path = %q, want /v1/evidence/evaluate", r.URL.Path)
				}
				headers = r.Header.Clone()
				if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
					t.Errorf("decode request: %v", err)
				}
				w.WriteHeader(tc.status)
				_ = json.NewEncoder(w).Encode(map[string]any{"outcome": tc.outcome})
			}))
			defer srv.Close()

			c := NewClient(srv.URL)
			err := c.EvaluateSufficient(context.Background(), "tenant-1", "entity-1", tc.wantDomain,
				"JOURNAL_POST", "corr-1", "principal-1", []string{"doc-1", " ", "doc-2"})

			if tc.wantOK {
				if err != nil {
					t.Fatalf("err = %v, want nil", err)
				}
			} else if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}

			wantHeaders := map[string]string{
				"X-Tenant-Id":       "tenant-1",
				"X-Principal-Id":    "principal-1",
				"X-Legal-Entity-Id": "entity-1",
				"X-Correlation-ID":  "corr-1",
				"X-Request-Id":      "corr-1",
				"X-Source-Channel":  "system",
				"Idempotency-Key":   "corr-1",
			}
			for k, want := range wantHeaders {
				if got := headers.Get(k); got != want {
					t.Errorf("%s = %q, want %q", k, got, want)
				}
			}

			if got.LegalEntityID != "entity-1" || got.DomainCode != tc.wantDomain ||
				got.ActionType != "JOURNAL_POST" || got.CorrelationID != "corr-1" {
				t.Errorf("unexpected body: %+v", got)
			}
			if len(got.PresentArtifacts) != tc.wantRefs {
				t.Fatalf("artifacts = %d, want %d", len(got.PresentArtifacts), tc.wantRefs)
			}
			if got.PresentArtifacts[0].EvidenceType != "SUPPORTING_DOCUMENT" ||
				got.PresentArtifacts[0].ReferenceID != "doc-1" ||
				got.PresentArtifacts[1].ReferenceID != "doc-2" {
				t.Errorf("unexpected artifacts: %+v", got.PresentArtifacts)
			}
		})
	}
}
