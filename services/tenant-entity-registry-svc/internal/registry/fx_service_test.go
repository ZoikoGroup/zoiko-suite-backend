package registry_test

// REF-03 service: currencies are confirmed read-only and fail closed, each command is its own action, a missing rate is
// an explicit error with no fallback, and derived rates are built from approved components in exact arithmetic.

import (
	"context"
	"math/big"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/tenant-entity-registry-svc/internal/currency"
	"zoiko.io/tenant-entity-registry-svc/internal/domain"
	"zoiko.io/tenant-entity-registry-svc/internal/registry"
)

type fxFake struct {
	*memStore
	registry.FXStore
	rates      map[string]string // "EUR/USD" -> rate, for the type, date and set asked
	calls      int
	actor      string
	lastKnown  time.Time
	lastSet    string
	lastIngest domain.IngestFXObservationRequest
}

func (f *fxFake) IngestFXObservation(_ context.Context, req domain.IngestFXObservationRequest, actor string) (*registry.FXIngestOutcome, error) {
	f.calls++
	f.actor, f.lastIngest = actor, req
	return &registry.FXIngestOutcome{}, nil
}

func (f *fxFake) GetApprovedFXComponent(_ context.Context, base, quote string, _ domain.FXRateType, md time.Time, set string, known time.Time) (*domain.FXComponent, error) {
	f.calls++
	f.lastKnown, f.lastSet = known, set
	r, ok := f.rates[base+"/"+quote]
	if !ok {
		return nil, nil
	}
	return &domain.FXComponent{ObservationID: "obs-" + base + quote, BaseCurrency: base, QuoteCurrency: quote, Rate: r, SourceID: "src",
		SetID: "set-1", SetVersion: 1, MarketDate: md.Format("2006-01-02")}, nil
}

type fakeCurrency struct {
	supported map[string]bool
	err       error
	asked     []string
}

func (c *fakeCurrency) Validate(_ context.Context, code string) (currency.Result, error) {
	c.asked = append(c.asked, code)
	if c.err != nil {
		return currency.Result{}, c.err
	}
	if c.supported[code] {
		return currency.Result{Supported: true}, nil
	}
	return currency.Result{Reason: "UNKNOWN_CURRENCY"}, nil
}

func fxSvc(t *testing.T, az registry.AuthorizationClient, cv currency.Validator) (*registry.Service, *fxFake) {
	t.Helper()
	f := &fxFake{memStore: newMemStore(), rates: map[string]string{"EUR/USD": "1.1", "USD/GBP": "0.8", "JPY/USD": "0.0067"}}
	svc := newSvc(t, f, az, acceptAllJurisd{})
	if cv != nil {
		svc.WithCurrencyValidator(cv)
	}
	return svc, f
}

func goodCurrencies() *fakeCurrency {
	return &fakeCurrency{supported: map[string]bool{"EUR": true, "USD": true, "GBP": true, "JPY": true}}
}

func fxIngestReq() domain.IngestFXObservationRequest {
	return domain.IngestFXObservationRequest{SourceID: "s", BaseCurrency: "EUR", QuoteCurrency: "USD", RateType: domain.FXSpot, Rate: "1.085",
		MarketDate: time.Now().UTC().Format("2006-01-02"), ObservedAt: time.Now().UTC().Add(-time.Hour), PayloadRef: "s3://x"}
}

