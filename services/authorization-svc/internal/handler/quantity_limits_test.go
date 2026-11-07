package handler_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"zoiko.io/authorization-svc/internal/domain"
)

// Quantity (non-monetary) authority limits (000040, decision S8-D1): a
// QUANTITY limit bounds the request's `quantity` attribute, beside — not
// instead of — the monetary limit on its amount.

const (
	qtyTenant    = "11111111-1111-4111-8111-111111111111"
	qtyEntity    = "22222222-2222-4222-8222-222222222222"
	qtyPrincipal = "usr-approver-002"
)

func quantityLimit(upper string) domain.AuthorityLimit {
	return domain.AuthorityLimit{
		AuthorityLimitID: "lim-qty", TenantID: qtyTenant, PrincipalID: strptr(qtyPrincipal),
		AuthorityType: "payment_release", LegalEntityID: strptr(qtyEntity), LimitKind: domain.AuthorityLimitQuantity,
		LowerLimit: "0", UpperLimit: upper, EffectiveFrom: time.Now().Add(-time.Hour),
	}
}

func decideWithLimits(t *testing.T, limits []domain.AuthorityLimit, attrs map[string]string) domain.CanonicalDecisionResponse {
	t.Helper()
	store := &stubStore{rbacActions: []string{"payment.release"}, rbacBasis: "rbac:role=PAYMENT_RELEASER", authorityLimits: limits}
	r := newTestRouterFull(store, &stubPublisher{}, &stubValidator{})
	body, _ := json.Marshal(domain.CanonicalDecisionRequest{
		SubjectID: qtyPrincipal, PrincipalType: "HUMAN", TenantID: qtyTenant, LegalEntityID: qtyEntity,
		Action: "payment.release", ResourceAttributes: attrs, CorrelationID: "corr-qty",
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

func TestQuantityLimit_WithinIsPermitted(t *testing.T) {
	resp := decideWithLimits(t, []domain.AuthorityLimit{quantityLimit("10")}, map[string]string{"quantity": "10"})
	if resp.Decision != domain.CanonicalDecisionPermit {
		t.Fatalf("expected PERMIT at the limit, got %s %s", resp.Decision, resp.Basis)
	}
}

func TestQuantityLimit_AboveIsDenied(t *testing.T) {
	resp := decideWithLimits(t, []domain.AuthorityLimit{quantityLimit("10")}, map[string]string{"quantity": "11"})
	if resp.Decision != domain.CanonicalDecisionDeny || resp.Basis != "authority_limit:quantity_exceeded" {
		t.Fatalf("expected DENY authority_limit:quantity_exceeded, got %s %s", resp.Decision, resp.Basis)
	}
}

func TestQuantityLimit_MalformedQuantityIsDenied(t *testing.T) {
	resp := decideWithLimits(t, []domain.AuthorityLimit{quantityLimit("10")}, map[string]string{"quantity": "-1"})
	if resp.Decision != domain.CanonicalDecisionDeny || resp.Basis != "authority_limit:invalid_quantity" {
		t.Fatalf("expected DENY authority_limit:invalid_quantity, got %s %s", resp.Decision, resp.Basis)
	}
}

func TestQuantityLimit_MissingQuantityDeniedOnlyWhenRequired(t *testing.T) {
	limits := []domain.AuthorityLimit{quantityLimit("10")}
	if resp := decideWithLimits(t, limits, map[string]string{"authority_limit_required": "true"}); resp.Basis != "authority_limit:quantity_required" {
		t.Fatalf("required: expected authority_limit:quantity_required, got %s %s", resp.Decision, resp.Basis)
	}
	if resp := decideWithLimits(t, limits, nil); resp.Decision != domain.CanonicalDecisionPermit {
		t.Fatalf("not required: expected PERMIT, got %s %s", resp.Decision, resp.Basis)
	}
}

// The two kinds are judged separately: a quantity within its limit does not
// excuse an amount over the monetary one, and a quantity limit is not read as
// an amount in GBP.
func TestQuantityLimit_KindsAreIndependent(t *testing.T) {
	money := domain.AuthorityLimit{
		AuthorityLimitID: "lim-gbp", TenantID: qtyTenant, PrincipalID: strptr(qtyPrincipal), AuthorityType: "payment_release",
		LegalEntityID: strptr(qtyEntity), LimitKind: domain.AuthorityLimitMonetary, Currency: "GBP",
		LowerLimit: "0", UpperLimit: "1000.00", EffectiveFrom: time.Now().Add(-time.Hour),
	}
	limits := []domain.AuthorityLimit{quantityLimit("10"), money}
	if resp := decideWithLimits(t, limits, map[string]string{"quantity": "5", "amount": "5000", "currency": "GBP"}); resp.Basis != "authority_limit:exceeded:upper_limit" {
		t.Fatalf("amount over its limit: expected authority_limit:exceeded:upper_limit, got %s %s", resp.Decision, resp.Basis)
	}
	if resp := decideWithLimits(t, limits, map[string]string{"quantity": "5", "amount": "500", "currency": "GBP"}); resp.Decision != domain.CanonicalDecisionPermit {
		t.Fatalf("both within: expected PERMIT, got %s %s", resp.Decision, resp.Basis)
	}
	if resp := decideWithLimits(t, []domain.AuthorityLimit{quantityLimit("10")}, map[string]string{"amount": "50000", "currency": "GBP"}); resp.Decision != domain.CanonicalDecisionPermit {
		t.Fatalf("a quantity limit must not cap an amount: got %s %s", resp.Decision, resp.Basis)
	}
}

func TestCreateAuthorityLimit_Quantity(t *testing.T) {
	post := func(body string) (*stubStore, *httptest.ResponseRecorder) {
		s := &stubStore{}
		r := newTestRouterFull(s, &stubPublisher{}, &stubValidator{})
		req := httptest.NewRequest(http.MethodPost, "/v1/admin/authority-limits", bytes.NewBufferString(body))
		req.Header.Set("X-Principal-Id", "admin-1")
		req.Header.Set("X-Tenant-Id", qtyTenant)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return s, w
	}
	const base = `"principal_id":"p-1","authority_type":"po_line_approval","reason_code":"POLICY"`

	s, w := post(`{` + base + `,"limit_kind":"quantity","upper_limit":"50"}`)
	if w.Code != http.StatusAccepted || s.createdLimit == nil || s.createdLimit.LimitKind != domain.AuthorityLimitQuantity || s.createdLimit.Currency != "" {
		t.Fatalf("expected 202 with a QUANTITY limit and no currency, got %d %+v: %s", w.Code, s.createdLimit, w.Body.String())
	}
	for name, body := range map[string]string{
		"currency on a count": `{` + base + `,"limit_kind":"QUANTITY","currency":"GBP","upper_limit":"50"}`,
		"fractional bound":    `{` + base + `,"limit_kind":"QUANTITY","upper_limit":"2.5"}`,
		"unknown kind":        `{` + base + `,"limit_kind":"WEIGHT","upper_limit":"50"}`,
	} {
		s, w := post(body)
		if w.Code != http.StatusBadRequest || s.createdLimit != nil || !strings.Contains(w.Body.String(), "invalid_field") {
			t.Errorf("%s: expected 400 invalid_field and no store call, got %d: %s", name, w.Code, w.Body.String())
		}
	}
	s, w = post(`{` + base + `,"upper_limit":"25000.00"}`)
	if w.Code != http.StatusAccepted || s.createdLimit.LimitKind != domain.AuthorityLimitMonetary || s.createdLimit.Currency != "GBP" {
		t.Fatalf("default: expected a MONETARY GBP limit, got %d %+v", w.Code, s.createdLimit)
	}
}
