//go:build integration

package store_test

import (
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/payment-initiation-adapter-svc/internal/domain"
)

// FIN-CTRL-004 population against a real Postgres, through the UNPRIVILEGED role so row-level security is in force.

func prepareAttempt(t *testing.T, tenant, entity, payRef string, amount float64, ccy string, exec time.Time) *domain.PaymentInitiationAttempt {
	t.Helper()
	key := "k-" + uuid.NewString()
	a, err := app(t).PrepareAttempt(tctx(tenant), tenant, domain.PrepareAttemptRequest{
		LegalEntityID: entity, SourceReference: "src-" + key, PayerAccountRef: "DE00AA", PayeeRef: "FR00BB", Amount: amount,
		Currency: ccy, ExecutionDate: exec, PaymentReference: payRef, PayerAccountVerified: true, IdempotencyKey: key,
	}, "maker", "corr-"+key)
	require.NoError(t, err)
	return a
}

func TestControlPopulation_PaymentAttempts_SignedCashOutEveryStatusScopedAndPaged(t *testing.T) {
	st := app(t)
	tenant, entity := uuid.NewString(), uuid.NewString()
	day := func(m time.Month, d int) time.Time { return time.Date(2026, m, d, 12, 0, 0, 0, time.UTC) }

	a1 := prepareAttempt(t, tenant, entity, "PAY-1", 10.50, "EUR", day(9, 10))
	a2 := prepareAttempt(t, tenant, entity, "", 20, "EUR", day(9, 20)) // no payment reference
	a3 := prepareAttempt(t, tenant, entity, "PAY-3", 5, "USD", day(10, 5))
	// a cancelled attempt moved no money but is still reported: the control excludes it by an explicit rule
	a4 := prepareAttempt(t, tenant, entity, "PAY-4", 1, "EUR", day(9, 11))
	_, err := st.CancelAttempt(tctx(tenant), a4.AttemptID, "maker")
	require.NoError(t, err)

	q := func(tenantID, entityID string, cut *time.Time, limit int, after string) *domain.ControlPopulationPage {
		t.Helper()
		p, err := st.QueryPaymentAttempts(tctx(tenantID), tenantID, domain.PaymentAttemptsQuery{LegalEntityID: entityID, CutOff: cut, Limit: limit, AfterRecordID: after})
		require.NoError(t, err)
		return p
	}

	p := q(tenant, entity, nil, 100, "")
	require.Len(t, p.Records, 4, "every status is reported")
	byID := map[string]domain.ControlPopulationRecord{}
	for _, r := range p.Records {
		byID[r.RecordID] = r
	}
	assert.Equal(t, "-10.50", byID[a1.AttemptID].Amount, "a payment is money OUT: negative, like a bank debit")
	assert.Equal(t, "PAY-1", byID[a1.AttemptID].Reference)
	assert.Equal(t, "2026-09-10", byID[a1.AttemptID].Date)
	assert.Equal(t, "EUR", byID[a1.AttemptID].Currency)
	assert.Equal(t, "attempt:"+a2.AttemptID, byID[a2.AttemptID].Reference, "a blank reference can never pair with a bank line")
	assert.Equal(t, "CANCELLED", byID[a4.AttemptID].Attributes["status"])
	assert.Equal(t, "PREPARED", byID[a3.AttemptID].Attributes["status"])
	assert.Equal(t, "-31.50", p.DeclaredTotals.Totals["EUR"], "10.50 + 20 + 1")
	assert.Equal(t, "-5.00", p.DeclaredTotals.Totals["USD"])
	assert.EqualValues(t, 4, p.DeclaredTotals.RowCount)
	assert.True(t, strings.HasPrefix(p.Watermark, "pa1:n=4;md5="), p.Watermark)

	// strict privacy: neither account reference leaves the service
	for _, r := range p.Records {
		for k, v := range r.Attributes {
			assert.NotContains(t, v, "DE00AA", k)
			assert.NotContains(t, v, "FR00BB", k)
		}
	}

	// the cut-off is "on or before the end of that day"
	sep := time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)
	assert.Len(t, q(tenant, entity, &sep, 100, "").Records, 3, "the October attempt is after the cut-off")
	tenth := time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC)
	cut := q(tenant, entity, &tenth, 100, "")
	require.Len(t, cut.Records, 1, "execution date 2026-09-10 is on the cut-off")
	assert.Equal(t, a1.AttemptID, cut.Records[0].RecordID)

	// tenant and entity isolation (the unprivileged role: RLS plus the explicit predicate)
	assert.Empty(t, q(uuid.NewString(), entity, nil, 100, "").Records)
	assert.Empty(t, q(tenant, uuid.NewString(), nil, 100, "").Records)

	// keyset paging is complete, ordered, repeat-free and under one watermark
	for _, limit := range []int{1, 3, 100} {
		var ids []string
		marks := map[string]bool{}
		after := ""
		for guard := 0; ; guard++ {
			require.Less(t, guard, 10, "paging did not terminate")
			pg := q(tenant, entity, nil, limit, after)
			marks[pg.Watermark] = true
			for _, r := range pg.Records {
				ids = append(ids, r.RecordID)
			}
			if pg.NextRecordID == "" {
				break
			}
			after = pg.NextRecordID
		}
		assert.Len(t, ids, 4, "limit=%d", limit)
		assert.Len(t, marks, 1, "limit=%d: one watermark", limit)
		assert.True(t, sort.StringsAreSorted(ids), "limit=%d", limit)
	}

	// the watermark moves when the population changes
	before := p.Watermark
	prepareAttempt(t, tenant, entity, "PAY-5", 2, "EUR", day(9, 12))
	assert.NotEqual(t, before, q(tenant, entity, nil, 100, "").Watermark)
}
