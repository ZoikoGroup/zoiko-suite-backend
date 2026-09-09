package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"zoiko.io/project-accounting-svc/internal/domain"
	svcmiddleware "zoiko.io/project-accounting-svc/internal/middleware"
	"zoiko.io/project-accounting-svc/internal/store"
)

func newActiveTestProject(t *testing.T, s *store.PgStore, ctx context.Context, tenantID, legalEntityID, code string) string {
	t.Helper()
	p := newTestProject(tenantID, legalEntityID, code)
	if err := s.CreateProject(ctx, p); err != nil {
		t.Fatalf("CreateProject failed: %v", err)
	}
	now := time.Now().UTC()
	if err := s.ApproveProject(ctx, p.ProjectID, "approver-1", now); err != nil {
		t.Fatalf("ApproveProject failed: %v", err)
	}
	if err := s.ActivateProject(ctx, p.ProjectID, now); err != nil {
		t.Fatalf("ActivateProject failed: %v", err)
	}
	return p.ProjectID
}

func newDraftCostEntry(projectID, sourceType, sourceRef string, amount float64) *domain.CostEntry {
	return &domain.CostEntry{
		EntryID: uuid.New().String(), ProjectID: projectID, SourceType: sourceType, SourceReference: sourceRef,
		Amount: amount, Currency: "USD", TransactionDate: time.Now().UTC(),
		Status: domain.CostEntryStatusCaptured, CreatedAt: time.Now().UTC(), CreatedByPrincipalID: "capturer-1",
	}
}

// TestPgStore_CaptureProjectCost_DuplicateSourceReference_ReturnsOriginal
// is the real proof of negative path #1.
func TestPgStore_CaptureProjectCost_DuplicateSourceReference_ReturnsOriginal(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantID := uuid.New().String()
	legalEntityID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	projectID := newActiveTestProject(t, s, ctx, tenantID, legalEntityID, "PRJ-COST-1")

	first := newDraftCostEntry(projectID, domain.CostSourceTypeAP, "idem-cost-1", 500)
	if err := s.CaptureProjectCost(ctx, first); err != nil {
		t.Fatalf("first CaptureProjectCost failed: %v", err)
	}
	firstID := first.EntryID

	second := newDraftCostEntry(projectID, domain.CostSourceTypeAP, "idem-cost-1", 500)
	if err := s.CaptureProjectCost(ctx, second); err != nil {
		t.Fatalf("second CaptureProjectCost failed: %v", err)
	}
	if second.EntryID != firstID {
		t.Fatalf("expected the original entry returned for a duplicate source reference, got a new one: %q vs %q", second.EntryID, firstID)
	}
}

// TestPgStore_CostEntry_EconomicFieldsImmutable is the real proof of the
// append-only backbone behind negative paths #1 and #4.
func TestPgStore_CostEntry_EconomicFieldsImmutable(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantID := uuid.New().String()
	legalEntityID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	projectID := newActiveTestProject(t, s, ctx, tenantID, legalEntityID, "PRJ-COST-2")

	e := newDraftCostEntry(projectID, domain.CostSourceTypeAP, "idem-cost-2", 250)
	if err := s.CaptureProjectCost(ctx, e); err != nil {
		t.Fatalf("CaptureProjectCost failed: %v", err)
	}

	if _, err := pool.Exec(context.Background(), `UPDATE project_cost_entries SET amount = 99999 WHERE entry_id = $1`, e.EntryID); err == nil {
		t.Fatalf("expected the reject-mutation trigger to refuse an amount UPDATE, got no error")
	}
	// Status IS allowed to change (evidentiary progression, not an
	// economic field) — proves the trigger is scoped correctly, not
	// simply blocking every UPDATE.
	if err := s.ValidateProjectCost(ctx, e.EntryID, time.Now().UTC()); err != nil {
		t.Fatalf("expected ValidateProjectCost (a status-only UPDATE) to succeed, got %v", err)
	}
}

