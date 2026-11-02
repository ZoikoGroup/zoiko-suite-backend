package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap"

	"zoiko.io/tax-rules-svc/internal/domain"
)

// A blank or malformed effective_from used to reach the DATE column and come
// back as a 500; an unknown category was stored verbatim.
func TestCreateTaxRule_RejectsBadDateAndCategory(t *testing.T) {
	cases := map[string]domain.CreateTaxRuleRequest{
		"blank effective_from":     {JurisdictionID: "GB", RuleCode: "R1", Name: "n", Category: domain.CategoryVAT},
		"malformed effective_from": {JurisdictionID: "GB", RuleCode: "R1", Name: "n", Category: domain.CategoryVAT, EffectiveFrom: "01/01/2026"},
		"unknown category":         {JurisdictionID: "GB", RuleCode: "R1", Name: "n", Category: "VATT", EffectiveFrom: "2026-01-01"},
	}
	for name, body := range cases {
		w := httptest.NewRecorder()
		newTestHandler().CreateTaxRule(w, buildRequest(http.MethodPost, "/v1/tax-rules", body))
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: expected 400, got %d - %s", name, w.Code, w.Body.String())
		}
	}
}

type dupStore struct{ *stubStore }

func (dupStore) CreateTaxRule(_ context.Context, _ *domain.TaxRule) error {
	return domain.ErrDuplicateRuleCode
}

func TestCreateTaxRule_DuplicateIs409(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	h := New(dupStore{newStubStore()}, &stubPublisher{}, &stubAuthz{}, logger)
	w := httptest.NewRecorder()
	h.CreateTaxRule(w, buildRequest(http.MethodPost, "/v1/tax-rules", domain.CreateTaxRuleRequest{
		JurisdictionID: "GB", RuleCode: "R1", Name: "n", Category: domain.CategoryVAT, EffectiveFrom: "2026-01-01",
	}))
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d - %s", w.Code, w.Body.String())
	}
}
