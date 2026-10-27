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
				"GOVERNANCE", "BOARD_RESOLUTION_ADOPT", "corr-1", "principal-1",
				[]Artifact{{EvidenceType: "SUPPORTING_DOCUMENT", ReferenceID: "doc-1"}})

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
			if got.LegalEntityID != "entity-1" || got.DomainCode != "GOVERNANCE" ||
				got.ActionType != "BOARD_RESOLUTION_ADOPT" || got.CorrelationID != "corr-1" ||
				len(got.PresentArtifacts) != 1 || got.PresentArtifacts[0].ReferenceID != "doc-1" {
				t.Errorf("unexpected body: %+v", got)
			}
		})
	}
}
