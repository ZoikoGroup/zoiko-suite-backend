package decisionlog

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func record() Record {
	return Record{
		TenantID:          "11111111-1111-1111-1111-111111111111",
		LegalEntityID:     PlatformScope,
		PrincipalID:       "svc-policy",
		CorrelationID:     "corr-1",
		CausationID:       "cause-1",
		RequestID:         "policy-evidence-e1",
		IdempotencyKey:    "policy-svc:evaluation:e1",
		DecisionID:        "dec-1",
		ActionType:        "APPROVAL_THRESHOLD",
		Outcome:           "GRANTED",
		RuleBasis:         "AP:v1",
		PolicyVersionID:   "v1",
		EvaluationContext: json.RawMessage(`{"evidence_id":"e1"}`),
		DecidedAt:         time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC),
	}
}

// Decision-log register D-02 and audit row "Preserve the evaluation basis":
// every header governance-decision-log-svc's write-strict contract needs is
// sent, and the body names no tenant (the header's verified tenant files it).
func TestRecordDecision_SendsFullEnvelopeAndNoBodyTenant(t *testing.T) {
	var got *http.Request
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	if err := NewHTTPClient(srv.URL).RecordDecision(context.Background(), record()); err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	want := map[string]string{
		"X-Tenant-Id":       "11111111-1111-1111-1111-111111111111",
		"X-Principal-Id":    "svc-policy",
		"X-Legal-Entity-Id": PlatformScope,
		"X-Correlation-ID":  "corr-1",
		"X-Causation-Id":    "cause-1",
		"X-Request-Id":      "policy-evidence-e1",
		"X-Source-Channel":  "system",
		"Idempotency-Key":   "policy-svc:evaluation:e1",
		"X-Purpose-Context": PurposeContext,
	}
	for h, v := range want {
		if got.Header.Get(h) != v {
			t.Errorf("%s: expected %q, got %q", h, v, got.Header.Get(h))
		}
	}
	if _, ok := body["tenant_id"]; ok {
		t.Error("the body must not name a tenant: decision-log refuses one that differs from the header")
	}
	if body["legal_entity_id"] != PlatformScope || body["correlation_id"] != "corr-1" || body["decision_id"] != "dec-1" {
		t.Errorf("unexpected body: %v", body)
	}
}

func TestRecordDecision_ClassifiesRefusals(t *testing.T) {
	cases := map[int]bool{ // status → permanent
		http.StatusOK:                  false,
		http.StatusBadRequest:          false,
		http.StatusConflict:            true,
		http.StatusUnauthorized:        false,
		http.StatusForbidden:           false,
		http.StatusServiceUnavailable:  false,
		http.StatusInternalServerError: false,
	}
	for status, permanent := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(status) }))
		err := NewHTTPClient(srv.URL).RecordDecision(context.Background(), record())
		srv.Close()
		if status == http.StatusOK {
			if err != nil {
				t.Errorf("200 (replay) must be success, got %v", err)
			}
			continue
		}
		if err == nil {
			t.Errorf("%d: expected an error", status)
			continue
		}
		if errors.Is(err, ErrPermanent) != permanent {
			t.Errorf("%d: permanent=%v, got %v", status, permanent, err)
		}
	}
}
