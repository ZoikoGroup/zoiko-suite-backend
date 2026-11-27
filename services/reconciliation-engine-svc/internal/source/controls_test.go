package source

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"zoiko.io/reconciliation-engine-svc/internal/domain"
)

// ZS-SVC-Z-001 DQC-03 (step I): the source's optional declared controls are
// re-performed by the client, across every page, and fail the population closed.

func withSeq(n int) []domain.PopulationRecord {
	r := recs(n)
	for i := range r {
		r[i].Attributes = map[string]string{"seq": fmt.Sprint(i + 1)}
	}
	return r
}

func fetchWith(t *testing.T, data []domain.PopulationRecord, controls func(page int) domain.DeclaredControls) (*Fetched, error) {
	t.Helper()
	srv, _ := server(t, data, func(page int, r *pageResponse) {
		r.DeclaredTotals.DeclaredControls = controls(page)
	})
	f := NewHTTPFetcher(map[string]string{"ar": srv.URL}, nil)
	return f.Fetch(context.Background(), domain.PopulationSpec{Side: "A", System: "ar", Population: "invoices"}, Scope{TenantID: "t", PrincipalID: "p", LegalEntityID: "le"})
}

func TestFetch_UndeclaredControlsAreRecordedNotPassed(t *testing.T) {
	got, err := fetchWith(t, recs(3), func(int) domain.DeclaredControls { return domain.DeclaredControls{} })
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Outcomes) != 4 {
		t.Fatalf("outcomes = %+v", got.Outcomes)
	}
	for _, o := range got.Outcomes {
		if o.Status != domain.OutcomeNotDeclared {
			t.Fatalf("undeclared control recorded as %+v", o)
		}
	}
	if got.Controls.Partial() {
		t.Fatal("an undeclared scope must not read as PARTIAL")
	}
}

func TestFetch_DeclaredControlsHoldOverMultiplePages(t *testing.T) {
	data := withSeq(2500) // three pages at the client's page size
	hash, err := domain.SourceContentHash(data)
	if err != nil {
		t.Fatal(err)
	}
	c := domain.DeclaredControls{
		Hash:         &domain.DeclaredHash{Algorithm: "sha256", Serialization: "cpc-v1", Value: hash},
		Sequence:     &domain.DeclaredSequence{Attribute: "seq", First: 1, Last: 2500},
		RollForward:  &domain.DeclaredRollForward{Opening: map[string]string{"USD": "10"}, Closing: map[string]string{"USD": "3760.00"}, PeriodStart: "2026-09-01", PeriodEnd: "2026-09-30"},
		Completeness: &domain.DeclaredCompleteness{Scope: "FULL"},
	}
	got, err := fetchWith(t, data, func(int) domain.DeclaredControls { return c })
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range got.Outcomes {
		if o.Status == domain.OutcomeNotDeclared {
			t.Fatalf("declared control reported as not declared: %+v", o)
		}
	}
	if got.Controls.RollForward == nil || got.Controls.RollForward.Closing["USD"] != "3760.00" {
		t.Fatalf("controls not carried: %+v", got.Controls)
	}
}

func TestFetch_ABrokenDeclaredControlFailsThePopulationClosed(t *testing.T) {
	data := withSeq(5)
	good, _ := domain.SourceContentHash(data)
	cases := map[string]domain.DeclaredControls{
		"wrong hash":      {Hash: &domain.DeclaredHash{Algorithm: "sha256", Serialization: "cpc-v1", Value: "00" + good[2:]}},
		"unknown format":  {Hash: &domain.DeclaredHash{Algorithm: "sha256", Serialization: "cpc-v9", Value: good}},
		"sequence gap":    {Sequence: &domain.DeclaredSequence{Attribute: "seq", First: 1, Last: 6}},
		"roll-forward":    {RollForward: &domain.DeclaredRollForward{Opening: map[string]string{"USD": "0"}, Closing: map[string]string{"USD": "999"}}},
		"partial no why":  {Completeness: &domain.DeclaredCompleteness{Scope: "PARTIAL"}},
		"unknown scope":   {Completeness: &domain.DeclaredCompleteness{Scope: "SOME"}},
		"sequence absent": {Sequence: &domain.DeclaredSequence{Attribute: "no_such_attr", First: 1, Last: 5}},
	}
	for name, c := range cases {
		c := c
		t.Run(name, func(t *testing.T) {
			_, err := fetchWith(t, data, func(int) domain.DeclaredControls { return c })
			if !errors.Is(err, ErrSourceInconsistent) {
				t.Fatalf("err = %v, want ErrSourceInconsistent", err)
			}
			if code := (&PopulationError{Side: "A", Err: err}).Code(); code != domain.FailureSourceInconsistent {
				t.Fatalf("failure code = %s", code)
			}
		})
	}
}

func TestFetch_ControlsThatChangeBetweenPagesFailClosed(t *testing.T) {
	data := withSeq(1500) // two pages
	hash, _ := domain.SourceContentHash(data)
	c := domain.DeclaredControls{Hash: &domain.DeclaredHash{Algorithm: "sha256", Serialization: "cpc-v1", Value: hash}}
	_, err := fetchWith(t, data, func(page int) domain.DeclaredControls {
		if page == 1 { // the second page declares the population is a subset
			d := c
			d.Completeness = &domain.DeclaredCompleteness{Scope: "PARTIAL", Description: "x"}
			return d
		}
		return c
	})
	if !errors.Is(err, ErrSourceInconsistent) {
		t.Fatalf("err = %v, want ErrSourceInconsistent", err)
	}
}

func TestBuildPopulations_CarriesControlsAndOutcomesIntoTheFrozenPopulation(t *testing.T) {
	data := withSeq(3)
	srv, _ := server(t, data, func(_ int, r *pageResponse) {
		r.DeclaredTotals.Completeness = &domain.DeclaredCompleteness{Scope: "PARTIAL", Description: "EMEA only"}
	})
	f := NewHTTPFetcher(map[string]string{"ar": srv.URL}, nil)
	def := domain.ReconciliationDefinition{AmountScale: 2, Populations: []domain.PopulationSpec{
		{Side: "A", System: "ar", Population: "invoices"}, {Side: "B", System: "ar", Population: "invoices"}}}
	pops, err := BuildPopulations(context.Background(), f, def, domain.StartRunRequest{}, Scope{TenantID: "t", PrincipalID: "p", LegalEntityID: "le"})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range pops {
		if !p.Controls.Partial() || len(p.ControlOutcomes) != 4 || p.ControlOutcomes[3].Status != domain.OutcomePartial {
			t.Fatalf("population %s: controls %+v outcomes %+v", p.Side, p.Controls, p.ControlOutcomes)
		}
	}
}
