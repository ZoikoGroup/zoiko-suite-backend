package source

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"zoiko.io/reconciliation-engine-svc/internal/domain"
)

// Ported from financial-control-svc's internal/source tests: the client is a
// copy, so it must keep the same fail-closed behaviour.

func recs(n int) []domain.PopulationRecord {
	out := make([]domain.PopulationRecord, n)
	for i := range out {
		out[i] = domain.PopulationRecord{RecordID: fmt.Sprintf("r%05d", i), Reference: fmt.Sprintf("REF-%d", i),
			Amount: "1.50", Currency: "USD", Date: "2026-09-01"}
	}
	return out
}

// server pages `data` by the cursor (an offset) and lets a test tweak each page.
func server(t *testing.T, data []domain.PopulationRecord, tweak func(page int, r *pageResponse)) (*httptest.Server, *[]*http.Request) {
	t.Helper()
	var seen []*http.Request
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Clone(context.Background()))
		if r.URL.Path != "/v1/control-populations/invoices" {
			http.NotFound(w, r)
			return
		}
		off, _ := strconv.Atoi(r.URL.Query().Get("cursor"))
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		end := off + limit
		resp := pageResponse{Watermark: "wm-1", DeclaredTotals: declare(data)}
		if end < len(data) {
			resp.NextCursor = strconv.Itoa(end)
		} else {
			end = len(data)
		}
		resp.Records = data[off:end]
		if tweak != nil {
			tweak(off/limit, &resp)
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(s.Close)
	return s, &seen
}

// declare computes the honest whole-population totals a conforming source sends.
func declare(data []domain.PopulationRecord) *declaredTotals {
	d := &declaredTotals{RowCount: len(data), Totals: map[string]string{}}
	got, err := domain.DecimalTotals(data)
	if err != nil {
		return d
	}
	for c, v := range got {
		d.Totals[c] = v.FloatString(2)
	}
	return d
}

var scope = Scope{TenantID: "t1", PrincipalID: "alice", LegalEntityID: "e1", PeriodID: "2026-09", CorrelationID: "c1"}
var spec = domain.PopulationSpec{Side: "AR", System: "ar", Population: "invoices"}

func fetcher(s *httptest.Server) *HTTPFetcher {
	return NewHTTPFetcher(map[string]string{"ar": s.URL}, nil)
}

func TestFetch_PagesAndForwardsVerifiedScope(t *testing.T) {
	s, seen := server(t, recs(2500), nil)
	got, err := fetcher(s).Fetch(context.Background(), spec, scope)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Records) != 2500 || got.Watermark != "wm-1" || got.DeclaredRowCount != 2500 || got.DeclaredTotals["USD"] != "3750.00" {
		t.Fatalf("fetched: %d records, watermark %q, declared %d %v", len(got.Records), got.Watermark, got.DeclaredRowCount, got.DeclaredTotals)
	}
	if len(*seen) != 3 {
		t.Fatalf("pages requested: %d, want 3", len(*seen))
	}
	first := (*seen)[0]
	if first.Header.Get("X-Tenant-Id") != "t1" || first.Header.Get("X-Principal-Id") != "alice" || first.Header.Get("X-Legal-Entity-Id") != "e1" {
		t.Fatalf("verified scope not forwarded: %v", first.Header)
	}
	q := first.URL.Query()
	if q.Get("legal_entity_id") != "e1" || q.Get("period_id") != "2026-09" {
		t.Fatalf("query: %v", q)
	}
}

func TestFetch_RefusesUnregisteredSystemAndBadNames(t *testing.T) {
	f := NewHTTPFetcher(map[string]string{}, nil)
	if _, err := f.Fetch(context.Background(), spec, scope); !errors.Is(err, ErrSourceUnknown) {
		t.Fatalf("unregistered system: %v", err)
	}
	s, _ := server(t, recs(1), nil)
	bad := domain.PopulationSpec{Side: "AR", System: "ar", Population: "../../etc/passwd"}
	if _, err := fetcher(s).Fetch(context.Background(), bad, scope); !errors.Is(err, domain.ErrInvalidArgument) {
		t.Fatalf("path traversal in population name: %v", err)
	}
}

func TestFetch_NoWatermarkIsNotReproducible(t *testing.T) {
	s, _ := server(t, recs(3), func(_ int, r *pageResponse) { r.Watermark = "" })
	if _, err := fetcher(s).Fetch(context.Background(), spec, scope); !errors.Is(err, ErrSourceInconsistent) {
		t.Fatalf("no watermark: %v", err)
	}
}

