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
	// limits are authority limits by principal ("" = tenant-wide).
	limits map[string][]domain.AuthorityLimit
}

func (c ceilingStore) FindDelegationCeilings(_ context.Context, _, _, _, _ string) ([]domain.DelegationCeiling, error) {
	return c.ceilings, nil
}

func (c ceilingStore) ListAuthorityLimits(_ context.Context, _ string, principalID, _, _ string) ([]domain.AuthorityLimit, error) {
	return c.limits[principalID], nil
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

// ORG-06: "Delegation can narrow or transmit authority but cannot manufacture
// authority", and negative case 11, "delegator grants higher limit than own
// authority → reject". A delegation without a ceiling of its own used to leave
// the amount unconstrained, so a delegator capped at 50,000 GBP could hand a
// delegate unlimited authority by simply not stating a ceiling: the delegate's
// own limits (usually none) were all the decision consulted.
func TestDelegationCeiling_InheritsTheDelegatorsLimit(t *testing.T) {
	gbp50k := []domain.AuthorityLimit{{AuthorityLimitID: "l1", TenantID: "t1", PrincipalID: str("cfo"),
		AuthorityType: "*", Currency: "GBP", UpperLimit: "50000"}}
	decide := func(ceilings []domain.DelegationCeiling, attrs map[string]string) (bool, string) {
		t.Helper()
		h := &Handler{store: ceilingStore{ceilings: ceilings, limits: map[string][]domain.AuthorityLimit{"cfo": gbp50k}}}
		denied, basis, _, err := h.evaluateDelegationCeiling(context.Background(),
			evalContext{PrincipalID: "assistant", TenantID: "t1", ActionType: "PAYMENT_APPROVE", Attributes: attrs}, "le-1")
		if err != nil {
			t.Fatal(err)
		}
		return denied, basis
	}
	uncapped := []domain.DelegationCeiling{{SourceDelegationID: "d1", DelegatorPrincipalID: "cfo"}}
	if denied, basis := decide(uncapped, map[string]string{"amount": "1000000", "currency": "GBP"}); !denied {
		t.Fatalf("an uncapped delegation must not confer more than the delegator's 50,000 GBP; allowed (basis %q)", basis)
	}
	if denied, basis := decide(uncapped, map[string]string{"amount": "49999", "currency": "GBP"}); denied {
		t.Fatalf("within the delegator's limit must be allowed, got %q", basis)
	}
	// A delegation's own ceiling above the delegator's limit is still bounded
	// by the delegator.
	over := []domain.DelegationCeiling{{SourceDelegationID: "d2", DelegatorPrincipalID: "cfo",
		LimitMinor: i64(10_000_000_00), LimitCurrency: str("GBP")}}
	if denied, _ := decide(over, map[string]string{"amount": "60000", "currency": "GBP"}); !denied {
		t.Fatal("a ceiling above the delegator's own limit must not lift the delegator's limit")
	}
	// Another conferring delegation from an uncapped delegator still admits.
	both := append(uncapped, domain.DelegationCeiling{SourceDelegationID: "d3", DelegatorPrincipalID: "ceo"})
	if denied, basis := decide(both, map[string]string{"amount": "1000000", "currency": "GBP"}); denied {
		t.Fatalf("a delegation from a principal without a limit admits the amount, got %q", basis)
	}
}
