package store

import (
	"context"
	"time"
)

// RateCounter is the shared per-minute counter behind the replica-local
// token buckets (Doc 06 §13). Both stores implement it.
type RateCounter interface {
	// IncrRate adds one hit to key's window and returns the window's total.
	IncrRate(ctx context.Context, key string, window time.Time) (int64, error)
	// PurgeRateCounters drops windows that started before cutoff.
	PurgeRateCounters(ctx context.Context, cutoff time.Time) error
}

type rateWindow struct {
	key    string
	window int64
}

func (m *Memory) IncrRate(_ context.Context, key string, window time.Time) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.rates == nil {
		m.rates = map[rateWindow]int64{}
	}
	k := rateWindow{key, window.UTC().UnixNano()}
	m.rates[k]++
	return m.rates[k], nil
}

func (m *Memory) PurgeRateCounters(_ context.Context, cutoff time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for k := range m.rates {
		if k.window < cutoff.UTC().UnixNano() {
			delete(m.rates, k)
		}
	}
	return nil
}

func (p *Postgres) IncrRate(ctx context.Context, key string, window time.Time) (int64, error) {
	var n int64
	err := p.pool.QueryRow(ctx, `INSERT INTO public_ai.rate_counters (counter_key, window_start, hits) VALUES ($1, $2, 1)
		ON CONFLICT (counter_key, window_start) DO UPDATE SET hits = public_ai.rate_counters.hits + 1
		RETURNING hits`, key, window.UTC()).Scan(&n)
	return n, err
}

func (p *Postgres) PurgeRateCounters(ctx context.Context, cutoff time.Time) error {
	_, err := p.pool.Exec(ctx, `DELETE FROM public_ai.rate_counters WHERE window_start < $1`, cutoff.UTC())
	return err
}