func TestFetch_WatermarkMovingMidExtractionFails(t *testing.T) {
	s, _ := server(t, recs(2500), func(page int, r *pageResponse) { r.Watermark = fmt.Sprintf("wm-%d", page) })
	if _, err := fetcher(s).Fetch(context.Background(), spec, scope); !errors.Is(err, ErrSourceInconsistent) {
		t.Fatalf("moving watermark: %v", err)
	}
}

func TestFetch_DeclaredTotalsMustTie(t *testing.T) {
	data := recs(4)
	s, _ := server(t, data, func(_ int, r *pageResponse) {
		r.DeclaredTotals = &declaredTotals{RowCount: 4, Totals: map[string]string{"USD": "6.000"}}
	})
	if _, err := fetcher(s).Fetch(context.Background(), spec, scope); err != nil {
		t.Fatalf("6.000 and 4 x 1.50 are the same exact total: %v", err)
	}

	for name, d := range map[string]*declaredTotals{
		"row count":      {RowCount: 5, Totals: map[string]string{"USD": "6"}},
		"currency total": {RowCount: 4, Totals: map[string]string{"USD": "6.01"}},
		"missing ccy":    {RowCount: 4, Totals: map[string]string{}},
		"extra ccy":      {RowCount: 4, Totals: map[string]string{"USD": "6", "EUR": "9"}},
		"garbage total":  {RowCount: 4, Totals: map[string]string{"USD": "six"}},
	} {
		d := d
		s, _ := server(t, data, func(_ int, r *pageResponse) { r.DeclaredTotals = d })
		if _, err := fetcher(s).Fetch(context.Background(), spec, scope); !errors.Is(err, ErrSourceInconsistent) {
			t.Fatalf("%s: %v, want ErrSourceInconsistent", name, err)
		}
	}
}

func TestFetch_MissingDeclaredTotalsFails(t *testing.T) {
	for name, page := range map[string]int{"first page": 0, "later page": 1} {
		page := page
		s, _ := server(t, recs(1500), func(p int, r *pageResponse) {
			if p == page {
				r.DeclaredTotals = nil
			}
		})
		if _, err := fetcher(s).Fetch(context.Background(), spec, scope); !errors.Is(err, ErrSourceInconsistent) {
			t.Fatalf("%s: the contract requires declared_totals on every page: %v", name, err)
		}
	}
}

func TestFetch_DeclaredTotalsChangingBetweenPagesFails(t *testing.T) {
	s, _ := server(t, recs(1500), func(page int, r *pageResponse) {
		if page == 1 {
			r.DeclaredTotals = &declaredTotals{RowCount: 1500, Totals: map[string]string{"USD": "9999"}}
		}
	})
	if _, err := fetcher(s).Fetch(context.Background(), spec, scope); !errors.Is(err, ErrSourceInconsistent) {
		t.Fatalf("declared totals changed between pages: %v", err)
	}
}

func TestFetch_SourceDownIsUnavailable(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer s.Close()
	if _, err := fetcher(s).Fetch(context.Background(), spec, scope); !errors.Is(err, ErrSourceUnavailable) {
		t.Fatalf("503 from source: %v", err)
	}
}

func TestFetch_CursorMustAdvance(t *testing.T) {
	s, _ := server(t, recs(10), func(_ int, r *pageResponse) { r.NextCursor = "0" })
	if _, err := fetcher(s).Fetch(context.Background(), spec, scope); !errors.Is(err, ErrSourceInconsistent) {
		t.Fatalf("stuck cursor: %v", err)
	}
}

func TestFetch_InvalidRecordFailsPopulation(t *testing.T) {
	data := recs(3)
	data[1].Amount = "1.5.0"
	s, _ := server(t, data, nil)
	if _, err := fetcher(s).Fetch(context.Background(), spec, scope); !errors.Is(err, domain.ErrInvalidArgument) {
		t.Fatalf("invalid record: %v", err)
	}
}

func TestResolveScopeParams(t *testing.T) {
	sp := domain.PopulationSpec{Side: "B", System: "bank", Population: "bank-transactions",
		Params: map[string]string{"bank_account_id": "${scope.bank_account_id}", "fixed": "x"}}
	got, err := ResolveScopeParams(sp, map[string]string{"bank_account_id": "ba-1"})
	if err != nil || got.Params["bank_account_id"] != "ba-1" || got.Params["fixed"] != "x" {
		t.Fatalf("bound: %+v %v", got.Params, err)
	}
	if sp.Params["bank_account_id"] != "${scope.bank_account_id}" {
		t.Fatal("binding mutated the definition's spec")
	}
	if _, err := ResolveScopeParams(sp, nil); !errors.Is(err, domain.ErrInvalidArgument) {
		t.Fatalf("missing scope key must fail closed: %v", err)
	}
}