func TestFX_IngestValidatesThenAuthorizesThenChecksCurrenciesThenStores(t *testing.T) {
	cur := goodCurrencies()
	svc, f := fxSvc(t, permitAllAuthZ{}, cur)
	bad := fxIngestReq()
	bad.Rate = "1e5"
	_, err := svc.IngestFXObservation(tenantCtx("tenant-001"), bad)
	require.ErrorIs(t, err, registry.ErrInvalidInput)
	assert.Empty(t, cur.asked)
	assert.Zero(t, f.calls)

	denied, f2 := fxSvc(t, denyAllAuthZ{}, cur)
	_, err = denied.IngestFXObservation(tenantCtx("tenant-001"), fxIngestReq())
	require.ErrorIs(t, err, registry.ErrUnauthorized)
	assert.Empty(t, cur.asked, "a denied caller makes the registry no calls")
	assert.Zero(t, f2.calls)

	_, err = svc.IngestFXObservation(tenantCtx("tenant-001"), fxIngestReq())
	require.NoError(t, err)
	assert.Equal(t, []string{"EUR", "USD"}, cur.asked, "both currencies are confirmed before anything is stored")
	assert.Equal(t, testPrincipal, f.actor, "the actor is the verified principal")

	f.tenants["tenant-001"].LifecycleState = domain.TenantLifecycleSuspended
	calls := f.calls
	_, err = svc.IngestFXObservation(tenantCtx("tenant-001"), fxIngestReq())
	require.Error(t, err)
	assert.Equal(t, calls, f.calls)
}

func TestFX_ACurrencyThatIsNotConfirmedIsNeverAccepted(t *testing.T) {
	cur := &fakeCurrency{supported: map[string]bool{"EUR": true}}
	svc, f := fxSvc(t, permitAllAuthZ{}, cur)
	_, err := svc.IngestFXObservation(tenantCtx("tenant-001"), fxIngestReq())
	require.ErrorIs(t, err, registry.ErrReferenceInvalid, "USD is not known to the registry")
	assert.Zero(t, f.calls)

	down, f2 := fxSvc(t, permitAllAuthZ{}, &fakeCurrency{err: currency.ErrUnavailable})
	_, err = down.IngestFXObservation(tenantCtx("tenant-001"), fxIngestReq())
	require.ErrorIs(t, err, registry.ErrServiceUnavailable, "a registry that cannot answer means the rate is not accepted")
	assert.Zero(t, f2.calls)

	none, f3 := fxSvc(t, permitAllAuthZ{}, nil)
	_, err = none.IngestFXObservation(tenantCtx("tenant-001"), fxIngestReq())
	require.ErrorIs(t, err, registry.ErrServiceUnavailable, "with no validator at all, fail closed")
	assert.Zero(t, f3.calls)
}

func TestFX_CommandsAreAuthorizedAsTheirOwnActions(t *testing.T) {
	for name, tc := range map[string]struct {
		resource, action string
		run              func(*registry.Service) error
	}{
		"source create": {"fx.source", "manage", func(s *registry.Service) error {
			_, err := s.CreateFXSource(tenantCtx("tenant-001"), domain.CreateFXSourceRequest{SourceCode: "c", Name: "n", SourceClass: domain.FXSourceProvider})
			return err
		}},
		"source withdraw": {"fx.source", "manage", func(s *registry.Service) error {
			_, err := s.WithdrawFXSource(tenantCtx("tenant-001"), "id", domain.WithdrawFXSourceRequest{Reason: "r"})
			return err
		}},
		"ingest": {"fx.observation", "ingest", func(s *registry.Service) error {
			_, err := s.IngestFXObservation(tenantCtx("tenant-001"), fxIngestReq())
			return err
		}},
		"supersede": {"fx.observation", "supersede", func(s *registry.Service) error {
			_, err := s.SupersedeFXObservation(tenantCtx("tenant-001"), "id", domain.SupersedeFXObservationRequest{Reason: "r", IngestFXObservationRequest: fxIngestReq()})
			return err
		}},
		"approve": {"fx.rate-set", "approve", func(s *registry.Service) error {
			_, err := s.ApproveFXRateSet(tenantCtx("tenant-001"), domain.ApproveFXRateSetRequest{RateType: domain.FXClosing, MarketDate: "2026-06-12", ObservationIDs: []string{"a"}, EvidenceRef: "e"})
			return err
		}},
		"close": {"fx.rate-set", "close", func(s *registry.Service) error {
			_, err := s.CloseFXRateSet(tenantCtx("tenant-001"), "id", domain.CloseFXRateSetRequest{EvidenceRef: "e"})
			return err
		}},
	} {
		t.Run(name, func(t *testing.T) {
			az := &recordingAuthZ{err: registry.ErrUnauthorized}
			svc, f := fxSvc(t, az, goodCurrencies())
			err := tc.run(svc)
			require.ErrorIs(t, err, registry.ErrUnauthorized)
			assert.Equal(t, tc.resource, az.resource)
			assert.Equal(t, tc.action, az.action)
			assert.Zero(t, f.calls)
		})
	}
}

