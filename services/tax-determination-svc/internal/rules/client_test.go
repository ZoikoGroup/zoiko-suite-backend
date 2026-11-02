package rules

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// fakeTaxRules behaves like tax-rules-svc's GET /v1/tax-rules: it refuses a
// request without X-Tenant-Id or X-Principal-Id with 401, exactly as the real
// tenant middleware and requirePrincipal do, and otherwise returns one rule.
// It records the headers it saw so the test can assert on the envelope.
func fakeTaxRules(t *testing.T, seen *http.Header) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*seen = r.Header.Clone()
		if r.Header.Get("X-Tenant-Id") == "" || r.Header.Get("X-Principal-Id") == "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"rules": []TaxRuleDTO{{RuleID: "trule-1", Category: "VAT", TaxRatePercentage: 20, Status: "ACTIVE"}},
		})
	}))
}

// Regression: the client used to send only X-Tenant-Id, so tax-rules-svc
// answered 401 and every determination failed closed as "tax-rules-svc could
// not be reached".
func TestFetchActiveRule_ForwardsCallerIdentity(t *testing.T) {
	var seen http.Header
	srv := fakeTaxRules(t, &seen)
	defer srv.Close()

	caller := Caller{TenantID: "tenant-1", PrincipalID: "principal-1", LegalEntityID: "le-1", CorrelationID: "corr-1"}
	rule, err := NewClient(srv.URL).FetchActiveRule(context.Background(), caller, "GB", "VAT")
	if err != nil {
		t.Fatalf("expected the rule, got error: %v", err)
	}
	if rule.RuleID != "trule-1" {
		t.Fatalf("unexpected rule %+v", rule)
	}

	expect := map[string]string{
		"X-Tenant-Id":       "tenant-1",
		"X-Principal-Id":    "principal-1",
		"X-Legal-Entity-Id": "le-1",
		"X-Correlation-ID":  "corr-1",
		"X-Source-Channel":  "system",
	}
	for header, want := range expect {
		if got := seen.Get(header); got != want {
			t.Errorf("header %s = %q, want %q", header, got, want)
		}
	}
	if seen.Get("X-Request-Id") == "" {
		t.Error("X-Request-Id must be set for this hop")
	}
}

// A refusal from tax-rules-svc must still fail closed (unavailable), never be
// mistaken for "no rule found" (which would zero-rate the transaction).
func TestFetchActiveRule_RefusalIsUnavailableNotNotFound(t *testing.T) {
	var seen http.Header
	srv := fakeTaxRules(t, &seen)
	defer srv.Close()

	_, err := NewClient(srv.URL).FetchActiveRule(context.Background(), Caller{TenantID: "tenant-1"}, "GB", "VAT")
	if !errors.Is(err, ErrTaxRulesUnavailable) {
		t.Fatalf("expected ErrTaxRulesUnavailable for a 401, got %v", err)
	}
	if errors.Is(err, ErrTaxRuleNotFound) {
		t.Fatal("a 401 must not be reported as not-found")
	}
}