// fakeFetcher returns canned populations by system name.
type fakeFetcher map[string]*Fetched

func (f fakeFetcher) Fetch(_ context.Context, s domain.PopulationSpec, _ Scope) (*Fetched, error) {
	got, ok := f[s.System]
	if !ok {
		return nil, ErrSourceUnavailable
	}
	return got, nil
}

func TestBuildPopulations(t *testing.T) {
	def := domain.ReconciliationDefinition{AmountScale: 2, Populations: []domain.PopulationSpec{
		{Side: "AR", System: "ar", Population: "invoices"},
		{Side: "GL", System: "gl", Population: "account-postings"},
	}}
	ar := recs(2)
	f := fakeFetcher{
		"ar": {Records: ar, Watermark: "w-ar", DeclaredRowCount: 2, DeclaredTotals: map[string]string{"USD": "3"}},
		"gl": {Records: nil, Watermark: "w-gl", DeclaredRowCount: 0, DeclaredTotals: map[string]string{}},
	}
	pops, err := BuildPopulations(context.Background(), f, def, domain.StartRunRequest{DefinitionID: "d"}, scope)
	if err != nil {
		t.Fatal(err)
	}
	if len(pops) != 2 || pops[0].Watermark != "w-ar" || len(pops[0].Items) != 2 || pops[0].Items[0].AmountMinorUnits != 150 ||
		pops[1].Side != "GL" || len(pops[1].Items) != 0 {
		t.Fatalf("populations: %+v", pops)
	}

	delete(f, "gl")
	if _, err := BuildPopulations(context.Background(), f, def, domain.StartRunRequest{DefinitionID: "d"}, scope); !errors.Is(err, ErrSourceUnavailable) {
		t.Fatalf("one side unavailable must refuse the whole run: %v", err)
	}
}

type errFetcher struct{ err error }

func (f errFetcher) Fetch(context.Context, domain.PopulationSpec, Scope) (*Fetched, error) {
	return nil, f.err
}

// A population that was attempted and could not be proven is a
// *PopulationError carrying the run's failure code; an error in the request
// itself (a scope value the run does not carry) is not, and starts nothing.
func TestBuildPopulations_ClassifiesFailures(t *testing.T) {
	def := domain.ReconciliationDefinition{AmountScale: 2, Populations: []domain.PopulationSpec{
		{Side: "AR", System: "ar", Population: "invoices"},
		{Side: "GL", System: "gl", Population: "account-postings"},
	}}
	req := domain.StartRunRequest{DefinitionID: "d"}
	for name, tc := range map[string]struct {
		f    Fetcher
		want domain.FailureCode
	}{
		"down":         {errFetcher{fmt.Errorf("%w: timeout", ErrSourceUnavailable)}, domain.FailureSourceUnavailable},
		"unregistered": {errFetcher{fmt.Errorf("%w: %q", ErrSourceUnknown, "ar")}, domain.FailureSourceUnregistered},
		"untied":       {errFetcher{fmt.Errorf("%w: totals", ErrSourceInconsistent)}, domain.FailureSourceInconsistent},
		"too large":    {errFetcher{ErrPopulationTooLarge}, domain.FailurePopulationTooLarge},
		"excess precision": {fakeFetcher{
			"ar": {Records: []domain.PopulationRecord{{RecordID: "r1", Reference: "R", Amount: "1.505", Currency: "USD", Date: "2026-09-01"}},
				Watermark: "w", DeclaredRowCount: 1, DeclaredTotals: map[string]string{"USD": "1.505"}},
		}, domain.FailurePopulationInvalid},
	} {
		_, err := BuildPopulations(context.Background(), tc.f, def, req, scope)
		var pe *PopulationError
		if !errors.As(err, &pe) || pe.Code() != tc.want || pe.Side != "AR" {
			t.Errorf("%s: %v, want a PopulationError on AR with code %s", name, err, tc.want)
		}
	}

	scoped := def
	scoped.Populations = []domain.PopulationSpec{
		{Side: "AR", System: "ar", Population: "invoices", Params: map[string]string{"as_of": "${scope.as_of}"}},
		def.Populations[1],
	}
	_, err := BuildPopulations(context.Background(), errFetcher{ErrSourceUnavailable}, scoped, req, scope)
	var pe *PopulationError
	if errors.As(err, &pe) || !errors.Is(err, domain.ErrInvalidArgument) {
		t.Fatalf("a missing run scope is a request error, not a population finding: %v", err)
	}
}
