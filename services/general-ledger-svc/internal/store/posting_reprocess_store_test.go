package store_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/general-ledger-svc/internal/domain"
	svcmiddleware "zoiko.io/general-ledger-svc/internal/middleware"
	"zoiko.io/general-ledger-svc/internal/store"
)

func newFailedExecution(t *testing.T, ctx context.Context, s *store.PgStore, tenantID string, payload []byte) *domain.PostingExecution {
	t.Helper()
	src := "src-" + uuid.NewString()
	e := &domain.PostingExecution{
		ExecutionID: uuid.NewString(), TenantID: tenantID, LegalEntityID: uuid.NewString(),
		Kind: domain.PostingExecutionKindEvent, SourceEventID: &src,
		Status: domain.PostingExecutionStatusSubmitted, CalculationTrace: "{}",
		CorrelationID: uuid.NewString(), CreatedAt: time.Now().UTC(), CreatedByPrincipalID: "svc-ar",
		RequestPayload: payload,
	}
	require.NoError(t, s.CreatePostingExecution(ctx, e))
	require.NoError(t, s.MarkPostingExecutionFailed(ctx, tenantID, e.ExecutionID, domain.PostingExecutionStatusFailed, "journal write failed"))
	return e
}

func TestPostingExecution_RequestPayloadRoundTrips(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop(), store.WithEventRegion("uk"))
	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	withPayload := newFailedExecution(t, ctx, s, tenantID, []byte(`{"source_event_id":"x","lines":[]}`))
	got, err := s.GetPostingExecution(ctx, tenantID, withPayload.ExecutionID)
	require.NoError(t, err)
	assert.JSONEq(t, `{"source_event_id":"x","lines":[]}`, string(got.RequestPayload))

	without := newFailedExecution(t, ctx, s, tenantID, nil)
	got, err = s.GetPostingExecution(ctx, tenantID, without.ExecutionID)
	require.NoError(t, err)
	assert.Nil(t, got.RequestPayload, "no capture must read back as nothing to replay, not an empty document")
}

// The claim is the only thing standing between two concurrent reprocesses
// and a fact posted twice: under contention exactly one may win.
func TestClaimPostingExecutionForReprocess_ExactlyOneWinnerUnderContention(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop(), store.WithEventRegion("uk"))
	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	e := newFailedExecution(t, ctx, s, tenantID, []byte(`{}`))

	const contenders = 8
	var wins int
	var mu sync.Mutex
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < contenders; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			ok, err := s.ClaimPostingExecutionForReprocess(ctx, tenantID, e.ExecutionID, time.Now().UTC())
			assert.NoError(t, err)
			if ok {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}()
	}
	close(start)
	wg.Wait()
	assert.Equal(t, 1, wins, "exactly one concurrent reprocess may claim the execution")

	got, err := s.GetPostingExecution(ctx, tenantID, e.ExecutionID)
	require.NoError(t, err)
	assert.Equal(t, domain.PostingExecutionStatusValidating, got.Status)
}

func TestClaimPostingExecutionForReprocess_StaleClaimIsTakenOver(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop(), store.WithEventRegion("uk"))
	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	e := newFailedExecution(t, ctx, s, tenantID, []byte(`{}`))

	claimTime := time.Now().UTC()
	ok, err := s.ClaimPostingExecutionForReprocess(ctx, tenantID, e.ExecutionID, claimTime)
	require.NoError(t, err)
	require.True(t, ok)

	ok, err = s.ClaimPostingExecutionForReprocess(ctx, tenantID, e.ExecutionID, claimTime.Add(time.Minute))
	require.NoError(t, err)
	assert.False(t, ok, "a live claim must not be taken over")

	ok, err = s.ClaimPostingExecutionForReprocess(ctx, tenantID, e.ExecutionID, claimTime.Add(store.ReprocessClaimTimeout+time.Minute))
	require.NoError(t, err)
	assert.True(t, ok, "a claim older than the timeout was abandoned and must be taken over")
}

func TestClaimPostingExecutionForReprocess_CommittedIsNeverClaimed(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop(), store.WithEventRegion("uk"))
	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	e := newFailedExecution(t, ctx, s, tenantID, []byte(`{}`))
	require.NoError(t, s.MarkPostingExecutionCommitted(ctx, tenantID, e.ExecutionID, uuid.NewString(), time.Now().UTC()))

	ok, err := s.ClaimPostingExecutionForReprocess(ctx, tenantID, e.ExecutionID, time.Now().UTC())
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestFindJournalIDsBySourceEvent(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop(), store.WithEventRegion("uk"))
	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	src := "src-" + uuid.NewString()
	d := domain.NewDate(2026, time.October, 28)
	h := &domain.JournalHeader{
		JournalID: uuid.NewString(), TenantID: tenantID, LegalEntityID: uuid.NewString(), FiscalPeriod: "2026-10",
		Status: domain.JournalStatusPending, CreatedByPrincipalID: "svc-ar", CorrelationID: uuid.NewString(),
		SourceEventID: &src, JournalType: domain.JournalTypeStandard, TransactionDate: d, PostingDate: d,
		CurrencyCode: "GBP", ApprovalStatus: domain.ApprovalStatusPostingRequested,
	}
	_, _, err := s.CreateJournal(ctx, h, []domain.JournalLine{
		{AccountCode: "1100", DebitAmount: 5000}, {AccountCode: "4000", CreditAmount: 5000},
	})
	require.NoError(t, err)

	ids, err := s.FindJournalIDsBySourceEvent(ctx, tenantID, src)
	require.NoError(t, err)
	assert.Equal(t, []string{h.JournalID}, ids)

	ids, err = s.FindJournalIDsBySourceEvent(ctx, tenantID, "src-never-posted")
	require.NoError(t, err)
	assert.Empty(t, ids)

	other := uuid.New().String()
	ids, err = s.FindJournalIDsBySourceEvent(svcmiddleware.WithTenant(context.Background(), other), other, src)
	require.NoError(t, err)
	assert.Empty(t, ids, "another tenant must never see this tenant's journals")
}
