package handler_test

// SPEC_DEVIATIONS A1/B10: GetReciprocalBalances — per-direction and net
// totals by currency, split by match_status, authorized on both entities.

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"

	"zoiko.io/intercompany-accounting-svc/internal/domain"
)

// ReciprocalBalances is the stub's version of the store's grouped query: it
// selects the pair's period rows and sums each
// (direction, currency, match_status) group with domain.SumAmounts, exactly
// as the real store does at the SQL level, so the handler tests exercise
// exact arithmetic end to end.
func (s *stubStoreReal) ReciprocalBalances(_ context.Context, entityA, entityB, period string) ([]domain.ReciprocalBalanceGroup, error) {
	grouped := map[string]*domain.ReciprocalBalanceGroup{}
	for _, e := range s.entries {
		if e.FiscalPeriod == nil || *e.FiscalPeriod != period {
			continue
		}
		if !((e.SourceLegalEntityID == entityA && e.TargetLegalEntityID == entityB) ||
			(e.SourceLegalEntityID == entityB && e.TargetLegalEntityID == entityA)) {
			continue
		}

		key := strings.Join([]string{e.SourceLegalEntityID, e.TargetLegalEntityID, e.CurrencyCode, e.MatchStatus}, "|")
		g, ok := grouped[key]
		if !ok {
			g = &domain.ReciprocalBalanceGroup{
				SourceLegalEntityID: e.SourceLegalEntityID, TargetLegalEntityID: e.TargetLegalEntityID,
				CurrencyCode: e.CurrencyCode, MatchStatus: e.MatchStatus,
			}
			grouped[key] = g
		}
		if g.Amount == "" {
			g.Amount = e.Amount
			continue
		}
		total, err := domain.SumAmounts([]string{string(g.Amount), string(e.Amount)})
		if err != nil {
			return nil, err
		}
		amount, err := domain.ExactDecimal(total)
		if err != nil {
			return nil, err
		}
		g.Amount = amount
	}

	out := make([]domain.ReciprocalBalanceGroup, 0, len(grouped))
	for _, g := range grouped {
		out = append(out, *g)
	}
	return out, nil
}

// seedBalance adds one pair to the stub store with an explicit period and
// status, so a test names exactly the group it seeds.
func seedBalance(s *stubStoreReal, source, target, currency, amount, status, period string) {
	id := strings.Join([]string{source, target, currency, amount, status, period}, "|")
	p := period
	s.entries[id] = &domain.IntercompanyEntry{
		IntercompanyEntryID: id, TenantID: "tenant-abc",
		SourceLegalEntityID: source, TargetLegalEntityID: target, SourceJournalID: "j-" + id,
		Amount: domain.Decimal(amount), CurrencyCode: currency,
		CounterpartyAmount: domain.Decimal(amount), CounterpartyCurrencyCode: currency,
		FiscalPeriod: &p, Dimensions: domain.Dimensions{}, MatchStatus: status,
	}
}

func reciprocalURL(entityA, entityB, period string) string {
	return "/v1/intercompany/reciprocal-balances?entity_a=" + entityA + "&entity_b=" + entityB + "&period=" + period
}

func statusBalances(t *testing.T, rb *domain.ReciprocalBalances, status string) domain.ReciprocalStatusBalances {
	t.Helper()
	for _, s := range rb.ByMatchStatus {
		if s.MatchStatus == status {
			return s
		}
	}
	t.Fatalf("no %s slice in %+v", status, rb.ByMatchStatus)
	return domain.ReciprocalStatusBalances{}
}

func assertCurrencyTotals(t *testing.T, what string, got []domain.ReciprocalCurrencyTotal, want ...domain.ReciprocalCurrencyTotal) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: expected %+v, got %+v", what, want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("%s[%d]: expected %+v, got %+v", what, i, want[i], got[i])
		}
	}
}

