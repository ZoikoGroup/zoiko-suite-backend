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
