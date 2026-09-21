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

func seedCashPositionAccount(t *testing.T, ctx context.Context, tenantID, legalEntityID string, balance float64) {
	t.Helper()
	s := testStore
	acct := newTestAccount(tenantID, legalEntityID)
	if _, err := s.CreateBankAccount(ctx, acct); err != nil {
		t.Fatalf("create account: %v", err)
	}
	if err := s.CreateCashBalance(ctx, &domain.CashBalance{
		BalanceID: uuid.New().String(), TenantID: tenantID, BankAccountID: acct.BankAccountID,
		LedgerBalance: balance, AvailableBalance: balance, AsOfTimestamp: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("create cash balance: %v", err)
	}
}

// TestPgStore_CashPositionSnapshot_CalculateThenPublish is the real
// end-to-end proof of Wave 14's Calculated->Published lifecycle: a
// snapshot is created CALCULATED, and only an explicit publish makes it
// PUBLISHED — nothing else does.
func TestPgStore_CashPositionSnapshot_CalculateThenPublish(t *testing.T) {
	cleanTables(t)
	s := testStore
	tenantID := uuid.New().String()
	legalEntityID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	snap, err := s.CreateCashPositionSnapshot(ctx, domain.CalculateCashPositionParams{
		TenantID: tenantID, LegalEntityID: legalEntityID, ReportingCurrency: "USD",
		RestrictedAmount: 1000, ActorPrincipalID: "analyst-1", CorrelationID: "corr-calc-1",
	}, domain.CashPositionCalculation{
		BankBalance: 10000, PendingAPCommitments: 2000, PayrollObligations: 500, TaxLiabilities: 300,
		AvailableCash: 10000 - 1000 - 2000 - 500 - 300, AsOfTimestamp: time.Now().UTC(),
		AccountBreakdown: []domain.CashPositionAccountLine{{BankAccountID: "acct-1", AccountName: "Main", Balance: 10000, AsOfTimestamp: time.Now().UTC()}},
	})
	if err != nil {
		t.Fatalf("create snapshot: %v", err)
	}
	if snap.Status != domain.CashPositionCalculated || snap.EffectiveStatus != domain.CashPositionCalculated {
		t.Fatalf("expected CALCULATED, got status=%s effective=%s", snap.Status, snap.EffectiveStatus)
	}
	if snap.AvailableCash != 6200 {
		t.Fatalf("expected available_cash=6200 (10000-1000-2000-500-300), got %v", snap.AvailableCash)
	}
	if len(snap.AccountBreakdown) != 1 || snap.AccountBreakdown[0].BankAccountID != "acct-1" {
		t.Fatalf("expected the account breakdown to round-trip, got %+v", snap.AccountBreakdown)
	}

	published, err := s.PublishCashPositionSnapshot(ctx, domain.PublishCashPositionParams{TenantID: tenantID, SnapshotID: snap.SnapshotID, ActorPrincipalID: "approver-1"})
	if err != nil || published.Status != domain.CashPositionPublished {
		t.Fatalf("publish: %+v err=%v", published, err)
	}
	if published.PublishedByPrincipalID != "approver-1" || published.PublishedAt == nil {
		t.Fatalf("expected published_by/published_at to be recorded, got %+v", published)
	}

	// Negative control: publishing again must fail — not CALCULATED anymore.
	if _, err := s.PublishCashPositionSnapshot(ctx, domain.PublishCashPositionParams{TenantID: tenantID, SnapshotID: snap.SnapshotID, ActorPrincipalID: "approver-2"}); err != domain.ErrInvalidCashPositionTransition {
		t.Fatalf("expected ErrInvalidCashPositionTransition re-publishing, got %v", err)
	}
}

// TestPgStore_CashPositionSnapshot_StaleCannotPublish is the real proof
// of the doc's own rule: a snapshot with a stale bank-balance component
// cannot be published as current.
func TestPgStore_CashPositionSnapshot_StaleCannotPublish(t *testing.T) {
	cleanTables(t)
	s := testStore
	tenantID := uuid.New().String()
	legalEntityID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	snap, err := s.CreateCashPositionSnapshot(ctx, domain.CalculateCashPositionParams{
		TenantID: tenantID, LegalEntityID: legalEntityID, ReportingCurrency: "USD", ActorPrincipalID: "analyst-1",
	}, domain.CashPositionCalculation{
		BankBalance: 5000, AvailableCash: 5000, AsOfTimestamp: time.Now().UTC(), HasStaleComponent: true,
	})
	if err != nil {
		t.Fatalf("create snapshot: %v", err)
	}

	if _, err := s.PublishCashPositionSnapshot(ctx, domain.PublishCashPositionParams{TenantID: tenantID, SnapshotID: snap.SnapshotID, ActorPrincipalID: "approver-1"}); err != domain.ErrCashPositionStaleCannotPublish {
		t.Fatalf("expected ErrCashPositionStaleCannotPublish, got %v", err)
	}

	// The stored status is still CALCULATED, but EffectiveStatus only
	// becomes "STALE" once PUBLISHED — a calculated-but-stale snapshot
	// reports its real status honestly, not a status it never reached.
	fetched, err := s.GetCashPositionSnapshot(ctx, tenantID, snap.SnapshotID)
	if err != nil {
		t.Fatalf("get snapshot: %v", err)
	}
	if fetched.EffectiveStatus != domain.CashPositionCalculated {
		t.Fatalf("expected EffectiveStatus=CALCULATED for a never-published stale snapshot, got %s", fetched.EffectiveStatus)
	}
}

// TestPgStore_CashPositionSnapshot_RefreshSupersedesPrior proves the
// refresh/supersede chain: a second calculation superseding the first
// leaves the first row intact but marked SUPERSEDED, pointing at the new
// one — the same append-only/superseded-by shape as every other history
// table in this service.
func TestPgStore_CashPositionSnapshot_RefreshSupersedesPrior(t *testing.T) {
	cleanTables(t)
	s := testStore
	tenantID := uuid.New().String()
	legalEntityID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	first, err := s.CreateCashPositionSnapshot(ctx, domain.CalculateCashPositionParams{
		TenantID: tenantID, LegalEntityID: legalEntityID, ReportingCurrency: "USD", ActorPrincipalID: "analyst-1",
	}, domain.CashPositionCalculation{BankBalance: 1000, AvailableCash: 1000, AsOfTimestamp: time.Now().UTC()})
	if err != nil {
		t.Fatalf("create first: %v", err)
	}
	if _, err := s.PublishCashPositionSnapshot(ctx, domain.PublishCashPositionParams{TenantID: tenantID, SnapshotID: first.SnapshotID, ActorPrincipalID: "approver-1"}); err != nil {
		t.Fatalf("publish first: %v", err)
	}

	second, err := s.CreateCashPositionSnapshot(ctx, domain.CalculateCashPositionParams{
		TenantID: tenantID, LegalEntityID: legalEntityID, ReportingCurrency: "USD", ActorPrincipalID: "analyst-1",
	}, domain.CashPositionCalculation{BankBalance: 1500, AvailableCash: 1500, AsOfTimestamp: time.Now().UTC()})
	if err != nil {
		t.Fatalf("create second: %v", err)
	}

	superseded, err := s.SupersedeCashPositionSnapshot(ctx, domain.SupersedeCashPositionParams{TenantID: tenantID, SnapshotID: first.SnapshotID, NewSnapshotID: second.SnapshotID})
	if err != nil || superseded.Status != domain.CashPositionSuperseded {
		t.Fatalf("supersede: %+v err=%v", superseded, err)
	}
	if superseded.SupersededBy == nil || *superseded.SupersededBy != second.SnapshotID {
		t.Fatalf("expected superseded_by to point at the new snapshot, got %+v", superseded.SupersededBy)
	}

	latest, err := s.GetLatestCashPosition(ctx, tenantID, legalEntityID, "USD")
	if err != nil || latest == nil || latest.SnapshotID != second.SnapshotID {
		t.Fatalf("expected the latest cash position to be the second snapshot, got %+v err=%v", latest, err)
	}
}

// TestPgStore_CashPositionSnapshot_SupersededIsImmutable is the
// negative-controlled proof of migration 000008's own
// reject_cash_position_mutation trigger.
func TestPgStore_CashPositionSnapshot_SupersededIsImmutable(t *testing.T) {
	cleanTables(t)
	s := testStore
	tenantID := uuid.New().String()
	legalEntityID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	first, err := s.CreateCashPositionSnapshot(ctx, domain.CalculateCashPositionParams{
		TenantID: tenantID, LegalEntityID: legalEntityID, ReportingCurrency: "USD", ActorPrincipalID: "analyst-1",
	}, domain.CashPositionCalculation{BankBalance: 1000, AvailableCash: 1000, AsOfTimestamp: time.Now().UTC()})
	if err != nil {
		t.Fatalf("create first: %v", err)
	}
	second, err := s.CreateCashPositionSnapshot(ctx, domain.CalculateCashPositionParams{
		TenantID: tenantID, LegalEntityID: legalEntityID, ReportingCurrency: "USD", ActorPrincipalID: "analyst-1",
	}, domain.CashPositionCalculation{BankBalance: 2000, AvailableCash: 2000, AsOfTimestamp: time.Now().UTC()})
	if err != nil {
		t.Fatalf("create second: %v", err)
	}
	if _, err := s.SupersedeCashPositionSnapshot(ctx, domain.SupersedeCashPositionParams{TenantID: tenantID, SnapshotID: first.SnapshotID, NewSnapshotID: second.SnapshotID}); err != nil {
		t.Fatalf("supersede: %v", err)
	}

	if _, err := testPool.Exec(ctx, `UPDATE cash_position_snapshots SET bank_balance = 999999 WHERE snapshot_id = $1`, first.SnapshotID); err == nil {
		t.Fatal("expected the trigger to refuse mutating a SUPERSEDED snapshot")
	}

	if _, err := testPool.Exec(ctx, `ALTER TABLE cash_position_snapshots DISABLE TRIGGER trg_reject_cash_position_mutation`); err != nil {
		t.Fatalf("disable trigger: %v", err)
	}
	if _, err := testPool.Exec(ctx, `UPDATE cash_position_snapshots SET bank_balance = 999999 WHERE snapshot_id = $1`, first.SnapshotID); err != nil {
		t.Fatalf("expected the UPDATE to succeed with the trigger disabled, proving it was the real mechanism: %v", err)
	}
	if _, err := testPool.Exec(ctx, `ALTER TABLE cash_position_snapshots ENABLE TRIGGER trg_reject_cash_position_mutation`); err != nil {
		t.Fatalf("re-enable trigger: %v", err)
	}
	if _, err := testPool.Exec(ctx, `UPDATE cash_position_snapshots SET bank_balance = 1 WHERE snapshot_id = $1`, first.SnapshotID); err == nil {
		t.Fatal("expected re-enabling the trigger to restore the refusal")
	}
}

// TestPgStore_CashPositionSnapshot_AsOfReconstructsHistory mirrors
// BNK-01's GetBankAccountAsOf proof: a timestamp between two
// calculations returns the FIRST snapshot, not the current (second) one.
func TestPgStore_CashPositionSnapshot_AsOfReconstructsHistory(t *testing.T) {
	cleanTables(t)
	s := testStore
	tenantID := uuid.New().String()
	legalEntityID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	first, err := s.CreateCashPositionSnapshot(ctx, domain.CalculateCashPositionParams{
		TenantID: tenantID, LegalEntityID: legalEntityID, ReportingCurrency: "USD", ActorPrincipalID: "analyst-1",
	}, domain.CashPositionCalculation{BankBalance: 1000, AvailableCash: 1000, AsOfTimestamp: time.Now().UTC()})
	if err != nil {
		t.Fatalf("create first: %v", err)
	}
	var midpoint time.Time
	if err := testPool.QueryRow(ctx, `SELECT created_at FROM cash_position_snapshots WHERE snapshot_id = $1`, first.SnapshotID).Scan(&midpoint); err != nil {
		t.Fatalf("read midpoint: %v", err)
	}
	time.Sleep(10 * time.Millisecond)

	if _, err := s.CreateCashPositionSnapshot(ctx, domain.CalculateCashPositionParams{
		TenantID: tenantID, LegalEntityID: legalEntityID, ReportingCurrency: "USD", ActorPrincipalID: "analyst-1",
	}, domain.CashPositionCalculation{BankBalance: 2000, AvailableCash: 2000, AsOfTimestamp: time.Now().UTC()}); err != nil {
		t.Fatalf("create second: %v", err)
	}

	asOf, err := s.GetCashPositionAsOf(ctx, tenantID, legalEntityID, "USD", midpoint.Add(1*time.Millisecond))
	if err != nil {
		t.Fatalf("GetCashPositionAsOf: %v", err)
	}
	if asOf == nil || asOf.BankBalance != 1000 {
		t.Fatalf("expected the FIRST snapshot's bank_balance=1000 at the midpoint, got %+v", asOf)
	}

	current, err := s.GetCashPositionAsOf(ctx, tenantID, legalEntityID, "USD", time.Now().UTC())
	if err != nil || current == nil || current.BankBalance != 2000 {
		t.Fatalf("expected the SECOND (latest) snapshot's bank_balance=2000 as of now, got %+v err=%v", current, err)
	}
}

// TestPgStore_ListCurrencyBreakdown_ReturnsLatestPerCurrency proves the
// GetCurrencyBreakdown query's real behavior: one row per currency, the
// latest calculated for each.
func TestPgStore_ListCurrencyBreakdown_ReturnsLatestPerCurrency(t *testing.T) {
	cleanTables(t)
	s := testStore
	tenantID := uuid.New().String()
	legalEntityID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	if _, err := s.CreateCashPositionSnapshot(ctx, domain.CalculateCashPositionParams{
		TenantID: tenantID, LegalEntityID: legalEntityID, ReportingCurrency: "USD", ActorPrincipalID: "analyst-1",
	}, domain.CashPositionCalculation{BankBalance: 1000, AvailableCash: 1000, AsOfTimestamp: time.Now().UTC()}); err != nil {
		t.Fatalf("create USD: %v", err)
	}
	if _, err := s.CreateCashPositionSnapshot(ctx, domain.CalculateCashPositionParams{
		TenantID: tenantID, LegalEntityID: legalEntityID, ReportingCurrency: "EUR", ActorPrincipalID: "analyst-1",
	}, domain.CashPositionCalculation{BankBalance: 500, AvailableCash: 500, AsOfTimestamp: time.Now().UTC()}); err != nil {
		t.Fatalf("create EUR: %v", err)
	}

	snaps, err := s.ListCurrencyBreakdown(ctx, tenantID, legalEntityID)
	if err != nil {
		t.Fatalf("ListCurrencyBreakdown: %v", err)
	}
	if len(snaps) != 2 {
		t.Fatalf("expected 2 currencies (USD, EUR), got %d: %+v", len(snaps), snaps)
	}
}
