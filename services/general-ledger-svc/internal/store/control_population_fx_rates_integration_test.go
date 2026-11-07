//go:build integration

package store_test

import (
	"context"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"

	"zoiko.io/general-ledger-svc/internal/domain"
)

func (f *w4Fixture) insertRate(t *testing.T, tenant, base, quote, rateType, rate, date, source string) {
	t.Helper()
	if err := fixtureExec(context.Background(), f.pool,
		`INSERT INTO fx_rates (fx_rate_id, tenant_id, base_currency, quote_currency, rate, rate_type, effective_at, rate_date, source, source_rate_id, ingested_by)
		 VALUES ($1::uuid, $2::uuid, $3, $4, $5::numeric, $6, ($7::date)::timestamptz, $7::date, $8, $9, 'treasury-feed')`,
		uuid.NewString(), tenant, base, quote, rate, rateType, date, source, uuid.NewString()); err != nil {
		t.Fatalf("insert rate: %v", err)
	}
}

func (f *w4Fixture) fxObs(t *testing.T, tenant, period, rateType string, limit int, after string) *domain.ControlPopulationPage {
	t.Helper()
	p, err := f.s.QueryFXRateObservations(f.ctx, tenant, domain.FXRateObservationsQuery{PeriodID: period, RateType: rateType, Limit: limit, AfterRecordID: after})
	if err != nil {
		t.Fatalf("QueryFXRateObservations: %v", err)
	}
	return p
}

func TestFXRateObservations_MonthAndTypeScopeSourceAndPaging(t *testing.T) {
	f := newW4Fixture(t)
	f.insertRate(t, f.tenant, "USD", "GBP", "SPOT", "0.80", "2026-09-01", "ECB")    // in
	f.insertRate(t, f.tenant, "USD", "GBP", "SPOT", "0.81", "2026-09-02", "ECB")    // in
	f.insertRate(t, f.tenant, "EUR", "USD", "SPOT", "1.08", "2026-09-30", "ECB")    // in (last day)
	f.insertRate(t, f.tenant, "USD", "GBP", "SPOT", "0.82", "2026-10-01", "ECB")    // next month: out
	f.insertRate(t, f.tenant, "USD", "GBP", "SPOT", "0.79", "2026-08-31", "ECB")    // previous month: out
	f.insertRate(t, f.tenant, "USD", "GBP", "CLOSING", "0.83", "2026-09-30", "ECB") // other type: out of a SPOT test
	other := uuid.NewString()
	f.insertRate(t, other, "USD", "GBP", "SPOT", "9.99", "2026-09-05", "OTHER") // other tenant

	p := f.fxObs(t, f.tenant, "2026-09", "SPOT", 100, "")
	if len(p.Records) != 3 || p.DeclaredTotals.RowCount != 3 {
		t.Fatalf("only the three September SPOT rates of this tenant are in scope: %+v", p.Records)
	}
	seen := map[string]bool{}
	for _, r := range p.Records {
		seen[r.Date+"/"+r.Reference] = true
		if r.Currency != "XXX" || r.Attributes["pair"] != r.Reference || r.Attributes["rate_type"] != "SPOT" ||
			r.Attributes["source"] != "ECB" || r.Attributes["override"] != "false" || r.Attributes["source_rate_id"] == "" {
			t.Fatalf("record %+v", r)
		}
	}
	for _, want := range []string{"2026-09-01/USD/GBP", "2026-09-02/USD/GBP", "2026-09-30/EUR/USD"} {
		if !seen[want] {
			t.Fatalf("missing %s in %v", want, seen)
		}
	}
	if !strings.HasPrefix(p.Watermark, "gl11:n=3;entry_seq=0;md5=") {
		t.Fatalf("watermark %q", p.Watermark)
	}

	// The type is part of the scope: the CLOSING test sees only the closing rate.
	if c := f.fxObs(t, f.tenant, "2026-09", "CLOSING", 100, ""); len(c.Records) != 1 || c.Records[0].Attributes["rate_type"] != "CLOSING" {
		t.Fatalf("closing: %+v", c.Records)
	}
	// Another tenant sees only its own rate; an empty month is an empty population.
	if o := f.fxObs(t, other, "2026-09", "SPOT", 10, ""); len(o.Records) != 1 || o.Records[0].Attributes["source"] != "OTHER" {
		t.Fatalf("tenant isolation: %+v", o.Records)
	}
	if e := f.fxObs(t, f.tenant, "2026-07", "SPOT", 10, ""); len(e.Records) != 0 {
		t.Fatalf("empty month: %+v", e.Records)
	}
	if _, err := f.s.QueryFXRateObservations(f.ctx, f.tenant, domain.FXRateObservationsQuery{PeriodID: "2026-9", RateType: "SPOT", Limit: 10}); err == nil {
		t.Fatal("a malformed period is refused, never defaulted")
	}

	// A rate changes the watermark.
	before := p.Watermark
	f.insertRate(t, f.tenant, "GBP", "USD", "SPOT", "1.25", "2026-09-15", "ECB")
	if after := f.fxObs(t, f.tenant, "2026-09", "SPOT", 100, ""); after.Watermark == before || len(after.Records) != 4 {
		t.Fatalf("watermark must move when the population changes: %q vs %q", before, after.Watermark)
	}

	// Keyset paging is complete, ordered and under one watermark.
	var ids []string
	marks := map[string]bool{}
	cursor := ""
	for guard := 0; ; guard++ {
		if guard > 10 {
			t.Fatal("paging did not terminate")
		}
		pg := f.fxObs(t, f.tenant, "2026-09", "SPOT", 1, cursor)
		marks[pg.Watermark] = true
		for _, r := range pg.Records {
			ids = append(ids, r.RecordID)
		}
		if pg.NextRecordID == "" {
			break
		}
		cursor = pg.NextRecordID
	}
	if len(ids) != 4 || len(marks) != 1 || !sort.StringsAreSorted(ids) {
		t.Fatalf("ids %v, %d watermarks", ids, len(marks))
	}
}
