package handler

import (
	"context"
	"testing"

	"zoiko.io/authorization-svc/internal/domain"
)

// ceilingStore answers only the ceiling lookup; every other store method is
// the embedded nil interface and would panic if the layer reached for it.
type ceilingStore struct {
	AuthorizationStore
	ceilings []domain.DelegationCeiling
}

func (c ceilingStore) FindDelegationCeilings(_ context.Context, _, _, _, _ string) ([]domain.DelegationCeiling, error) {
	return c.ceilings, nil
}

func i64(v int64) *int64   { return &v }
func str(v string) *string { return &v }

func ceilingDecision(t *testing.T, ceilings []domain.DelegationCeiling, attrs map[string]string) (bool, string) {
	t.Helper()
	h := &Handler{store: ceilingStore{ceilings: ceilings}}
	denied, basis, _, err := h.evaluateDelegationCeiling(context.Background(),
		evalContext{PrincipalID: "assistant", TenantID: "t1", ActionType: "PAYMENT_APPROVE", Attributes: attrs}, "le-1")
	if err != nil {
		t.Fatal(err)
	}
	return denied, basis
}

func TestDelegationCeiling(t *testing.T) {
	usd500 := []domain.DelegationCeiling{{SourceDelegationID: "d1", LimitMinor: i64(50000), LimitCurrency: str("USD")}}
	cases := []struct {
		name      string
		ceilings  []domain.DelegationCeiling
		attrs     map[string]string
		wantDeny  bool
		wantBasis string
	}{
		{"within the cap", usd500, map[string]string{"amount": "499.99", "currency": "USD"}, false, ""},
		{"exactly the cap", usd500, map[string]string{"amount": "500.00", "currency": "USD"}, false, ""},
		{"over the cap", usd500, map[string]string{"amount": "500.01", "currency": "USD"}, true, "delegation_limit:exceeded"},
		{"capped but no amount", usd500, nil, true, "delegation_limit:amount_required"},
		{"currency defaults to the cap's", usd500, map[string]string{"amount": "400"}, false, ""},
		// 400 GBP ≈ 512.82 USD at the reference rates: over a 500 USD cap.
		{"converted at reference rates", usd500, map[string]string{"amount": "400", "currency": "GBP"}, true, "delegation_limit:exceeded"},
		// A caller-supplied rate must not choose the ceiling.
		{"request fx_rate is ignored", usd500, map[string]string{"amount": "400", "currency": "GBP", "fx_rate": "0.0001"}, true, "delegation_limit:exceeded"},
		{"unknown currency cannot be compared", usd500, map[string]string{"amount": "1", "currency": "XYZ"}, true, "delegation_limit:exceeded"},
		{"uncapped delegation among the matches", append(usd500, domain.DelegationCeiling{SourceDelegationID: "d2"}),
			map[string]string{"amount": "9999999", "currency": "USD"}, false, ""},
		{"no delegation matches", nil, map[string]string{"amount": "9999999"}, false, ""},
		{"quantity cap", []domain.DelegationCeiling{{LimitQuantity: i64(3)}}, map[string]string{"quantity": "4"}, true, "delegation_limit:quantity_exceeded"},
		{"quantity within", []domain.DelegationCeiling{{LimitQuantity: i64(3)}}, map[string]string{"quantity": "3"}, false, ""},
		{"JPY has no minor units", []domain.DelegationCeiling{{LimitMinor: i64(1000), LimitCurrency: str("JPY")}},
			map[string]string{"amount": "1000", "currency": "JPY"}, false, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			denied, basis := ceilingDecision(t, c.ceilings, c.attrs)
			if denied != c.wantDeny || basis != c.wantBasis {
				t.Errorf("denied=%v basis=%q, want %v %q", denied, basis, c.wantDeny, c.wantBasis)
			}
		})
	}
}
