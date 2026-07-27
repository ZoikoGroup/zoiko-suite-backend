//go:build integration

package store_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/treasury-svc/internal/domain"
	svcmiddleware "zoiko.io/treasury-svc/internal/middleware"
	"zoiko.io/treasury-svc/internal/store"
)

var (
	testPool  *pgxpool.Pool
	testStore *store.PgStore
)

func TestMain(m *testing.M) {
	dbPort := uint32(15701 + uint32(os.Getpid()%499))
	pg := embeddedpostgres.NewDatabase(
		embeddedpostgres.DefaultConfig().
			Port(dbPort).
			Database("treasury_isolation_test").
			Username("postgres").
			Password("postgres"),
	)
	if err := pg.Start(); err != nil {
		fmt.Printf("failed to start embedded postgres: %v\n", err)
		os.Exit(1)
	}

	dsn := fmt.Sprintf(
		"host=localhost port=%d dbname=treasury_isolation_test user=postgres password=postgres sslmode=disable",
		dbPort,
	)

	ctx := context.Background()
	var err error
	testPool, err = pgxpool.New(ctx, dsn)
	if err != nil {
		fmt.Printf("failed to connect to postgres: %v\n", err)
		_ = pg.Stop()
		os.Exit(1)
	}

	for i := 0; i < 75; i++ {
		if err = testPool.Ping(ctx); err == nil {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if err != nil {
		fmt.Printf("postgres did not become ready: %v\n", err)
		testPool.Close()
		_ = pg.Stop()
		os.Exit(1)
	}

	for _, migration := range []string{
		"000001_initial_schema.up.sql",
		"000002_add_idempotency_index.up.sql",
	} {
		sql, err := os.ReadFile("../../deployments/migrations/" + migration)
		if err != nil {
			fmt.Printf("failed to read migration %s: %v\n", migration, err)
			testPool.Close()
			_ = pg.Stop()
			os.Exit(1)
		}
		if _, err = testPool.Exec(ctx, string(sql)); err != nil {
			fmt.Printf("failed to apply migration %s: %v\n", migration, err)
			testPool.Close()
			_ = pg.Stop()
			os.Exit(1)
		}
	}

	testStore = store.New(testPool, zap.NewNop())

	code := m.Run()

	testPool.Close()
	_ = pg.Stop()
	os.Exit(code)
}

func cleanTables(t *testing.T) {
	t.Helper()
	_, err := testPool.Exec(context.Background(), "DELETE FROM transfers; DELETE FROM cash_balances; DELETE FROM bank_accounts; DELETE FROM liquidity_thresholds;")
	if err != nil {
		t.Fatalf("failed to clean tables: %v", err)
	}
}

func newTestAccount(tenantID, legalEntityID string) *domain.BankAccount {
	return &domain.BankAccount{
		BankAccountID:       uuid.New().String(),
		TenantID:            tenantID,
		LegalEntityID:       legalEntityID,
		AccountName:         "Corporate Checking",
		MaskedAccountNumber: "****1234",
		BankIdentifier:      "SWIFT-TEST",
		CurrencyCode:        "USD",
		AccountStatus:       "ACTIVE",
	}
}

// TestPgStore_ExecuteTransfer_RetriedCorrelationID_DoesNotDoubleMoveMoney
// proves the idempotency guarantee against a REAL Postgres unique index —
// this is the exact scenario a network-timeout-triggered client retry
// produces, and it must not debit the source or credit the target twice.
func TestPgStore_ExecuteTransfer_RetriedCorrelationID_DoesNotDoubleMoveMoney(t *testing.T) {
	cleanTables(t)
	s := testStore

	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	src := newTestAccount(tenantID, uuid.New().String())
	tgt := newTestAccount(tenantID, uuid.New().String())
	if err := s.CreateBankAccount(ctx, src); err != nil {
		t.Fatalf("failed to create source account: %v", err)
	}
	if err := s.CreateBankAccount(ctx, tgt); err != nil {
		t.Fatalf("failed to create target account: %v", err)
	}

	if err := s.CreateCashBalance(ctx, &domain.CashBalance{
		BalanceID: uuid.New().String(), TenantID: tenantID, BankAccountID: src.BankAccountID,
		LedgerBalance: 1000.0, AvailableBalance: 1000.0, AsOfTimestamp: time.Now().UTC(), CorrelationID: "corr-init-src",
	}); err != nil {
		t.Fatalf("failed to seed source balance: %v", err)
	}
	if err := s.CreateCashBalance(ctx, &domain.CashBalance{
		BalanceID: uuid.New().String(), TenantID: tenantID, BankAccountID: tgt.BankAccountID,
		LedgerBalance: 500.0, AvailableBalance: 500.0, AsOfTimestamp: time.Now().UTC(), CorrelationID: "corr-init-tgt",
	}); err != nil {
		t.Fatalf("failed to seed target balance: %v", err)
	}

	created1, err := s.ExecuteTransfer(ctx, src.BankAccountID, tgt.BankAccountID, 200.0, "USD", "corr-retry-1")
	if err != nil {
		t.Fatalf("first ExecuteTransfer failed: %v", err)
	}
	if !created1 {
		t.Fatal("expected created=true on the first call")
	}

	// Simulate a client retry: identical call, same correlation_id.
	created2, err := s.ExecuteTransfer(ctx, src.BankAccountID, tgt.BankAccountID, 200.0, "USD", "corr-retry-1")
	if err != nil {
		t.Fatalf("retried ExecuteTransfer failed: %v", err)
	}
	if created2 {
		t.Fatal("expected created=false on the retried call — this is a double-money-movement bug if it's true")
	}

	resSrc, err := s.GetLatestCashBalance(ctx, src.BankAccountID)
	if err != nil || resSrc == nil {
		t.Fatalf("failed to get final source balance: %v", err)
	}
	if resSrc.AvailableBalance != 800.0 {
		t.Fatalf("DOUBLE DEBIT: expected source available balance to be 800 (debited once), got %f", resSrc.AvailableBalance)
	}

	resTgt, err := s.GetLatestCashBalance(ctx, tgt.BankAccountID)
	if err != nil || resTgt == nil {
		t.Fatalf("failed to get final target balance: %v", err)
	}
	if resTgt.AvailableBalance != 700.0 {
		t.Fatalf("DOUBLE CREDIT: expected target available balance to be 700 (credited once), got %f", resTgt.AvailableBalance)
	}

	var transferCount int
	if err := testPool.QueryRow(ctx, `SELECT COUNT(*) FROM transfers WHERE tenant_id = $1 AND correlation_id = $2`,
		tenantID, "corr-retry-1").Scan(&transferCount); err != nil {
		t.Fatalf("count query failed: %v", err)
	}
	if transferCount != 1 {
		t.Fatalf("expected exactly 1 transfers row for this correlation_id, got %d", transferCount)
	}
}

func TestPgStore_CreateAndGetBankAccount(t *testing.T) {
	cleanTables(t)
	s := testStore

	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	acct := newTestAccount(tenantID, uuid.New().String())
	if err := s.CreateBankAccount(ctx, acct); err != nil {
		t.Fatalf("CreateBankAccount failed: %v", err)
	}

	got, err := s.GetBankAccount(ctx, acct.BankAccountID)
	if err != nil {
		t.Fatalf("GetBankAccount failed: %v", err)
	}
	if got == nil {
		t.Fatal("expected bank account to be found")
	}
	if got.AccountName != acct.AccountName {
		t.Fatalf("expected account name %s, got %s", acct.AccountName, got.AccountName)
	}
}

func TestPgStore_RLS_TenantIsolation(t *testing.T) {
	cleanTables(t)
	s := testStore

	tenantA := uuid.New().String()
	tenantB := uuid.New().String()
	ctxA := svcmiddleware.WithTenant(context.Background(), tenantA)
	ctxB := svcmiddleware.WithTenant(context.Background(), tenantB)

	acctA := newTestAccount(tenantA, uuid.New().String())
	if err := s.CreateBankAccount(ctxA, acctA); err != nil {
		t.Fatalf("CreateBankAccount (tenant A) failed: %v", err)
	}

	// Query tenant A's account while scoped to tenant B's context — must return nil/error
	got, err := s.GetBankAccount(ctxB, acctA.BankAccountID)
	if err != nil && err != domain.ErrIdentityMissing {
		t.Fatalf("GetBankAccount under tenant B's context returned an unexpected error: %v", err)
	}
	if got != nil {
		t.Fatal("tenant isolation failure: tenant B was able to read tenant A's bank account")
	}

	// Verify Tenant B cannot update Tenant A's account status
	err = s.UpdateBankAccountStatus(ctxB, acctA.BankAccountID, "CLOSED")
	if err == nil {
		t.Fatal("tenant isolation failure: tenant B was able to update tenant A's status")
	}
}

func TestPgStore_ExecuteTransfer_And_Isolation(t *testing.T) {
	cleanTables(t)
	s := testStore

	tenantA := uuid.New().String()
	tenantB := uuid.New().String()
	ctxA := svcmiddleware.WithTenant(context.Background(), tenantA)
	ctxB := svcmiddleware.WithTenant(context.Background(), tenantB)

	acctA1 := newTestAccount(tenantA, uuid.New().String())
	acctA2 := newTestAccount(tenantA, uuid.New().String())
	if err := s.CreateBankAccount(ctxA, acctA1); err != nil {
		t.Fatalf("failed to create source account A1: %v", err)
	}
	if err := s.CreateBankAccount(ctxA, acctA2); err != nil {
		t.Fatalf("failed to create target account A2: %v", err)
	}

	// Record initial balances for tenant A
	balA1 := &domain.CashBalance{
		BalanceID:        uuid.New().String(),
		TenantID:         tenantA,
		BankAccountID:    acctA1.BankAccountID,
		LedgerBalance:    1000.0,
		AvailableBalance: 1000.0,
		AsOfTimestamp:    time.Now().UTC(),
		CorrelationID:    "corr-init-1",
	}
	balA2 := &domain.CashBalance{
		BalanceID:        uuid.New().String(),
		TenantID:         tenantA,
		BankAccountID:    acctA2.BankAccountID,
		LedgerBalance:    500.0,
		AvailableBalance: 500.0,
		AsOfTimestamp:    time.Now().UTC(),
		CorrelationID:    "corr-init-2",
	}

	if err := s.CreateCashBalance(ctxA, balA1); err != nil {
		t.Fatalf("failed to insert initial balance A1: %v", err)
	}
	if err := s.CreateCashBalance(ctxA, balA2); err != nil {
		t.Fatalf("failed to insert initial balance A2: %v", err)
	}

	// Attempt transfer scoped to Tenant B's context — must fail
	_, err := s.ExecuteTransfer(ctxB, acctA1.BankAccountID, acctA2.BankAccountID, 100.0, "USD", "attacker-corr")
	if err == nil {
		t.Fatal("tenant isolation failure: Tenant B was allowed to execute transfer on Tenant A's accounts")
	}

	// Correct transfer scoped to Tenant A
	created, err := s.ExecuteTransfer(ctxA, acctA1.BankAccountID, acctA2.BankAccountID, 200.0, "USD", "valid-transfer")
	if err != nil {
		t.Fatalf("transfer failed: %v", err)
	}
	if !created {
		t.Fatal("expected created=true on the first transfer")
	}

	// Check final balances for Tenant A
	resA1, err := s.GetLatestCashBalance(ctxA, acctA1.BankAccountID)
	if err != nil || resA1 == nil {
		t.Fatalf("failed to get final balance A1: %v", err)
	}
	if resA1.AvailableBalance != 800.0 {
		t.Fatalf("expected final available balance A1 to be 800, got %f", resA1.AvailableBalance)
	}

	resA2, err := s.GetLatestCashBalance(ctxA, acctA2.BankAccountID)
	if err != nil || resA2 == nil {
		t.Fatalf("failed to get final balance A2: %v", err)
	}
	if resA2.AvailableBalance != 700.0 {
		t.Fatalf("expected final available balance A2 to be 700, got %f", resA2.AvailableBalance)
	}
}
