//go:build integration

package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"go.uber.org/zap"

	"zoiko.io/general-ledger-svc/internal/domain"
	svcmiddleware "zoiko.io/general-ledger-svc/internal/middleware"
	"zoiko.io/general-ledger-svc/internal/store"
)

// newTestAccount builds a minimal valid account for the ACC-01 store tests.
func newTestAccount(tenantID, code, name string) *domain.Account {
	return &domain.Account{
		AccountID:            uuid.NewString(),
		TenantID:             tenantID,
		AccountCode:          code,
		AccountName:          name,
		AccountType:          domain.AccountTypeAsset,
		Status:               "ACTIVE",
		CreatedAt:            time.Now().UTC(),
		CreatedByPrincipalID: "test-admin",
	}
}

func TestPgStore_AmendAccountVersion_AppendsVersionAndAdvancesHead(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop(), store.WithEventRegion("uk"))
	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	acct := newTestAccount(tenantID, "1000", "Cash")
	if err := s.CreateAccount(ctx, acct); err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}

	head, err := s.GetAccountByCode(ctx, tenantID, "1000")
	if err != nil {
		t.Fatalf("GetAccountByCode: %v", err)
	}
	if head.Version != 1 {
		t.Fatalf("new account should be version 1, got %d", head.Version)
	}

	effective := time.Now().UTC().Add(time.Minute)
	updates := *head
	updates.AccountName = "Cash on hand"
	amended, err := s.AmendAccountVersion(ctx, tenantID, "1000", 1, effective, &updates, "renamed by finance", nil, "test-admin")
	if err != nil {
		t.Fatalf("AmendAccountVersion: %v", err)
	}
	if amended.Version != 2 || amended.AccountName != "Cash on hand" {
		t.Fatalf("expected version 2 named %q, got version %d named %q", "Cash on hand", amended.Version, amended.AccountName)
	}

	head, err = s.GetAccountByCode(ctx, tenantID, "1000")
	if err != nil {
		t.Fatalf("GetAccountByCode after amend: %v", err)
	}
	if head.Version != 2 || head.AccountName != "Cash on hand" {
		t.Fatalf("head row did not advance: %+v", head)
	}

	// The assertion reads the history table directly: a version write must not
	// be verified only by the same code path that produced it.
	scoped(t, pool, tenantID, func(tx pgx.Tx) {
		var count int
		var priorClosed bool
		if err := tx.QueryRow(context.Background(),
			`SELECT COUNT(*) FROM account_versions WHERE tenant_id = $1 AND account_id = $2`,
			tenantID, acct.AccountID).Scan(&count); err != nil {
			t.Fatalf("count account_versions: %v", err)
		}
		if count != 2 {
			t.Fatalf("expected two version rows, got %d", count)
		}
		if err := tx.QueryRow(context.Background(),
			`SELECT effective_to IS NOT NULL FROM account_versions
			 WHERE tenant_id = $1 AND account_id = $2 AND version = 1`,
			tenantID, acct.AccountID).Scan(&priorClosed); err != nil {
			t.Fatalf("read version 1 interval: %v", err)
		}
		if !priorClosed {
			t.Fatal("amendment must close the prior version's interval")
		}
	})
}

func TestPgStore_ActivateDeactivate_RoundTripsThroughVersionHistory(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop(), store.WithEventRegion("uk"))
	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	acct := newTestAccount(tenantID, "1000", "Cash")
	if err := s.CreateAccount(ctx, acct); err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}

	deactivated, err := s.DeactivateAccount(ctx, tenantID, "1000", 1, time.Time{}, "cash desk closed", nil, "test-admin")
	if err != nil {
		t.Fatalf("DeactivateAccount: %v", err)
	}
	if deactivated.Status != "INACTIVE" || deactivated.Version != 2 {
		t.Fatalf("expected INACTIVE at version 2, got %s at version %d", deactivated.Status, deactivated.Version)
	}

	reactivated, err := s.ActivateAccount(ctx, tenantID, "1000", 2, time.Time{}, "cash desk reopened", nil, "test-admin")
	if err != nil {
		t.Fatalf("ActivateAccount: %v", err)
	}
	if reactivated.Status != "ACTIVE" || reactivated.Version != 3 {
		t.Fatalf("expected ACTIVE at version 3, got %s at version %d", reactivated.Status, reactivated.Version)
	}

	// Re-issuing the transition the account is already in is a no-op and must
	// not write a fourth version.
	idempotent, err := s.ActivateAccount(ctx, tenantID, "1000", 3, time.Time{}, "retry", nil, "test-admin")
	if err != nil {
		t.Fatalf("idempotent ActivateAccount: %v", err)
	}
	if idempotent.Version != 3 {
		t.Fatalf("idempotent activation must not append a version, got version %d", idempotent.Version)
	}

	scoped(t, pool, tenantID, func(tx pgx.Tx) {
		var count int
		if err := tx.QueryRow(context.Background(),
			`SELECT COUNT(*) FROM account_versions WHERE tenant_id = $1 AND account_id = $2`,
			tenantID, acct.AccountID).Scan(&count); err != nil {
			t.Fatalf("count account_versions: %v", err)
		}
		if count != 3 {
			t.Fatalf("expected three version rows (created, deactivated, activated), got %d", count)
		}
	})
}