func fxQ(base, quote string) registry.FXRateQuery {
	return registry.FXRateQuery{Base: base, Quote: quote, Type: domain.FXClosing, MarketDate: time.Date(2026, 6, 12, 0, 0, 0, 0, time.UTC)}
}

func TestFX_GetRateIsDirectOnlyAndAMissingRateIsExplicit(t *testing.T) {
	svc, f := fxSvc(t, permitAllAuthZ{}, goodCurrencies())
	r, err := svc.GetFXRate(tenantCtx("tenant-001"), fxQ("EUR", "USD"))
	require.NoError(t, err)
	assert.Equal(t, "1.1", r.Rate)
	assert.Equal(t, domain.FXDirect, r.Method)
	require.Len(t, r.Components, 1)
	assert.Equal(t, "obs-EURUSD", r.Components[0].ObservationID)
	assert.Equal(t, "DEFAULT", f.lastSet, "the default set is named, not guessed at")

	// An inverse exists, and still GetRate does not invert it: no silent fallback.
	_, err = svc.GetFXRate(tenantCtx("tenant-001"), fxQ("USD", "EUR"))
	require.ErrorIs(t, err, registry.ErrRateMissing)
	_, err = svc.GetFXRate(tenantCtx("tenant-001"), fxQ("EUR", "CHF"))
	require.ErrorIs(t, err, registry.ErrRateMissing)
	assert.ErrorIs(t, registry.ErrRateMissing, registry.ErrNotFound, "it is a 404, with its own code")

	for name, q := range map[string]registry.FXRateQuery{
		"same":    fxQ("EUR", "EUR"),
		"short":   fxQ("EU", "USD"),
		"no type": {Base: "EUR", Quote: "USD", MarketDate: time.Now()},
		"no date": {Base: "EUR", Quote: "USD", Type: domain.FXSpot},
	} {
		_, err := svc.GetFXRate(tenantCtx("tenant-001"), q)
		assert.ErrorIs(t, err, registry.ErrInvalidInput, name)
	}
}

func TestFX_HistoricalRateNeedsAKnownAtAndPassesItThrough(t *testing.T) {
	svc, f := fxSvc(t, permitAllAuthZ{}, goodCurrencies())
	_, err := svc.GetFXHistoricalRate(tenantCtx("tenant-001"), fxQ("EUR", "USD"))
	require.ErrorIs(t, err, registry.ErrInvalidInput)
	q := fxQ("EUR", "USD")
	q.KnownAt = time.Date(2026, 6, 13, 8, 0, 0, 0, time.UTC)
	_, err = svc.GetFXHistoricalRate(tenantCtx("tenant-001"), q)
	require.NoError(t, err)
	assert.Equal(t, q.KnownAt, f.lastKnown)
}