// TestPgStore_CreateLinkedCostEntry_SelfApprovalRefused is the real proof
// of negative path #4.
func TestPgStore_CreateLinkedCostEntry_SelfApprovalRefused(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantID := uuid.New().String()
	legalEntityID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	projectID := newActiveTestProject(t, s, ctx, tenantID, legalEntityID, "PRJ-COST-3")

	e := newDraftCostEntry(projectID, domain.CostSourceTypeAP, "idem-cost-3", 400)
	if err := s.CaptureProjectCost(ctx, e); err != nil {
		t.Fatalf("CaptureProjectCost failed: %v", err)
	}

	now := time.Now().UTC()
	if _, err := s.CreateLinkedCostEntry(ctx, e.EntryID, "capturer-1", "wrong category", false, uuid.New().String(), nil, nil, nil, nil, now); err != domain.ErrSelfApprovalNotPermittedReclassify {
		t.Fatalf("expected ErrSelfApprovalNotPermittedReclassify, got %v", err)
	}
	linked, err := s.CreateLinkedCostEntry(ctx, e.EntryID, "reviewer-2", "wrong category", false, uuid.New().String(), nil, nil, nil, nil, now)
	if err != nil {
		t.Fatalf("expected reclassify by a different principal to succeed, got %v", err)
	}
	if linked.ReclassifiesEntryID == nil || *linked.ReclassifiesEntryID != e.EntryID {
		t.Fatalf("expected linked entry to reference the original, got %+v", linked)
	}
}

// TestPgStore_CreateLinkedCostEntry_ReversalNetsOut proves a real
// reversal against Postgres nets the original out of any SUM query.
func TestPgStore_CreateLinkedCostEntry_ReversalNetsOut(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantID := uuid.New().String()
	legalEntityID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	projectID := newActiveTestProject(t, s, ctx, tenantID, legalEntityID, "PRJ-COST-4")

	e := newDraftCostEntry(projectID, domain.CostSourceTypeAP, "idem-cost-4", 250)
	if err := s.CaptureProjectCost(ctx, e); err != nil {
		t.Fatalf("CaptureProjectCost failed: %v", err)
	}
	now := time.Now().UTC()
	reversal, err := s.CreateLinkedCostEntry(ctx, e.EntryID, "reviewer-2", "payroll reversed", true, uuid.New().String(), nil, nil, nil, nil, now)
	if err != nil {
		t.Fatalf("CreateLinkedCostEntry (reversal) failed: %v", err)
	}
	if reversal.Amount != -250 {
		t.Fatalf("expected reversal amount=-250, got %v", reversal.Amount)
	}

	entries, err := s.ListCostEntries(ctx, projectID, "")
	if err != nil {
		t.Fatalf("ListCostEntries failed: %v", err)
	}
	var total float64
	for _, en := range entries {
		total += en.Amount
	}
	if total != 0 {
		t.Fatalf("expected net total=0 after reversal, got %v", total)
	}

	// A second reversal must be refused.
	if _, err := s.CreateLinkedCostEntry(ctx, e.EntryID, "reviewer-3", "again", true, uuid.New().String(), nil, nil, nil, nil, now); err != domain.ErrCostEntryAlreadyReversed {
		t.Fatalf("expected ErrCostEntryAlreadyReversed, got %v", err)
	}
}

// TestPgStore_CertifyCostPopulation_ExcludesReversedOriginal proves the
// real certification query against Postgres.
func TestPgStore_CertifyCostPopulation_ExcludesReversedOriginal(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantID := uuid.New().String()
	legalEntityID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	projectID := newActiveTestProject(t, s, ctx, tenantID, legalEntityID, "PRJ-COST-5")

	kept := newDraftCostEntry(projectID, domain.CostSourceTypeAP, "idem-cost-5a", 100)
	if err := s.CaptureProjectCost(ctx, kept); err != nil {
		t.Fatalf("CaptureProjectCost (kept) failed: %v", err)
	}
	toReverse := newDraftCostEntry(projectID, domain.CostSourceTypeAP, "idem-cost-5b", 50)
	if err := s.CaptureProjectCost(ctx, toReverse); err != nil {
		t.Fatalf("CaptureProjectCost (toReverse) failed: %v", err)
	}
	now := time.Now().UTC()
	if _, err := s.CreateLinkedCostEntry(ctx, toReverse.EntryID, "reviewer-2", "x", true, uuid.New().String(), nil, nil, nil, nil, now); err != nil {
		t.Fatalf("CreateLinkedCostEntry (reversal) failed: %v", err)
	}

	cert, err := s.CertifyCostPopulation(ctx, projectID, "certifier-1", now)
	if err != nil {
		t.Fatalf("CertifyCostPopulation failed: %v", err)
	}
	if cert.TotalAmount != 50 {
		t.Fatalf("expected total_amount=50 (100 - 50 reversal, REVERSED original excluded), got %v", cert.TotalAmount)
	}
	if cert.EntryCount != 2 {
		t.Fatalf("expected entry_count=2 (kept + the reversal entry), got %d", cert.EntryCount)
	}
}
