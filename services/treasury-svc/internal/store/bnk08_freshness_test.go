//go:build integration

package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"zoiko.io/treasury-svc/internal/domain"
	svcmiddleware "zoiko.io/treasury-svc/internal/middleware"
)

func createCashPosition(t *testing.T, ctx context.Context, tenantID, legalEntityID string, balanceAsOf time.Time) *domain.CashPositionSnapshot {
	t.Helper()
	snap, err := testStore.CreateCashPositionSnapshot(ctx, domain.CalculateCashPositionParams{
		TenantID: tenantID, LegalEntityID: legalEntityID, ReportingCurrency: "USD", ActorPrincipalID: "analyst-1",
	}, domain.CashPositionCalculation{
		BankBalance: 100, AvailableCash: 100, AsOfTimestamp: time.Now().UTC(),
		AccountBreakdown: []domain.CashPositionAccountLine{{BankAccountID: "acct-1", Balance: 100, AsOfTimestamp: balanceAsOf}},
	})
	if err != nil {
		t.Fatalf("create snapshot: %v", err)
	}
	return snap
}

// Staleness is derived from the composed balances' age at the time of the
// read, not frozen at calculation. A snapshot whose balance is two days old
// now is stale and cannot be published as current. Before,
// has_stale_component was only ever the calculation's own verdict.
func TestPgStore_CashPosition_StalenessDerivedAtRead(t *testing.T) {
	cleanTables(t)
	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	snap := createCashPosition(t, ctx, tenantID, uuid.New().String(), time.Now().UTC().Add(-48*time.Hour))
	if !snap.HasStaleComponent {
		t.Fatal("a snapshot composed from a two-day-old balance reads as fresh")
	}
	if _, err := testStore.PublishCashPositionSnapshot(ctx, domain.PublishCashPositionParams{TenantID: tenantID, SnapshotID: snap.SnapshotID, ActorPrincipalID: "publisher-1"}); err != domain.ErrCashPositionStaleCannotPublish {
		t.Fatalf("publishing an aged snapshot: %v, want ErrCashPositionStaleCannotPublish", err)
	}
}

// A published snapshot becomes STALE once its balances age past the
// threshold ("never silently carry forward old bank balance as current").
// Before, a published snapshot stayed PUBLISHED however old it got: STALE
// was unreachable, because publish refuses a snapshot that was stale when
// calculated.
func TestPgStore_CashPosition_PublishedSnapshotAgesIntoStale(t *testing.T) {
	cleanTables(t)
	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	almostStale := time.Now().UTC().Add(-domain.BankBalanceStalenessThreshold + 2*time.Second)
	snap := createCashPosition(t, ctx, tenantID, uuid.New().String(), almostStale)
	published, err := testStore.PublishCashPositionSnapshot(ctx, domain.PublishCashPositionParams{TenantID: tenantID, SnapshotID: snap.SnapshotID, ActorPrincipalID: "publisher-1"})
	if err != nil || published.EffectiveStatus != domain.CashPositionPublished {
		t.Fatalf("publish while fresh: %+v err=%v", published, err)
	}

	time.Sleep(3 * time.Second)
	read, err := testStore.GetCashPositionSnapshot(ctx, tenantID, snap.SnapshotID)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if read.EffectiveStatus != "STALE" || !read.HasStaleComponent || read.Status != domain.CashPositionPublished {
		t.Fatalf("published snapshot past the threshold: status=%s effective=%s stale=%v, want PUBLISHED/STALE/true",
			read.Status, read.EffectiveStatus, read.HasStaleComponent)
	}
}

