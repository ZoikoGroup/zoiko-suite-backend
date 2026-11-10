package handler_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/authorization-svc/internal/domain"
	"zoiko.io/authorization-svc/internal/handler"
	"zoiko.io/authorization-svc/internal/siem"
)

// Pipeline hardening (10 Nov 2026): two controls that answered PERMIT on a
// request that did not carry the facts they need.

func decideRevalidation(t *testing.T, attrs map[string]string) domain.CanonicalDecisionResponse {
	t.Helper()
	store := &stubStore{rbacActions: []string{"payment.execute"}, rbacBasis: "rbac:role=PAYMENT_EXECUTOR"}
	r := newTestRouterFull(store, &stubPublisher{}, &stubValidator{})
	body, _ := json.Marshal(domain.CanonicalDecisionRequest{
		SubjectID: "usr-executor", PrincipalType: "HUMAN", TenantID: qtyTenant, LegalEntityID: qtyEntity,
		Action: "payment.execute", ResourceAttributes: attrs, CorrelationID: "corr-reval",
	})
	req := httptest.NewRequest(http.MethodPost, "/internal/authorization/decisions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-Id", qtyTenant)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp domain.CanonicalDecisionResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	return resp
}

func hasCode(codes []string, want string) bool {
	for _, c := range codes {
		if c == want {
			return true
		}
	}
	return false
}

// Scenario A10, fail-closed: an approval fingerprint with nothing to compare
// it against is not a revalidation. Before, omitting the current hash
// skipped the check and the approved action executed on unknown facts.
func TestRevalidation_ApprovedHashWithoutCurrentIsDenied(t *testing.T) {
	resp := decideRevalidation(t, map[string]string{"approved_fact_hash": "hash-v1"})
	if resp.Decision != domain.CanonicalDecisionDeny || resp.Basis != "revalidation:current_fact_hash_missing" {
		t.Fatalf("expected DENY revalidation:current_fact_hash_missing, got %s %s", resp.Decision, resp.Basis)
	}
	if !hasCode(resp.ReasonCodes, "CURRENT_FACT_HASH_REQUIRED") || !hasCode(resp.ReasonCodes, "REAPPROVAL_REQUIRED") {
		t.Fatalf("reason codes %v lack CURRENT_FACT_HASH_REQUIRED / REAPPROVAL_REQUIRED", resp.ReasonCodes)
	}
}

func TestRevalidation_RequestedWithoutApprovedHashIsDenied(t *testing.T) {
	for name, attrs := range map[string]map[string]string{
		"neither hash":       {"execution_revalidation": "true"},
		"current hash alone": {"execution_revalidation": "true", "current_fact_hash": "hash-v2"},
	} {
		resp := decideRevalidation(t, attrs)
		if resp.Decision != domain.CanonicalDecisionDeny || resp.Basis != "revalidation:approved_fact_hash_missing" {
			t.Errorf("%s: expected DENY revalidation:approved_fact_hash_missing, got %s %s", name, resp.Decision, resp.Basis)
		}
	}
}

func TestRevalidation_NotRequestedChangesNothing(t *testing.T) {
	if resp := decideRevalidation(t, map[string]string{"current_fact_hash": "hash-v2"}); resp.Decision != domain.CanonicalDecisionPermit {
		t.Fatalf("a current hash alone, revalidation not requested: expected PERMIT, got %s %s", resp.Decision, resp.Basis)
	}
}

func gbpLimit(upper string) domain.AuthorityLimit {
	return domain.AuthorityLimit{
		AuthorityLimitID: "lim-gbp", TenantID: qtyTenant, PrincipalID: strptr(qtyPrincipal), AuthorityType: "payment_release",
		LegalEntityID: strptr(qtyEntity), LimitKind: domain.AuthorityLimitMonetary, Currency: "GBP",
		LowerLimit: "0", UpperLimit: upper, EffectiveFrom: time.Now().Add(-time.Hour),
	}
}

