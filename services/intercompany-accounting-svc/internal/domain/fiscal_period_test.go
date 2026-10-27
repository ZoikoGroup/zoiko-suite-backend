package domain_test

import (
	"testing"
	"time"

	"zoiko.io/intercompany-accounting-svc/internal/domain"
)

func TestFiscalPeriodEnd(t *testing.T) {
	cases := []struct {
		period string
		want   time.Time
	}{
		// The control-population contract's "as of the end of that month"
		// cut-off: the last instant of the month, not its last whole day.
		{"2026-01", time.Date(2026, 1, 31, 23, 59, 59, 999999999, time.UTC)},
		{"2024-02", time.Date(2024, 2, 29, 23, 59, 59, 999999999, time.UTC)},
		{"2026-12", time.Date(2026, 12, 31, 23, 59, 59, 999999999, time.UTC)},
	}
	for _, c := range cases {
		got, ok := domain.FiscalPeriodEnd(c.period)
		if !ok {
			t.Errorf("FiscalPeriodEnd(%q): not ok", c.period)
			continue
		}
		if !got.Equal(c.want) {
			t.Errorf("FiscalPeriodEnd(%q) = %s, want %s", c.period, got.Format(time.RFC3339Nano), c.want.Format(time.RFC3339Nano))
		}
		if got.Location() != time.UTC {
			t.Errorf("FiscalPeriodEnd(%q) location = %s, want UTC", c.period, got.Location())
		}
	}
}

func TestFiscalPeriodEnd_NonCalendarPeriodIsNotInvented(t *testing.T) {
	// fiscal_period is a free 1-20 character reference (ValidFiscalPeriod)
	// and general-ledger-svc stores it verbatim, so only an exact YYYY-MM
	// has an end this service can name.
	for _, period := range []string{"", "2026-1", "2026-01-15", "2026-13", "FY26-P09", " 2026-01", "2026-01 "} {
		if got, ok := domain.FiscalPeriodEnd(period); ok {
			t.Errorf("FiscalPeriodEnd(%q) = %s, true; want false", period, got)
		}
	}
}