func TestPgStore_ReparentAccount_GuardsHierarchyAndLeavesPostedFactsAlone(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop(), store.WithEventRegion("uk"))
	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	assets := newTestAccount(tenantID, "1000", "Assets")
	cash := newTestAccount(tenantID, "1010", "Cash")
	if err := s.CreateAccount(ctx, assets); err != nil {
		t.Fatalf("CreateAccount assets: %v", err)
	}
	if err := s.CreateAccount(ctx, cash); err != nil {
		t.Fatalf("CreateAccount cash: %v", err)
	}

	// Post a real ledger fact on the account that is about to move subtree.
	header := &domain.JournalHeader{
		JournalType:          domain.JournalTypeStandard,
		TransactionDate:      domain.NewDate(2026, 10, 1),
		PostingDate:          domain.NewDate(2026, 10, 1),
		CurrencyCode:         "GBP",
		JournalID:            uuid.New().String(),
		TenantID:             tenantID,
		LegalEntityID:        uuid.New().String(),
		FiscalPeriod:         "2026-10",
		Status:               domain.JournalStatusPending,
		Description:          "reparent fixture",
		CreatedByPrincipalID: "test-admin",
		CorrelationID:        "reparent-fixture",
	}
	lines := []domain.JournalLine{
		{AccountCode: "1010", DebitAmount: 100},
		{AccountCode: "4000", CreditAmount: 100},
	}
	if _, _, err := s.CreateJournal(ctx, header, lines); err != nil {
		t.Fatalf("CreateJournal: %v", err)
	}
	if err := s.TransitionJournal(ctx, tenantID, header.JournalID,
		domain.JournalStatusPending, domain.JournalStatusValidated, "test-admin"); err != nil {
		t.Fatalf("TransitionJournal PENDING->VALIDATED: %v", err)
	}
	if err := s.TransitionJournal(ctx, tenantID, header.JournalID,
		domain.JournalStatusValidated, domain.JournalStatusFinalized, "test-admin"); err != nil {
		t.Fatalf("TransitionJournal VALIDATED->FINALIZED: %v", err)
	}

	entriesBefore := countLedgerEntriesForAccount(t, pool, tenantID, "1010")

	moved, err := s.ReparentAccount(ctx, tenantID, "1010", &assets.AccountID, 1, time.Time{}, "moving cash under assets", nil, "test-admin")
	if err != nil {
		t.Fatalf("ReparentAccount: %v", err)
	}
	if moved.ParentAccountID == nil || *moved.ParentAccountID != assets.AccountID {
		t.Fatalf("expected cash under assets, got parent %v", moved.ParentAccountID)
	}
	if moved.Version != 2 {
		t.Fatalf("expected version 2 after reparent, got %d", moved.Version)
	}

	entriesAfter := countLedgerEntriesForAccount(t, pool, tenantID, "1010")
	if entriesBefore == 0 || entriesAfter != entriesBefore {
		t.Fatalf("reparent must not move or remove posted entries: before=%d after=%d", entriesBefore, entriesAfter)
	}

	// A cycle must be refused: assets cannot become a child of its own child.
	if _, err := s.ReparentAccount(ctx, tenantID, "1000", &cash.AccountID, 1, time.Time{}, "cycle attempt", nil, "test-admin"); !errors.Is(err, domain.ErrAccountHierarchyCycle) {
		t.Fatalf("expected ErrAccountHierarchyCycle, got %v", err)
	}
	// Self-parenting is the minimal cycle.
	if _, err := s.ReparentAccount(ctx, tenantID, "1000", &assets.AccountID, 1, time.Time{}, "self-parent attempt", nil, "test-admin"); !errors.Is(err, domain.ErrAccountHierarchyCycle) {
		t.Fatalf("expected ErrAccountHierarchyCycle for self-parenting, got %v", err)
	}
	// A hierarchy change backdated before today is refused outright, because
	// this chart has no entity/book with which to ask a close service whether
	// the target period is open.
	backdated := time.Now().UTC().AddDate(0, 0, -1)
	if _, err := s.ReparentAccount(ctx, tenantID, "1010", nil, 2, backdated, "backdated", nil, "test-admin"); !errors.Is(err, domain.ErrAccountHierarchyBackdated) {
		t.Fatalf("expected ErrAccountHierarchyBackdated, got %v", err)
	}
	// The refused backdated command must leave the hierarchy exactly as it was.
	stillUnderAssets, err := s.GetAccountByCode(ctx, tenantID, "1010")
	if err != nil {
		t.Fatalf("GetAccountByCode after refused backdate: %v", err)
	}
	if stillUnderAssets.ParentAccountID == nil || *stillUnderAssets.ParentAccountID != assets.AccountID {
		t.Fatalf("refused backdated change moved the account anyway: %+v", stillUnderAssets)
	}
}

