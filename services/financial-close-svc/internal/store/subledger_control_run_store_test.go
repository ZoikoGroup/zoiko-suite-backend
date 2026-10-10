package store_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/financial-close-svc/internal/domain"
	svcmiddleware "zoiko.io/financial-close-svc/internal/middleware"
	"zoiko.io/financial-close-svc/internal/store"
)

// Every subledger type RunSubledgerControl accepts must be storable. The
// column used to be VARCHAR(10), so five of the eight types failed at the
// database with "value too long" while the handler tests, which use a stub
// store, passed: DEPRECIATION_COMPLETENESS, INVENTORY_QUANTITY,
// INVENTORY_VALUE, PROJECT_REVENUE and STOCK_COUNT.
func TestCreateControlRun_StoresEverySubledgerType(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop(), store.WithEventRegion("uk"))
	tenantID := uuid.NewString()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	entity := "le-" + uuid.NewString()[:8]

	for _, ledger := range []string{"AP", "AR", "ASSETS", "DEPRECIATION_COMPLETENESS",
		"INVENTORY_QUANTITY", "INVENTORY_VALUE", "PROJECT_REVENUE", "STOCK_COUNT"} {
		run := &domain.SubledgerControlRun{
			ControlRunID: uuid.NewString(), TenantID: tenantID, LegalEntityID: entity,
			FiscalPeriod: "2026-10", Subledger: ledger, ControlAccountCode: "1500",
			Status: "MATCHED", RunAt: time.Now().UTC(), RunByPrincipalID: "controller-1",
		}
		if ledger == "ASSETS" {
			run.BookID = "STATUTORY"
		}
		if err := s.CreateControlRun(ctx, run, "corr-1", "actor-1"); err != nil {
			t.Fatalf("%s: could not be stored: %v", ledger, err)
		}
	}

	runs, err := s.ListControlRuns(ctx, entity, "2026-10")
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 8 {
		t.Fatalf("listed %d runs, want 8", len(runs))
	}
	for _, run := range runs {
		want := ""
		if run.Subledger == "ASSETS" {
			want = "STATUTORY"
		}
		if run.BookID != want {
			t.Fatalf("%s run book_id = %q, want %q", run.Subledger, run.BookID, want)
		}
	}
}

// An ASSETS run proves one book's net book value against the GL. Stored
// without the book, the evidence cannot say what was proven.
func TestCreateControlRun_AssetsRunRequiresItsBook(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop(), store.WithEventRegion("uk"))
	tenantID := uuid.NewString()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	err := s.CreateControlRun(ctx, &domain.SubledgerControlRun{
		ControlRunID: uuid.NewString(), TenantID: tenantID, LegalEntityID: "le-1",
		FiscalPeriod: "2026-10", Subledger: "ASSETS", ControlAccountCode: "1500",
		Status: "MATCHED", RunAt: time.Now().UTC(), RunByPrincipalID: "controller-1",
	}, "corr-1", "actor-1")
	if err == nil {
		t.Fatal("an ASSETS run without a book was stored")
	}
}

// The column admits only the types the service knows: a typo must fail at
// the database rather than create a run no close requirement will ever match.
func TestCreateControlRun_UnknownSubledgerIsRejected(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop(), store.WithEventRegion("uk"))
	tenantID := uuid.NewString()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	err := s.CreateControlRun(ctx, &domain.SubledgerControlRun{
		ControlRunID: uuid.NewString(), TenantID: tenantID, LegalEntityID: "le-1",
		FiscalPeriod: "2026-10", Subledger: "INVENTORY_VALEU", ControlAccountCode: "1500",
		Status: "MATCHED", RunAt: time.Now().UTC(), RunByPrincipalID: "controller-1",
	}, "corr-1", "actor-1")
	if err == nil || !strings.Contains(err.Error(), "subledger") {
		t.Fatalf("unknown subledger type: err = %v, want a check-constraint violation", err)
	}
}
