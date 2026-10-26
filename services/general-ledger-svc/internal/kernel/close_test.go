package kernel

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestHMACSignerSignsAndVerifies(t *testing.T) {
	s := HMACSigner{Key: []byte("0123456789abcdef0123456789abcdef"), KeyID: "k1"}
	sig, id := s.Sign("hash-1")
	if len(sig) != 64 || id != "k1" {
		t.Fatalf("%q %q", sig, id)
	}
	if ok, known := s.Verify("hash-1", sig, "k1"); !ok || !known {
		t.Fatal("a signature verifies under its own key")
	}
	if ok, _ := s.Verify("hash-2", sig, "k1"); ok {
		t.Fatal("a signature does not verify for other content")
	}
	if ok, known := s.Verify("hash-1", sig, "k0"); ok || known {
		t.Fatal("an unknown key id is reported as unknown, not as a bad signature")
	}
	other := HMACSigner{Key: []byte("a-different-key-a-different-key!!"), KeyID: "k1"}
	if ok, _ := other.Verify("hash-1", sig, "k1"); ok {
		t.Fatal("another key does not verify it")
	}
}

func TestSnapshotContentHashCoversEverythingThatMatters(t *testing.T) {
	base := func() *BalanceSnapshot {
		return &BalanceSnapshot{TenantID: "t", LegalEntityID: "e", BookID: "b", FiscalPeriod: "2026-03", AsOf: "2026-03-31", LedgerWatermark: 10,
			Lines: []SnapshotLine{{AccountCode: "1100", Debit: "10.00", Credit: "0.00", Net: "10.00"}}}
	}
	h := contentHash(base())
	if h != contentHash(base()) {
		t.Fatal("deterministic")
	}
	for name, mut := range map[string]func(*BalanceSnapshot){
		"watermark": func(s *BalanceSnapshot) { s.LedgerWatermark = 11 },
		"amount":    func(s *BalanceSnapshot) { s.Lines[0].Debit = "10.01" },
		"account":   func(s *BalanceSnapshot) { s.Lines[0].AccountCode = "1101" },
		"as of":     func(s *BalanceSnapshot) { s.AsOf = "2026-03-30" },
		"book":      func(s *BalanceSnapshot) { s.BookID = "b2" },
		"tenant":    func(s *BalanceSnapshot) { s.TenantID = "t2" },
	} {
		s := base()
		mut(s)
		if contentHash(s) == h {
			t.Fatalf("a change of %s must change the hash", name)
		}
	}
}

func TestPeriodBounds(t *testing.T) {
	s, e, ok := periodBounds("2026-02")
	if !ok || s.Format("2006-01-02") != "2026-02-01" || e.Format("2006-01-02") != "2026-02-28" {
		t.Fatalf("%v %v %v", s, e, ok)
	}
	if _, e, _ := periodBounds("2028-02"); e.Day() != 29 {
		t.Fatal("leap year")
	}
	if _, _, ok := periodBounds("March"); ok {
		t.Fatal("a period is YYYY-MM")
	}
}

// fakeRates is a rate store with one fixed rate, to test the resolver's age and direction handling without a database.
type fakeRates struct {
	rate FXRate
	err  error
	asks []string
}

func (f *fakeRates) InsertRate(context.Context, *FXRate, OutEvent) (*FXRate, bool, error) {
	return nil, false, nil
}
func (f *fakeRates) ListRates(context.Context, string, FXFilter) ([]FXRate, error) { return nil, nil }
func (f *fakeRates) BestRate(_ context.Context, _, base, quote, typ string, _ time.Time) (*FXRate, error) {
	f.asks = append(f.asks, base+quote+typ)
	if f.err != nil {
		return nil, f.err
	}
	if base == f.rate.Base && quote == f.rate.Quote && typ == f.rate.RateType {
		r := f.rate
		return &r, nil
	}
	return nil, ErrNotFound
}
func (f *fakeRates) InsertOverride(context.Context, *FXOverride, OutEvent) error { return nil }
func (f *fakeRates) GetOverride(context.Context, string, string) (*FXOverride, error) {
	return nil, ErrNotFound
}
func (f *fakeRates) ListOverrides(context.Context, string, string) ([]FXOverride, error) {
	return nil, nil
}
func (f *fakeRates) DecideOverride(context.Context, string, string, string, bool, string, time.Time, OutEvent) (*FXOverride, *FXRate, error) {
	return nil, nil, ErrNotFound
}