func countLedgerEntriesForAccount(t *testing.T, pool interface {
	Begin(context.Context) (pgx.Tx, error)
}, tenantID, accountCode string) int {
	t.Helper()
	count := 0
	tx, err := pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin verification tx: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(context.Background(), "SELECT set_config('app.tenant_id', $1, true)", tenantID); err != nil {
		t.Fatalf("install verification tenant scope: %v", err)
	}
	if err := tx.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM ledger_entries WHERE tenant_id = $1 AND account_code = $2`,
		tenantID, accountCode).Scan(&count); err != nil {
		t.Fatalf("count ledger_entries: %v", err)
	}
	return count
}

func TestPgStore_AmendAccountVersion_StaleExpectedVersionIsRefused(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop(), store.WithEventRegion("uk"))
	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	acct := newTestAccount(tenantID, "1000", "Cash")
	if err := s.CreateAccount(ctx, acct); err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	head, _ := s.GetAccountByCode(ctx, tenantID, "1000")
	updates := *head
	updates.AccountName = "Cash on hand"

	if _, err := s.AmendAccountVersion(ctx, tenantID, "1000", 99, time.Now().UTC(), &updates, "", nil, "test-admin"); !errors.Is(err, domain.ErrAccountVersionConflict) {
		t.Fatalf("expected ErrAccountVersionConflict, got %v", err)
	}
	head, _ = s.GetAccountByCode(ctx, tenantID, "1000")
	if head.Version != 1 || head.AccountName != "Cash" {
		t.Fatalf("refused amendment must leave the head untouched, got %+v", head)
	}
}

func TestPgStore_GetAccountAsOf_ReconstructsVersionInForce(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop(), store.WithEventRegion("uk"))
	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	created := time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)
	acct := newTestAccount(tenantID, "1000", "Cash")
	acct.CreatedAt = created
	if err := s.CreateAccount(ctx, acct); err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}

	amendAt := created.Add(24 * time.Hour)
	updates := *acct
	updates.AccountName = "Cash on hand"
	if _, err := s.AmendAccountVersion(ctx, tenantID, "1000", 1, amendAt, &updates, "renamed by finance", nil, "test-admin"); err != nil {
		t.Fatalf("AmendAccountVersion: %v", err)
	}

	// Inside the first interval: version 1, whose interval was closed exactly
	// at the amendment instant.
	v1, err := s.GetAccountAsOf(ctx, tenantID, "1000", created.Add(time.Hour))
	if err != nil {
		t.Fatalf("GetAccountAsOf (v1): %v", err)
	}
	if v1.Version != 1 || v1.AccountName != "Cash" {
		t.Fatalf("expected version 1 named %q, got version %d named %q", "Cash", v1.Version, v1.AccountName)
	}
	if v1.EffectiveTo == nil || !v1.EffectiveTo.Equal(amendAt) {
		t.Fatalf("version 1 interval must close at the amendment, got %v", v1.EffectiveTo)
	}

	// The interval is half-open [effective_from, effective_to): the version
	// that starts exactly at the instant is the one in force.
	v2, err := s.GetAccountAsOf(ctx, tenantID, "1000", amendAt)
	if err != nil {
		t.Fatalf("GetAccountAsOf at amendment instant: %v", err)
	}
	if v2.Version != 2 || v2.AccountName != "Cash on hand" {
		t.Fatalf("expected version 2 at the amendment instant, got version %d named %q", v2.Version, v2.AccountName)
	}
	if v2.EffectiveTo != nil {
		t.Fatalf("the current version's interval must stay open, got %v", v2.EffectiveTo)
	}

	// And it remains in force afterwards.
	v2Later, err := s.GetAccountAsOf(ctx, tenantID, "1000", amendAt.Add(time.Hour))
	if err != nil {
		t.Fatalf("GetAccountAsOf after amendment: %v", err)
	}
	if v2Later.Version != 2 {
		t.Fatalf("expected version 2 to remain in force, got %d", v2Later.Version)
	}

	// Before the account's first version, nothing was in force.
	if _, err := s.GetAccountAsOf(ctx, tenantID, "1000", created.Add(-time.Second)); !errors.Is(err, domain.ErrAccountNotFound) {
		t.Fatalf("expected ErrAccountNotFound before the account existed, got %v", err)
	}
}