// A9 basis: an amount with no currency against a currency-bearing limit is
// not compared. It used to be read as GBP, so 9,000 JPY-denominated units
// sent without a currency were judged as 9,000 GBP and vice versa.
func TestAuthorityLimit_AmountWithoutCurrencyIsDenied(t *testing.T) {
	resp := decideWithLimits(t, []domain.AuthorityLimit{gbpLimit("10000.00")}, map[string]string{"amount": "500"})
	if resp.Decision != domain.CanonicalDecisionDeny || resp.Basis != "authority_limit:currency_required" {
		t.Fatalf("expected DENY authority_limit:currency_required, got %s %s", resp.Decision, resp.Basis)
	}
	if !hasCode(resp.ReasonCodes, "CURRENCY_REQUIRED") {
		t.Fatalf("reason codes %v lack CURRENCY_REQUIRED", resp.ReasonCodes)
	}
}

func TestAuthorityLimit_FXConversionNamesItsBasis(t *testing.T) {
	// 1,000 USD at the default reference rate is 780 GBP: within 1,000 GBP.
	resp := decideWithLimits(t, []domain.AuthorityLimit{gbpLimit("1000.00")}, map[string]string{"amount": "1000", "currency": "USD"})
	if resp.Decision != domain.CanonicalDecisionPermit {
		t.Fatalf("expected PERMIT, got %s %s", resp.Decision, resp.Basis)
	}
	if !hasCode(resp.ReasonCodes, "FX_BASIS:REFERENCE_RATES:USD/GBP") {
		t.Fatalf("reason codes %v do not name the FX basis", resp.ReasonCodes)
	}
}

// The reference rates are configuration (AUTHZ_FX_REFERENCE_RATES), not code.
func TestAuthorityLimit_ConfiguredReferenceRatesAreUsed(t *testing.T) {
	store := &stubStore{rbacActions: []string{"payment.release"}, rbacBasis: "rbac:role=PAYMENT_RELEASER",
		authorityLimits: []domain.AuthorityLimit{gbpLimit("1000.00")}}
	h := handler.New(store, &stubPublisher{}, &stubValidator{}, siem.New("", "authorization-svc", zap.NewNop()), "platform-scope-entity", false, zap.NewNop())
	h.SetFXReferenceRates(map[string]float64{"GBP": 1.0, "USD": 2.0}) // 1 USD = 2 GBP
	r := chi.NewRouter()
	handler.RegisterRoutes(r, h)

	body, _ := json.Marshal(domain.CanonicalDecisionRequest{
		SubjectID: qtyPrincipal, PrincipalType: "HUMAN", TenantID: qtyTenant, LegalEntityID: qtyEntity,
		Action: "payment.release", ResourceAttributes: map[string]string{"amount": "600", "currency": "USD"}, CorrelationID: "corr-fx",
	})
	req := httptest.NewRequest(http.MethodPost, "/internal/authorization/decisions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-Id", qtyTenant)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	var resp domain.CanonicalDecisionResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Decision != domain.CanonicalDecisionDeny || resp.Basis != "authority_limit:exceeded:after_fx_conversion" {
		t.Fatalf("600 USD at 2.0 = 1,200 GBP over 1,000: expected DENY after_fx_conversion, got %s %s", resp.Decision, resp.Basis)
	}
}

func TestParseFXReferenceRates(t *testing.T) {
	rates, err := handler.ParseFXReferenceRates("GBP=1, usd=0.79 ,EUR=0.86")
	if err != nil || rates["USD"] != 0.79 || rates["EUR"] != 0.86 || rates["GBP"] != 1 {
		t.Fatalf("got %v %v", rates, err)
	}
	for _, bad := range []string{"USD", "USD=abc", "USD=-1", "USD=0", "=1"} {
		if _, err := handler.ParseFXReferenceRates(bad); err == nil {
			t.Errorf("%q: expected an error", bad)
		}
	}
	if rates, err := handler.ParseFXReferenceRates(""); err != nil || rates != nil {
		t.Fatalf("empty: expected nil, nil; got %v %v", rates, err)
	}
}
