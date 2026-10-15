package arctl

import (
	"context"
	"fmt"
	"sort"
	"testing"

	"github.com/stretchr/testify/require"

	"zoiko.io/accounts-receivable-svc/internal/domain"
	"zoiko.io/accounts-receivable-svc/internal/kit"
)

// fakePart serves a fixed record set with the same keyset/limit semantics as a real provider.
func fakePart(wm string, recs []domain.ControlRecord) PopulationFunc {
	sorted := append([]domain.ControlRecord(nil), recs...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].RecordID < sorted[j].RecordID })
	return func(_ context.Context, q domain.ControlPopulationQuery) (*domain.ControlPopulationPage, error) {
		p := &domain.ControlPopulationPage{Records: []domain.ControlRecord{}, Watermark: wm + ":" + fmt.Sprint(len(sorted)),
			DeclaredTotals: domain.DeclaredTotals{RowCount: int64(len(sorted)), Totals: map[string]string{}}}
		var sum kit.Amount
		for _, r := range sorted {
			sum += kit.MustAmount(r.Amount)
		}
		if len(sorted) > 0 {
			p.DeclaredTotals.Totals["USD"] = sum.String()
		}
		for _, r := range sorted {
			if q.AfterRecordID != "" && r.RecordID <= q.AfterRecordID {
				continue
			}
			p.Records = append(p.Records, r)
		}
		if len(p.Records) > q.Limit {
			p.Records = p.Records[:q.Limit]
			p.NextCursor = p.Records[q.Limit-1].RecordID
		}
		return p, nil
	}
}

func recs(prefix string, n int, amount string) []domain.ControlRecord {
	out := make([]domain.ControlRecord, n)
	for i := range out {
		out[i] = domain.ControlRecord{RecordID: fmt.Sprintf("%s%07d", prefix, i), Reference: "R", Amount: amount, Currency: "USD", Date: "2026-09-01", Attributes: map[string]string{}}
	}
	return out
}

func drain(t *testing.T, fn PopulationFunc, limit int) (all []domain.ControlRecord, pages int, wms map[string]bool, last *domain.ControlPopulationPage) {
	t.Helper()
	wms = map[string]bool{}
	after := ""
	for {
		pg, err := fn(context.Background(), domain.ControlPopulationQuery{TenantID: "t", LegalEntityID: "e", Limit: limit, AfterRecordID: after})
		require.NoError(t, err)
		pages++
		wms[pg.Watermark] = true
		all = append(all, pg.Records...)
		last = pg
		if pg.NextCursor == "" {
			return
		}
		require.NotEqual(t, after, pg.NextCursor, "cursor must advance")
		after = pg.NextCursor
		require.Less(t, pages, 1000)
	}
}

func TestUnion_PagingWatermarkAndTotals(t *testing.T) {
	// interleaved ids across the two parts
	legacy := recs("00000000-0000-4000-8000-1", 7, "10.00")
	newer := recs("00000000-0000-4000-8000-0", 5, "2.5000")
	u := Union(fakePart("L", legacy), fakePart("N", newer))
	for _, limit := range []int{1, 2, 5, 12, 100} {
		all, _, wms, last := drain(t, u, limit)
		require.Len(t, all, 12, "limit %d", limit)
		require.True(t, sort.SliceIsSorted(all, func(i, j int) bool { return all[i].RecordID < all[j].RecordID }))
		seen := map[string]bool{}
		for _, r := range all {
			require.False(t, seen[r.RecordID], "no record repeated across pages")
			seen[r.RecordID] = true
		}
		require.Len(t, wms, 1, "watermark is the same on every page of an unchanged population")
		require.Equal(t, int64(12), last.DeclaredTotals.RowCount)
		require.Equal(t, "82.5000", last.DeclaredTotals.Totals["USD"]) // 7*10 + 5*2.5
		var sum kit.Amount
		for _, r := range all {
			sum += kit.MustAmount(r.Amount)
		}
		require.Equal(t, last.DeclaredTotals.Totals["USD"], sum.String(), "declared totals equal the sum of the records")
	}
	// the watermark changes when either part changes
	u2 := Union(fakePart("L", legacy), fakePart("N2", newer))
	_, _, w1, _ := drain(t, u, 100)
	_, _, w2, _ := drain(t, u2, 100)
	require.NotEqual(t, w1, w2)
}

// Legacy-only tenants see the legacy page byte for byte (watermark, formatting).
func TestUnion_LegacyOnlyIsVerbatim(t *testing.T) {
	legacy := fakePart("L", recs("00000000-0000-4000-8000-1", 3, "10.00"))
	u := Union(legacy, fakePart("N", nil))
	want, err := legacy(context.Background(), domain.ControlPopulationQuery{Limit: 10})
	require.NoError(t, err)
	got, err := u(context.Background(), domain.ControlPopulationQuery{Limit: 10})
	require.NoError(t, err)
	require.Equal(t, want, got)
	require.Equal(t, "30.0000", got.DeclaredTotals.Totals["USD"])
}

func TestUnion_CapAndErrors(t *testing.T) {
	big := func(n int64) PopulationFunc {
		return func(context.Context, domain.ControlPopulationQuery) (*domain.ControlPopulationPage, error) {
			return &domain.ControlPopulationPage{Records: []domain.ControlRecord{}, Watermark: "w", DeclaredTotals: domain.DeclaredTotals{RowCount: n, Totals: map[string]string{}}}, nil
		}
	}
	_, err := Union(big(150000), big(50001))(context.Background(), domain.ControlPopulationQuery{Limit: 1})
	require.ErrorIs(t, err, domain.ErrPopulationTooLarge)
	_, err = Union(big(150000), big(50000))(context.Background(), domain.ControlPopulationQuery{Limit: 1})
	require.NoError(t, err, "exactly at the cap is allowed")
	boom := func(context.Context, domain.ControlPopulationQuery) (*domain.ControlPopulationPage, error) {
		return nil, fmt.Errorf("db down")
	}
	_, err = Union(big(1), boom)(context.Background(), domain.ControlPopulationQuery{Limit: 1})
	require.Error(t, err)
}

func TestPopulationSubledger(t *testing.T) {
	p := PopulationSubledger{Provider: fakePart("L", recs("00000000-0000-4000-8000-1", 3, "10.00"))}
	sl, err := p.OpenReceivable(context.Background(), "t", "e", CutoffOf(tNow))
	require.NoError(t, err)
	require.True(t, sl.Available)
	require.Equal(t, "30.0000", sl.Totals["USD"].String())
	require.Equal(t, int64(3), sl.Count)
}
