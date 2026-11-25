package store_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"

	"zoiko.io/workflow-svc/internal/domain"
	"zoiko.io/workflow-svc/internal/outbox"
	"zoiko.io/workflow-svc/internal/store"
)

// 25 Nov 2026 repair. WF-S22 read and wrote workflow_definitions.status,
// but 000025 names the column definition_status, so every definition read
// failed. No store test covered it.
func TestPgStore_Repair_DefinitionCreateThenRead(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	setupTestDB(t, pool)
	s := store.New(pool, zap.NewNop())
	ctx := tenantCtx(testTenantID)

	hours := 48
	name := "Purchase approval"
	def, err := s.CreateWorkflowDefinition(ctx, domain.CreateWorkflowDefinitionParams{
		TenantID: testTenantID, WorkflowType: "PURCHASE_APPROVAL", Version: 1,
		StagesJSON: []byte(`[{"approver_principal_id":"approver-1"}]`),
		Name:       &name, CreatedBy: "author-1", ExpiryHours: &hours,
	})
	if err != nil {
		t.Fatalf("create definition: %v", err)
	}
	if def.Status == nil || *def.Status != "DRAFT" {
		t.Fatalf("a new definition is DRAFT, got %v", def.Status)
	}
	if def.Checksum == nil || len(*def.Checksum) != 64 {
		t.Fatalf("a new definition carries its sha256 checksum, got %v", def.Checksum)
	}

	id := def.WorkflowDefinitionID
	got, err := s.GetWorkflowDefinition(ctx, domain.GetWorkflowDefinitionParams{TenantID: testTenantID, WorkflowDefinitionID: &id})
	if err != nil {
		t.Fatalf("read definition back: %v", err)
	}
	if got.Status == nil || *got.Status != "DRAFT" || got.ExpiryHours == nil || *got.ExpiryHours != 48 {
		t.Fatalf("read back status=%v expiry=%v", got.Status, got.ExpiryHours)
	}
}

// The keyset clause was appended after ORDER BY, so following next_cursor
// to page 2 was a SQL syntax error, for both lists.
func TestPgStore_Repair_ListsFollowTheirCursor(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	setupTestDB(t, pool)
	s := store.New(pool, zap.NewNop())
	ctx := tenantCtx(testTenantID)

	made := map[string]bool{}
	for i := 0; i < 3; i++ {
		w, _, _, err := s.CreateWorkflow(ctx, twoStageParams())
		if err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
		made[w.WorkflowInstanceID] = true
	}

	type lister func(c *domain.PaginationCursor) ([]*domain.WorkflowInstance, *domain.PaginationCursor, error)
	lists := map[string]lister{
		"ListWorkflows": func(c *domain.PaginationCursor) ([]*domain.WorkflowInstance, *domain.PaginationCursor, error) {
			return s.ListWorkflows(ctx, testTenantID, domain.ListWorkflowsParams{TenantID: testTenantID, Limit: 2, Cursor: c})
		},
		"ListPendingApprovals": func(c *domain.PaginationCursor) ([]*domain.WorkflowInstance, *domain.PaginationCursor, error) {
			return s.ListPendingApprovals(ctx, testTenantID, domain.ListPendingApprovalsParams{TenantID: testTenantID, Limit: 2, Cursor: c})
		},
	}
	for name, list := range lists {
		page1, next, err := list(nil)
		if err != nil || len(page1) != 2 || next == nil {
			t.Fatalf("%s page 1: %d rows, next=%v, err=%v", name, len(page1), next, err)
		}
		page2, next2, err := list(next)
		if err != nil {
			t.Fatalf("%s page 2: %v", name, err)
		}
		if len(page2) != 1 || next2 != nil {
			t.Fatalf("%s page 2: want 1 row and no cursor, got %d rows, next=%v", name, len(page2), next2)
		}
		seen := map[string]bool{}
		for _, w := range append(page1, page2...) {
			if seen[w.WorkflowInstanceID] || !made[w.WorkflowInstanceID] {
				t.Fatalf("%s: row %s repeated or unknown", name, w.WorkflowInstanceID)
			}
			seen[w.WorkflowInstanceID] = true
		}
	}
}

// 000022 put FORCE RLS on outbox_events with a tenant-only policy, so the
// relay (no tenant) polled zero rows as the runtime role and nothing was
// ever published (found live 25 Nov 2026). As a NOBYPASSRLS role the relay
// now drains both destinations; a request transaction still sees only its
// own tenant's rows.
func TestPgStore_Repair_RelayDrainsAsTheRuntimeRole(t *testing.T) {
	admin := getTestPool(t)
	defer admin.Close()
	setupTestDB(t, admin)
	app := appRolePool(t, admin)
	s := store.New(app, zap.NewNop())

	p := twoStageParams()
	w, _, _, err := s.CreateWorkflow(tenantCtx(testTenantID), p)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, _, _, err := s.SubmitAction(tenantCtx(testTenantID), domain.SubmitActionParams{
		WorkflowInstanceID: w.WorkflowInstanceID, ActorPrincipalID: "approver-1", Action: "APPROVE",
		Meta: meta("idem-relay-1", "h-relay-1"),
	}); err != nil {
		t.Fatalf("approve stage 1: %v", err)
	}

	var mu sync.Mutex
	delivered := map[string]int{}
	for _, dest := range []string{outbox.DestinationKafka, outbox.DestinationDecisionLog} {
		dest := dest
		relay := outbox.NewRelay(app, outbox.DelivererFunc(func(_ context.Context, e outbox.StoredEvent) error {
			mu.Lock()
			delivered[dest]++
			mu.Unlock()
			return nil
		}), dest, time.Second, 50, zap.NewNop())
		relay.RelayOnce(context.Background())
	}
	if delivered[outbox.DestinationKafka] == 0 || delivered[outbox.DestinationDecisionLog] == 0 {
		t.Fatalf("the relay must see the rows as the runtime role, delivered %v", delivered)
	}

	var unpublished int
	if err := admin.QueryRow(context.Background(),
		`SELECT count(*) FROM outbox_events WHERE published_at IS NULL`).Scan(&unpublished); err != nil {
		t.Fatalf("count: %v", err)
	}
	if unpublished != 0 {
		t.Fatalf("%d outbox rows left unpublished after one pass per destination", unpublished)
	}

	// Outside the relay, a transaction for another tenant still sees none.
	other := "00000000-0000-0000-0000-0000000000ff"
	tx, err := app.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(context.Background(), `SELECT set_config('app.tenant_id', $1, true)`, other); err != nil {
		t.Fatalf("set tenant: %v", err)
	}
	var seen int
	if err := tx.QueryRow(context.Background(), `SELECT count(*) FROM outbox_events`).Scan(&seen); err != nil {
		t.Fatalf("count as other tenant: %v", err)
	}
	if seen != 0 {
		t.Fatalf("another tenant's transaction sees %d outbox rows, want 0", seen)
	}
}
