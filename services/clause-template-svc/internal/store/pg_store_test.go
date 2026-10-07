package store_test

import (
	"context"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/clause-template-svc/internal/domain"
	"zoiko.io/clause-template-svc/internal/middleware"
	"zoiko.io/clause-template-svc/internal/store"
)

func openTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("Skipping Postgres integration test: TEST_DATABASE_URL not set")
	}
	requireThrowawayDatabase(t, dsn)

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("failed to connect to postgres: %v", err)
	}
	t.Cleanup(pool.Close)

	_, filename, _, _ := runtime.Caller(0)
	base := filepath.Dir(filename)

	_, _ = pool.Exec(ctx, `DROP TABLE IF EXISTS clause_deviation_rules, clause_versions, contract_templates, clauses CASCADE;`)

	for _, name := range []string{
		"000001_initial_schema.up.sql",
		"000002_approval_workflow_and_tenant_isolation.up.sql",
	} {
		sql, err := os.ReadFile(filepath.Join(base, "../../deployments/migrations", name))
		if err != nil {
			t.Fatalf("failed to read migration %s: %v", name, err)
		}
		if _, err := pool.Exec(ctx, string(sql)); err != nil {
			t.Fatalf("failed to apply migration %s: %v", name, err)
		}
	}

	return pool
}

func requireThrowawayDatabase(t *testing.T, dsn string) {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("refusing to run: TEST_DATABASE_URL is not a parseable URL: %v", err)
	}
	dbName := strings.TrimPrefix(u.Path, "/")
	if !strings.Contains(strings.ToLower(dbName), "test") {
		t.Fatalf("refusing to run: TEST_DATABASE_URL names database %q, which is not recognisably "+
			"disposable, and this suite DROPs clauses and contract_templates.", dbName)
	}
}

func tenantCtx(tenantID string) context.Context {
	return middleware.WithTenant(context.Background(), tenantID)
}

func newClause(legalEntityID, createdBy string) *domain.Clause {
	return &domain.Clause{
		LegalEntityID:  legalEntityID,
		Title:          "Confidentiality Standard",
		Category:       domain.ClauseCategoryConfidentiality,
		Body:           "All information shared shall remain confidential.",
		JurisdictionID: "us-delaware",
		EffectiveFrom:  "2026-01-01",
		CreatedBy:      createdBy,
	}
}

func TestPgStore_ClauseFullLifecycle_ReachesActive(t *testing.T) {
	pool := openTestPool(t)
	s := store.NewPgStore(pool)
	ctx := tenantCtx("tenant-a")

	c := newClause("le-us", "drafter-1")
	if err := s.CreateClause(ctx, c); err != nil {
		t.Fatalf("create: %v", err)
	}
	if c.Status != domain.StatusDraft {
		t.Fatalf("status = %s, want DRAFT", c.Status)
	}

	reviewed, err := s.SubmitClauseForLegalReview(ctx, c.ClauseID, "drafter-1")
	if err != nil {
		t.Fatalf("submit review: %v", err)
	}
	if reviewed.Status != domain.StatusLegalReview {
		t.Fatalf("status after submit = %s, want LEGAL_REVIEW", reviewed.Status)
	}

	approved, err := s.ApproveClause(ctx, c.ClauseID, "legal-lead-1")
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if approved.Status != domain.StatusApproved {
		t.Fatalf("status after approve = %s, want APPROVED", approved.Status)
	}

	activated, err := s.ActivateClause(ctx, c.ClauseID, "legal-lead-1")
	if err != nil {
		t.Fatalf("activate: %v", err)
	}
	if activated.Status != domain.StatusActive {
		t.Fatalf("status after activate = %s, want ACTIVE", activated.Status)
	}

	versions, err := s.ListClauseVersions(ctx, c.ClauseID)
	if err != nil {
		t.Fatalf("list versions: %v", err)
	}
	// Initial draft, approved = 2 snapshots (activate does not re-snapshot
	// content, only status/attribution columns).
	if len(versions) != 2 {
		t.Fatalf("expected 2 version snapshots, got %d", len(versions))
	}
}

// Maker-checker (LEG-06 §8): the submitter may not approve their own clause.
func TestPgStore_ApproveClause_SelfApprovalIsRefusedAtTheWrite(t *testing.T) {
	pool := openTestPool(t)
	s := store.NewPgStore(pool)
	ctx := tenantCtx("tenant-a")

	c := newClause("le-us", "drafter-1")
	if err := s.CreateClause(ctx, c); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.SubmitClauseForLegalReview(ctx, c.ClauseID, "drafter-1"); err != nil {
		t.Fatalf("submit review: %v", err)
	}
	if _, err := s.ApproveClause(ctx, c.ClauseID, "drafter-1"); !errors.Is(err, domain.ErrSelfApprovalNotAllowed) {
		t.Fatalf("self-approve returned %v, want ErrSelfApprovalNotAllowed", err)
	}
}