func TestRateResolverAgeAndDirection(t *testing.T) {
	f := &fakeRates{rate: FXRate{FXRateID: "r1", Base: "USD", Quote: "GBP", Rate: "0.8", RateType: "SPOT", RateDate: "2026-03-10", Source: "ecb"}}
	rr := RateResolver{Repo: f, Tenant: "t"}
	day := func(d int) time.Time { return time.Date(2026, 3, d, 15, 0, 0, 0, time.UTC) }

	b, err := rr.Basis(context.Background(), "", "USD", "GBP", day(10))
	if err != nil || b.Rate != "0.8" || b.RateID != "r1" || b.Source != "ecb" || b.AsOf != "2026-03-10" {
		t.Fatalf("%+v %v", b, err)
	}
	if _, err = rr.Basis(context.Background(), "SPOT_ON_DATE", "USD", "GBP", day(13)); err != nil {
		t.Fatalf("three days old is within the spot policy: %v", err)
	}
	e, ok := AsException(func() error {
		_, err := rr.Basis(context.Background(), "SPOT_ON_DATE", "USD", "GBP", day(14))
		return err
	}())
	if !ok || e.Code != "RATE_STALE" || e.Family != FamFX {
		t.Fatalf("four days old is stale: %v", e)
	}
	if b, err = rr.Basis(context.Background(), "", "GBP", "USD", day(10)); err != nil || b.Rate != "1.25" || b.RateID != "r1:inverse" {
		t.Fatalf("the other direction is derived and says so: %+v %v", b, err)
	}
	if e, ok = AsException(func() error { _, err := rr.Basis(context.Background(), "", "EUR", "GBP", day(10)); return err }()); !ok || e.Code != "RATE_MISSING" {
		t.Fatalf("%v", e)
	}
	if e, ok = AsException(func() error { _, err := rr.Basis(context.Background(), "NOPE", "USD", "GBP", day(10)); return err }()); !ok || e.Code != "POLICY_UNKNOWN" {
		t.Fatalf("%v", e)
	}
	f.err = errors.New("db down")
	if e, ok = AsException(func() error { _, err := rr.Basis(context.Background(), "", "USD", "GBP", day(10)); return err }()); !ok || e.Family != FamExt {
		t.Fatalf("an unreadable store is an outage, not a missing rate: %v", e)
	}
}

func TestCheckRateRefusesWhatIsNotARate(t *testing.T) {
	at := time.Date(2026, 3, 10, 0, 0, 0, 0, time.UTC)
	if _, _, err := checkRate("USD", "GBP", "0.123456789012", "SPOT", at, "ecb", "1"); err != nil {
		t.Fatalf("12 decimals are allowed: %v", err)
	}
	for name, c := range map[string][]string{
		"13 decimals": {"USD", "GBP", "0.1234567890123", "SPOT"}, "negative": {"USD", "GBP", "-1", "SPOT"}, "same pair": {"USD", "USD", "1", "SPOT"},
		"not a number": {"USD", "GBP", "abc", "SPOT"}, "bad type": {"USD", "GBP", "1", "LIVE"}, "huge": {"USD", "GBP", "1000000000000", "SPOT"},
	} {
		if _, _, err := checkRate(c[0], c[1], c[2], c[3], at, "ecb", "1"); err == nil {
			t.Fatalf("%s must be refused", name)
		}
	}
	if _, _, err := checkRate("USD", "GBP", "1", "SPOT", at, "", "1"); err == nil {
		t.Fatal("a rate names its source")
	}
	if _, _, err := checkRate("USD", "GBP", "1", "SPOT", time.Time{}, "ecb", "1"); err == nil {
		t.Fatal("a rate has an effective time")
	}
}