// One fixture with everything the deviation names: two currencies that must
// never be co-mingled, both directions, a status split, CLOSED counted, and
// a row outside the pair and period that must not appear.
func TestGetReciprocalBalances_SplitsByStatusAndCurrency(t *testing.T) {
	s := newStubStoreReal()
	// Direction A→B, MATCHED: USD in two groups that must sum exactly, and
	// GBP that must stay its own currency.
	seedBalance(s, "le-a", "le-b", "USD", "100.0001", domain.MatchStatusMatched, "2026-09")
	seedBalance(s, "le-a", "le-b", "USD", "0.3", domain.MatchStatusMatched, "2026-09")
	seedBalance(s, "le-a", "le-b", "GBP", "50.5", domain.MatchStatusMatched, "2026-09")
	// Direction B→A, MATCHED.
	seedBalance(s, "le-b", "le-a", "USD", "30", domain.MatchStatusMatched, "2026-09")
	// Other statuses: UNMATCHED, and CLOSED — still a real settled position.
	seedBalance(s, "le-a", "le-b", "USD", "7", domain.MatchStatusUnmatched, "2026-09")
	seedBalance(s, "le-a", "le-b", "USD", "12", domain.MatchStatusClosed, "2026-09")
	seedBalance(s, "le-b", "le-a", "GBP", "9.5", domain.MatchStatusUnmatched, "2026-09")
	// Neither of these belongs to the answer: another period, another pair.
	seedBalance(s, "le-a", "le-b", "USD", "999", domain.MatchStatusMatched, "2026-08")
	seedBalance(s, "le-a", "le-c", "USD", "999", domain.MatchStatusMatched, "2026-09")

	resp := doReq(newRouter(s, &stubAuthZ{}, &stubLedger{}), http.MethodGet, reciprocalURL("le-a", "le-b", "2026-09"), nil, "user-1")
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.Code, resp.Body.String())
	}
	var got domain.ReciprocalBalances
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.EntityA != "le-a" || got.EntityB != "le-b" || got.FiscalPeriod != "2026-09" {
		t.Fatalf("unexpected echo: %+v", got)
	}

	// Statuses present, in the spec's own state-model order.
	var ordered []string
	for _, s := range got.ByMatchStatus {
		ordered = append(ordered, s.MatchStatus)
	}
	if want := []string{domain.MatchStatusUnmatched, domain.MatchStatusMatched, domain.MatchStatusClosed}; !slices.Equal(ordered, want) {
		t.Fatalf("expected statuses %v, got %v", want, ordered)
	}

	matched := statusBalances(t, &got, domain.MatchStatusMatched)
	assertCurrencyTotals(t, "MATCHED A→B", matched.EntityAToEntityB,
		domain.ReciprocalCurrencyTotal{CurrencyCode: "GBP", Amount: "50.5"},
		domain.ReciprocalCurrencyTotal{CurrencyCode: "USD", Amount: "100.3001"})
	assertCurrencyTotals(t, "MATCHED B→A", matched.EntityBToEntityA,
		domain.ReciprocalCurrencyTotal{CurrencyCode: "USD", Amount: "30"})
	// Net per currency: USD 100.3001−30 and GBP 50.5−0, never USD+GBP.
	assertCurrencyTotals(t, "MATCHED net", matched.Net,
		domain.ReciprocalCurrencyTotal{CurrencyCode: "GBP", Amount: "50.5"},
		domain.ReciprocalCurrencyTotal{CurrencyCode: "USD", Amount: "70.3001"})

	unmatched := statusBalances(t, &got, domain.MatchStatusUnmatched)
	assertCurrencyTotals(t, "UNMATCHED A→B", unmatched.EntityAToEntityB,
		domain.ReciprocalCurrencyTotal{CurrencyCode: "USD", Amount: "7"})
	assertCurrencyTotals(t, "UNMATCHED B→A", unmatched.EntityBToEntityA,
		domain.ReciprocalCurrencyTotal{CurrencyCode: "GBP", Amount: "9.5"})
	assertCurrencyTotals(t, "UNMATCHED net", unmatched.Net,
		domain.ReciprocalCurrencyTotal{CurrencyCode: "GBP", Amount: "-9.5"},
		domain.ReciprocalCurrencyTotal{CurrencyCode: "USD", Amount: "7"})

	// CLOSED is terminal but its position is real: it must be counted.
	closed := statusBalances(t, &got, domain.MatchStatusClosed)
	assertCurrencyTotals(t, "CLOSED A→B", closed.EntityAToEntityB,
		domain.ReciprocalCurrencyTotal{CurrencyCode: "USD", Amount: "12"})
	assertCurrencyTotals(t, "CLOSED B→A", closed.EntityBToEntityA)
	assertCurrencyTotals(t, "CLOSED net", closed.Net,
		domain.ReciprocalCurrencyTotal{CurrencyCode: "USD", Amount: "12"})
}