// Refresh writes the new snapshot and supersedes the prior one in one
// transaction, and the prior must be for the same legal entity and
// currency. Before, a refresh for entity B could retire entity A's
// snapshot, and a failed supersede left the new snapshot behind.
func TestPgStore_CashPosition_RefreshIsAtomicAndScoped(t *testing.T) {
	cleanTables(t)
	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	entityA, entityB := uuid.New().String(), uuid.New().String()
	priorA := createCashPosition(t, ctx, tenantID, entityA, time.Now().UTC())

	_, err := testStore.RefreshCashPositionSnapshot(ctx, domain.RefreshCashPositionParams{
		CalculateCashPositionParams: domain.CalculateCashPositionParams{TenantID: tenantID, LegalEntityID: entityB, ReportingCurrency: "USD", ActorPrincipalID: "analyst-1"},
		PriorSnapshotID:             priorA.SnapshotID,
	}, domain.CashPositionCalculation{AsOfTimestamp: time.Now().UTC()})
	if err != domain.ErrCashPositionScopeMismatch {
		t.Fatalf("refresh for entity B superseding entity A's snapshot: %v, want ErrCashPositionScopeMismatch", err)
	}
	if latestB, err := testStore.GetLatestCashPosition(ctx, tenantID, entityB, "USD"); err != nil || latestB != nil {
		t.Fatalf("the refused refresh left a snapshot behind for entity B: %+v err=%v", latestB, err)
	}
	if a, _ := testStore.GetCashPositionSnapshot(ctx, tenantID, priorA.SnapshotID); a.Status != domain.CashPositionCalculated {
		t.Fatalf("entity A's snapshot was changed to %s", a.Status)
	}

	refreshed, err := testStore.RefreshCashPositionSnapshot(ctx, domain.RefreshCashPositionParams{
		CalculateCashPositionParams: domain.CalculateCashPositionParams{TenantID: tenantID, LegalEntityID: entityA, ReportingCurrency: "USD", ActorPrincipalID: "analyst-1"},
		PriorSnapshotID:             priorA.SnapshotID,
	}, domain.CashPositionCalculation{AsOfTimestamp: time.Now().UTC()})
	if err != nil {
		t.Fatalf("refresh in scope: %v", err)
	}
	a, _ := testStore.GetCashPositionSnapshot(ctx, tenantID, priorA.SnapshotID)
	if a.Status != domain.CashPositionSuperseded || a.SupersededBy == nil || *a.SupersededBy != refreshed.SnapshotID {
		t.Fatalf("prior after refresh: status=%s superseded_by=%v", a.Status, a.SupersededBy)
	}
}

// The standalone supersede accepts only a different, live snapshot for the
// same legal entity and currency as the replacement.
func TestPgStore_SupersedeCashPosition_RejectsOutOfScopeReplacement(t *testing.T) {
	cleanTables(t)
	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	entityA := uuid.New().String()
	a1 := createCashPosition(t, ctx, tenantID, entityA, time.Now().UTC())
	other := createCashPosition(t, ctx, tenantID, uuid.New().String(), time.Now().UTC())

	for name, replacement := range map[string]string{"another entity's snapshot": other.SnapshotID, "itself": a1.SnapshotID} {
		if _, err := testStore.SupersedeCashPositionSnapshot(ctx, domain.SupersedeCashPositionParams{TenantID: tenantID, SnapshotID: a1.SnapshotID, NewSnapshotID: replacement}); err != domain.ErrCashPositionScopeMismatch {
			t.Errorf("superseded by %s: %v, want ErrCashPositionScopeMismatch", name, err)
		}
	}
	a2 := createCashPosition(t, ctx, tenantID, entityA, time.Now().UTC())
	if got, err := testStore.SupersedeCashPositionSnapshot(ctx, domain.SupersedeCashPositionParams{TenantID: tenantID, SnapshotID: a1.SnapshotID, NewSnapshotID: a2.SnapshotID}); err != nil || got.Status != domain.CashPositionSuperseded {
		t.Fatalf("supersede in scope: %+v err=%v", got, err)
	}
}

// GetLatestCashBalances reads the latest balance of every account in one
// query; an account with none is absent.
func TestPgStore_GetLatestCashBalances_LatestPerAccount(t *testing.T) {
	cleanTables(t)
	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	legalEntityID := uuid.New().String()
	withTwo, withOne, withNone := newTestAccount(tenantID, legalEntityID), newTestAccount(tenantID, legalEntityID), newTestAccount(tenantID, legalEntityID)
	for _, a := range []*domain.BankAccount{withTwo, withOne, withNone} {
		if _, err := testStore.CreateBankAccount(ctx, a); err != nil {
			t.Fatalf("create account: %v", err)
		}
	}
	now := time.Now().UTC()
	for _, b := range []struct {
		acct   string
		amount float64
		asOf   time.Time
	}{{withTwo.BankAccountID, 1, now.Add(-time.Hour)}, {withTwo.BankAccountID, 2, now}, {withOne.BankAccountID, 3, now}} {
		if err := testStore.CreateCashBalance(ctx, &domain.CashBalance{BalanceID: uuid.New().String(), TenantID: tenantID, BankAccountID: b.acct, LedgerBalance: b.amount, AvailableBalance: b.amount, AsOfTimestamp: b.asOf}); err != nil {
			t.Fatalf("create balance: %v", err)
		}
	}

	got, err := testStore.GetLatestCashBalances(ctx, []string{withTwo.BankAccountID, withOne.BankAccountID, withNone.BankAccountID})
	if err != nil {
		t.Fatalf("GetLatestCashBalances: %v", err)
	}
	if len(got) != 2 || got[withTwo.BankAccountID].AvailableBalance != 2 || got[withOne.BankAccountID].AvailableBalance != 3 || got[withNone.BankAccountID] != nil {
		t.Fatalf("latest balances: %+v", got)
	}
}
