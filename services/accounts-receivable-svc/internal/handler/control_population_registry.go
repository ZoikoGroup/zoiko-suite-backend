package handler

import (
	"context"
	"sync"

	"zoiko.io/accounts-receivable-svc/internal/domain"
)

// PopulationFunc serves one keyset page of a control population (docs/architecture/control-population-contract.md):
// the whole-set watermark and declared totals come from the same snapshot as the page, and a population over
// domain.MaxControlPopulationRecords answers domain.ErrPopulationTooLarge.
type PopulationFunc func(ctx context.Context, q domain.ControlPopulationQuery) (*domain.ControlPopulationPage, error)

var (
	populationMu        sync.RWMutex
	populationProviders = map[string]PopulationFunc{}
)

// RegisterPopulation makes GET /v1/control-populations/{name} dispatch to fn. Registering `open-invoices` replaces the
// legacy-only store behaviour (main.go registers the legacy + AR-04 union, which keeps the legacy rows byte-identical).
// It is called once at wiring time, before the server listens. A nil fn removes the registration.
func RegisterPopulation(name string, fn PopulationFunc) {
	populationMu.Lock()
	defer populationMu.Unlock()
	if fn == nil {
		delete(populationProviders, name)
		return
	}
	populationProviders[name] = fn
}

func populationProvider(name string) (PopulationFunc, bool) {
	populationMu.RLock()
	defer populationMu.RUnlock()
	fn, ok := populationProviders[name]
	return fn, ok
}