func TestFX_ExplainDerivedCrossRate(t *testing.T) {
	svc, _ := fxSvc(t, permitAllAuthZ{}, goodCurrencies())
	explain := func(q registry.FXRateQuery) (*domain.FXRate, error) {
		return svc.ExplainDerivedCrossRate(tenantCtx("tenant-001"), q)
	}

	d, err := explain(fxQ("EUR", "USD"))
	require.NoError(t, err)
	assert.Equal(t, domain.FXDirect, d.Method)
	assert.Equal(t, "1.1", d.Rate)
	assert.Empty(t, d.Exact, "a direct rate is as approved")

	inv, err := explain(fxQ("USD", "EUR"))
	require.NoError(t, err)
	assert.Equal(t, domain.FXInverse, inv.Method)
	assert.Equal(t, "0.909090909090909091", inv.Rate)
	assert.Equal(t, "10/11", inv.Exact, "the exact fraction is returned beside the rounded rate")
	assert.Equal(t, domain.FXRounding, inv.Rounding)
	assert.Equal(t, "USD/EUR = 1 / (EUR/USD)", inv.Formula)
	require.Len(t, inv.Components, 1)
	assert.Equal(t, "INVERTED", inv.Components[0].Direction)

	cross, err := explain(fxQ("EUR", "GBP"))
	require.NoError(t, err)
	assert.Equal(t, domain.FXCross, cross.Method)
	assert.Equal(t, "0.88", cross.Rate, "1.1 x 0.8 exactly, not 0.8800000000000001")
	assert.Equal(t, "22/25", cross.Exact)
	assert.Equal(t, "EUR/GBP = EUR/USD x USD/GBP", cross.Formula)
	require.Len(t, cross.Components, 2)
	assert.Equal(t, "obs-EURUSD", cross.Components[0].ObservationID, "traceable to the observations behind it")
	assert.Equal(t, "obs-USDGBP", cross.Components[1].ObservationID)
	assert.Equal(t, domain.FXDirect, cross.Components[0].Direction)

	// Both legs inverted.
	rev, err := explain(fxQ("GBP", "EUR"))
	require.NoError(t, err)
	assert.Equal(t, "25/22", rev.Exact)
	assert.Equal(t, "1.136363636363636364", rev.Rate)
	assert.Equal(t, "INVERTED", rev.Components[0].Direction)
	assert.Equal(t, "INVERTED", rev.Components[1].Direction)

	// A mixed pair.
	mixed, err := explain(fxQ("JPY", "GBP"))
	require.NoError(t, err)
	assert.Equal(t, "0.00536", mixed.Rate)
	assert.Equal(t, "67/12500", mixed.Exact)

	// Through a named pivot.
	q := fxQ("EUR", "JPY")
	q.Via = "USD"
	viaUSD, err := explain(q)
	require.NoError(t, err)
	want := new(big.Rat).Quo(big.NewRat(11, 10), mustRat("0.0067"))
	assert.Equal(t, domain.FormatRate(want), viaUSD.Rate)
	assert.Equal(t, domain.FractionString(want), viaUSD.Exact)

	// What it will not do: invent a rate. No pivot leg, a pivot that is one of the pair, a pair with no path.
	for name, q := range map[string]registry.FXRateQuery{
		"no path":           fxQ("EUR", "CHF"),
		"pivot is the base": fxQ("USD", "CHF"),
		"pivot missing":     func() registry.FXRateQuery { q := fxQ("EUR", "JPY"); q.Via = "GBP"; return q }(),
	} {
		_, err := explain(q)
		assert.ErrorIs(t, err, registry.ErrRateMissing, name)
	}
}

func mustRat(s string) *big.Rat {
	r, _ := new(big.Rat).SetString(s)
	return r
}

func TestFX_AStoreWithoutRatesIsUnavailable(t *testing.T) {
	bare := newSvc(t, newMemStore(), permitAllAuthZ{}, acceptAllJurisd{})
	_, err := bare.GetFXRate(tenantCtx("tenant-001"), fxQ("EUR", "USD"))
	require.ErrorIs(t, err, registry.ErrServiceUnavailable)
	_, err = bare.ListFXSources(tenantCtx("tenant-001"))
	require.ErrorIs(t, err, registry.ErrServiceUnavailable)
	_, err = bare.GetFXRateSet(tenantCtx("tenant-001"), "x")
	require.ErrorIs(t, err, registry.ErrServiceUnavailable)
}