// An approved clause version is immutable: UpdateClause (CreateClauseVersion)
// must refuse once the clause has left DRAFT/LEGAL_REVIEW.
func TestPgStore_UpdateClause_RefusedOnceApproved(t *testing.T) {
	pool := openTestPool(t)
	s := store.NewPgStore(pool)
	ctx := tenantCtx("tenant-a")

	c := newClause("le-us", "drafter-1")
	if err := s.CreateClause(ctx, c); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.SubmitClauseForLegalReview(ctx, c.ClauseID, "drafter-1"); err != nil {
		t.Fatalf("submit review: %v", err)
	}
	if _, err := s.ApproveClause(ctx, c.ClauseID, "legal-lead-1"); err != nil {
		t.Fatalf("approve: %v", err)
	}

	c.Title = "Sneaky edit"
	if err := s.UpdateClause(ctx, c, "sneaking in a change"); !errors.Is(err, domain.ErrWrongStatus) {
		t.Fatalf("update after APPROVED returned %v, want ErrWrongStatus", err)
	}
}

func TestPgStore_SupersedeClause_RequiresActive(t *testing.T) {
	pool := openTestPool(t)
	s := store.NewPgStore(pool)
	ctx := tenantCtx("tenant-a")

	c := newClause("le-us", "drafter-1")
	if err := s.CreateClause(ctx, c); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.SupersedeClause(ctx, c.ClauseID, "cls-999"); !errors.Is(err, domain.ErrWrongStatus) {
		t.Fatalf("supersede of a DRAFT clause returned %v, want ErrWrongStatus", err)
	}
}

// Accountable approval (LEG-06 §8.1): the proposer may not approve their own
// deviation rule.
func TestPgStore_ApproveDeviationRule_SelfApprovalIsRefused(t *testing.T) {
	pool := openTestPool(t)
	s := store.NewPgStore(pool)
	ctx := tenantCtx("tenant-a")

	d := &domain.DeviationRule{
		LegalEntityID: "le-us", JurisdictionID: "us-delaware",
		RiskClassification: domain.RiskHigh, Description: "Cap liability at 2x fees", ProposedBy: "lawyer-1",
	}
	if err := s.CreateDeviationRule(ctx, d); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.ApproveDeviationRule(ctx, d.DeviationID, "lawyer-1"); !errors.Is(err, domain.ErrSelfApprovalNotAllowed) {
		t.Fatalf("self-approve returned %v, want ErrSelfApprovalNotAllowed", err)
	}

	approved, err := s.ApproveDeviationRule(ctx, d.DeviationID, "legal-lead-1")
	if err != nil {
		t.Fatalf("approve by a different principal: %v", err)
	}
	if approved.Status != domain.DeviationStatusApproved {
		t.Fatalf("status = %s, want APPROVED", approved.Status)
	}
}

// A write must not be able to conclude another tenant's clause, and the
// policy must actually apply — FORCE ROW LEVEL SECURITY plus this store's
// own explicit tenant_id predicates (migration 000002) are what make that
// true for this superuser connection.
func TestPgStore_Transitions_AreTenantScoped(t *testing.T) {
	pool := openTestPool(t)
	s := store.NewPgStore(pool)

	c := newClause("le-us", "drafter-1")
	if err := s.CreateClause(tenantCtx("tenant-a"), c); err != nil {
		t.Fatalf("create: %v", err)
	}

	if _, err := s.GetClause(tenantCtx("tenant-b"), c.ClauseID); !errors.Is(err, domain.ErrClauseNotFound) {
		t.Fatalf("cross-tenant read returned %v, want ErrClauseNotFound", err)
	}
	if _, err := s.SubmitClauseForLegalReview(tenantCtx("tenant-b"), c.ClauseID, "drafter-1"); !errors.Is(err, domain.ErrClauseNotFound) {
		t.Fatalf("cross-tenant submit returned %v, want ErrClauseNotFound", err)
	}
}

// A store call with no tenant must be refused, not run with an empty scope.
func TestPgStore_WithoutTenant_IsRefused(t *testing.T) {
	pool := openTestPool(t)
	s := store.NewPgStore(pool)
	ctx := context.Background()

	if err := s.CreateClause(ctx, newClause("le-us", "drafter-1")); !errors.Is(err, domain.ErrTenantMissing) {
		t.Fatalf("create without tenant returned %v, want ErrTenantMissing", err)
	}
}