// A pair with no positions in the period answers with an explicit empty
// list, never a null body a consumer would have to special-case.
func TestGetReciprocalBalances_NoPositions(t *testing.T) {
	s := newStubStoreReal()
	resp := doReq(newRouter(s, &stubAuthZ{}, &stubLedger{}), http.MethodGet, reciprocalURL("le-a", "le-b", "2026-09"), nil, "user-1")
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.Code, resp.Body.String())
	}
	if body := resp.Body.String(); !strings.Contains(body, `"by_match_status":[]`) {
		t.Fatalf("expected an empty by_match_status list, got %s", body)
	}
	var got domain.ReciprocalBalances
	if err := json.NewDecoder(strings.NewReader(resp.Body.String())).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.ByMatchStatus == nil {
		t.Fatal("by_match_status decoded as null")
	}
}

// A reciprocal balance is both sides' position: a principal allowed to see
// intercompany data for one entity only is refused.
func TestGetReciprocalBalances_RefusedWithoutEitherEntity(t *testing.T) {
	for _, deny := range []string{"le-a", "le-b"} {
		s := newStubStoreReal()
		authz := &entityAuthZ{deny: deny}
		resp := doReq(newRouter(s, authz, &stubLedger{}), http.MethodGet, reciprocalURL("le-a", "le-b", "2026-09"), nil, "user-1")
		if resp.Code != http.StatusForbidden {
			t.Errorf("deny %s: expected 403, got %d: %s", deny, resp.Code, resp.Body.String())
		}
		if !slices.Contains(authz.checked, deny) {
			t.Errorf("deny %s: the denied entity was never checked (%v)", deny, authz.checked)
		}
	}
}

// Both entities are checked, and a fully authorized caller passes.
func TestGetReciprocalBalances_AuthorizedOnBothEntities(t *testing.T) {
	s := newStubStoreReal()
	authz := &entityAuthZ{}
	resp := doReq(newRouter(s, authz, &stubLedger{}), http.MethodGet, reciprocalURL("le-a", "le-b", "2026-09"), nil, "user-1")
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.Code, resp.Body.String())
	}
	if want := []string{"le-a", "le-b"}; !reflect.DeepEqual(authz.checked, want) {
		t.Fatalf("expected both entities checked, got %v", authz.checked)
	}
}

func TestGetReciprocalBalances_Validation(t *testing.T) {
	r := newRouter(newStubStoreReal(), &stubAuthZ{}, &stubLedger{})

	for name, path := range map[string]string{
		"no params":       "/v1/intercompany/reciprocal-balances",
		"no period":       "/v1/intercompany/reciprocal-balances?entity_a=le-a&entity_b=le-b",
		"no entity_b":     "/v1/intercompany/reciprocal-balances?entity_a=le-a&period=2026-09",
		"same entity":     reciprocalURL("le-a", "le-a", "2026-09"),
		"period too long": reciprocalURL("le-a", "le-b", strings.Repeat("9", 21)),
		"empty period":    reciprocalURL("le-a", "le-b", ""),
	} {
		resp := doReq(r, http.MethodGet, path, nil, "user-1")
		if resp.Code != http.StatusBadRequest {
			t.Errorf("%s: expected 400, got %d: %s", name, resp.Code, resp.Body.String())
		}
	}

	resp := doReq(r, http.MethodGet, reciprocalURL("le-a", "le-b", "2026-09"), nil, "")
	if resp.Code != http.StatusUnauthorized {
		t.Errorf("missing principal: expected 401, got %d", resp.Code)
	}
}

// A fifth decimal place would be rounded away by NUMERIC(18,4) on insert —
// 10.00005 would be stored as 10.0000 or 10.0001, a number the caller never
// sent. It is refused at the door (domain.ValidatePositiveAmount, A4/B7), so
// no reciprocal balance can ever be summed from a silently rounded amount.
func TestCreateEntry_RefusesAmountNumericWouldRound(t *testing.T) {
	s := newStubStoreReal()
	resp := doReq(newRouter(s, &stubAuthZ{}, &stubLedger{}), http.MethodPost, "/v1/intercompany/entries/",
		createBody(map[string]any{"amount": "10.00005", "fiscal_period": "2026-09"}), "p-1")
	if resp.Code != http.StatusBadRequest || !strings.Contains(resp.Body.String(), "invalid_amount") {
		t.Fatalf("expected 400 invalid_amount, got %d: %s", resp.Code, resp.Body.String())
	}
	if len(s.entries) != 0 {
		t.Fatalf("a rounded amount reached the store: %+v", s.entries)
	}
}
