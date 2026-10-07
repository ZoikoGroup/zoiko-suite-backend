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

	"zoiko.io/contract-lifecycle-svc/internal/domain"
	"zoiko.io/contract-lifecycle-svc/internal/middleware"
	"zoiko.io/contract-lifecycle-svc/internal/store"
)

// openTestPool connects to a real Postgres and reapplies the migrations from
// a clean slate. Skips (not fails) if TEST_DATABASE_URL isn't set — same
// convention as every other service in this platform.
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

	_, _ = pool.Exec(ctx, `DROP TABLE IF EXISTS contract_versions, contracts CASCADE;`)

	for _, name := range []string{
		"000001_initial_schema.up.sql",
		"000002_add_governance_decision_id.up.sql",
		"000003_orthogonal_status_and_amend_renew.up.sql",
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
			"disposable, and this suite DROPs contracts and contract_versions.", dbName)
	}
}

func tenantCtx(tenantID string) context.Context {
	return middleware.WithTenant(context.Background(), tenantID)
}

func newContract(legalEntityID, createdBy string) *domain.Contract {
	return &domain.Contract{
		LegalEntityID:    legalEntityID,
		ContractType:     domain.ContractTypeVendor,
		Title:            "Cloud Services Agreement",
		CounterpartyID:   "cp-001",
		CounterpartyName: "Acme Cloud",
		EffectiveFrom:    "2026-01-01",
		Currency:         "USD",
		TotalValue:       50000,
		CreatedBy:        createdBy,
	}
}

// The full happy path: a contract must pass through every orthogonal
// dimension transition to reach EFFECTIVE, and each command's own
// attribution is recorded distinctly.
func TestPgStore_FullLifecycle_ReachesEffective(t *testing.T) {
	pool := openTestPool(t)
	s := store.NewPgStore(pool)
	ctx := tenantCtx("tenant-a")

	c := newContract("le-us", "drafter-1")
	if err := s.CreateContract(ctx, c); err != nil {
		t.Fatalf("create: %v", err)
	}
	if c.Status != domain.ContractStatusDraft || c.SignatureStatus != domain.SignatureStatusNotRequested {
		t.Fatalf("new contract status = %s/%s, want DRAFT/NOT_REQUESTED", c.Status, c.SignatureStatus)
	}

	reviewed, err := s.SubmitReview(ctx, c.ContractID, "drafter-1")
	if err != nil {
		t.Fatalf("submit review: %v", err)
	}
	if reviewed.Status != domain.ContractStatusReview {
		t.Fatalf("status after submit = %s, want REVIEW", reviewed.Status)
	}

	approved, err := s.ApproveContract(ctx, c.ContractID, "legal-lead-1", "dec-001")
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if approved.Status != domain.ContractStatusApproved {
		t.Fatalf("status after approve = %s, want APPROVED", approved.Status)
	}

	sent, err := s.SendForSignature(ctx, c.ContractID, "legal-lead-1")
	if err != nil {
		t.Fatalf("send for signature: %v", err)
	}
	if sent.SignatureStatus != domain.SignatureStatusSent {
		t.Fatalf("signature_status after send = %s, want SENT", sent.SignatureStatus)
	}
	if sent.Status != domain.ContractStatusApproved {
		t.Fatalf("lifecycle status changed by SendForSignature: %s, want still APPROVED (orthogonal dimensions)", sent.Status)
	}

	executed, err := s.RecordExecution(ctx, c.ContractID, &domain.RecordExecutionRequest{SignedBy: "counterparty-signer"})
	if err != nil {
		t.Fatalf("record execution: %v", err)
	}
	if executed.Status != domain.ContractStatusEffective {
		t.Fatalf("status after execution = %s, want EFFECTIVE", executed.Status)
	}
	if executed.SignatureStatus != domain.SignatureStatusCompleted {
		t.Fatalf("signature_status after execution = %s, want COMPLETED", executed.SignatureStatus)
	}
}

// An approved-for-signature version is immutable (LEG-05 §7.1): CreateVersion
// (UpdateContract) must refuse once the contract has left DRAFT/REVIEW.
func TestPgStore_UpdateContract_RefusedOnceApproved(t *testing.T) {
	pool := openTestPool(t)
	s := store.NewPgStore(pool)
	ctx := tenantCtx("tenant-a")

	c := newContract("le-us", "drafter-1")
	if err := s.CreateContract(ctx, c); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.SubmitReview(ctx, c.ContractID, "drafter-1"); err != nil {
		t.Fatalf("submit review: %v", err)
	}
	if _, err := s.ApproveContract(ctx, c.ContractID, "legal-lead-1", "dec-001"); err != nil {
		t.Fatalf("approve: %v", err)
	}

	c.Title = "Materially different terms"
	if err := s.UpdateContract(ctx, c, "sneaking in a change"); !errors.Is(err, domain.ErrWrongLifecycleStatus) {
		t.Fatalf("update after APPROVED returned %v, want ErrWrongLifecycleStatus", err)
	}
}

// Segregation of duties is re-checked against the locked row.
func TestPgStore_ApproveContract_SelfApprovalIsRefusedAtTheWrite(t *testing.T) {
	pool := openTestPool(t)
	s := store.NewPgStore(pool)
	ctx := tenantCtx("tenant-a")

	c := newContract("le-us", "drafter-1")
	if err := s.CreateContract(ctx, c); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.SubmitReview(ctx, c.ContractID, "drafter-1"); err != nil {
		t.Fatalf("submit review: %v", err)
	}
	if _, err := s.ApproveContract(ctx, c.ContractID, "drafter-1", "dec-001"); !errors.Is(err, domain.ErrSelfApprovalNotAllowed) {
		t.Fatalf("self-approve returned %v, want ErrSelfApprovalNotAllowed", err)
	}
}

// RecordExecution before SendForSignature is refused.
func TestPgStore_RecordExecution_RefusedWithoutSignatureSent(t *testing.T) {
	pool := openTestPool(t)
	s := store.NewPgStore(pool)
	ctx := tenantCtx("tenant-a")

	c := newContract("le-us", "drafter-1")
	if err := s.CreateContract(ctx, c); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.SubmitReview(ctx, c.ContractID, "drafter-1"); err != nil {
		t.Fatalf("submit review: %v", err)
	}
	if _, err := s.ApproveContract(ctx, c.ContractID, "legal-lead-1", "dec-001"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if _, err := s.RecordExecution(ctx, c.ContractID, &domain.RecordExecutionRequest{SignedBy: "x"}); !errors.Is(err, domain.ErrSignatureNotSent) {
		t.Fatalf("record execution without signature sent returned %v, want ErrSignatureNotSent", err)
	}
}

// Amend and Renew keep the contract EFFECTIVE while recording a new version
// — neither is a status value (see migration 000003's doc comment).
func TestPgStore_AmendAndRenew_StayEffective(t *testing.T) {
	pool := openTestPool(t)
	s := store.NewPgStore(pool)
	ctx := tenantCtx("tenant-a")

	c := newContract("le-us", "drafter-1")
	if err := s.CreateContract(ctx, c); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.SubmitReview(ctx, c.ContractID, "drafter-1"); err != nil {
		t.Fatalf("submit review: %v", err)
	}
	if _, err := s.ApproveContract(ctx, c.ContractID, "legal-lead-1", "dec-001"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if _, err := s.SendForSignature(ctx, c.ContractID, "legal-lead-1"); err != nil {
		t.Fatalf("send for signature: %v", err)
	}
	if _, err := s.RecordExecution(ctx, c.ContractID, &domain.RecordExecutionRequest{SignedBy: "counterparty-signer"}); err != nil {
		t.Fatalf("record execution: %v", err)
	}

	amended, err := s.AmendContract(ctx, c.ContractID, &domain.AmendContractRequest{AmendedBy: "legal-lead-1", TotalValue: 75000, ChangeSummary: "scope increase"})
	if err != nil {
		t.Fatalf("amend: %v", err)
	}
	if amended.Status != domain.ContractStatusEffective {
		t.Fatalf("status after amend = %s, want still EFFECTIVE", amended.Status)
	}
	if amended.TotalValue != 75000 {
		t.Fatalf("total_value after amend = %v, want 75000", amended.TotalValue)
	}

	renewed, err := s.RenewContract(ctx, c.ContractID, &domain.RenewContractRequest{RenewedBy: "legal-lead-1", NewEffectiveTo: "2027-01-01"})
	if err != nil {
		t.Fatalf("renew: %v", err)
	}
	if renewed.Status != domain.ContractStatusEffective {
		t.Fatalf("status after renew = %s, want still EFFECTIVE", renewed.Status)
	}
	if renewed.EffectiveTo == nil || *renewed.EffectiveTo != "2027-01-01" {
		t.Fatalf("effective_to after renew = %v, want 2027-01-01", renewed.EffectiveTo)
	}

	versions, err := s.ListContractVersions(ctx, c.ContractID)
	if err != nil {
		t.Fatalf("list versions: %v", err)
	}
	// Initial draft, approved, executed, amended, renewed = 5 snapshots.
	if len(versions) != 5 {
		t.Fatalf("expected 5 version snapshots, got %d", len(versions))
	}
}

// A DRAFT or REVIEW contract has nothing to terminate.
func TestPgStore_TerminateContract_RefusedBeforeApproved(t *testing.T) {
	pool := openTestPool(t)
	s := store.NewPgStore(pool)
	ctx := tenantCtx("tenant-a")

	c := newContract("le-us", "drafter-1")
	if err := s.CreateContract(ctx, c); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.TerminateContract(ctx, c.ContractID, &domain.TerminateContractRequest{TerminatedBy: "x", TerminationNote: "n/a"}); !errors.Is(err, domain.ErrWrongLifecycleStatus) {
		t.Fatalf("terminate a DRAFT contract returned %v, want ErrWrongLifecycleStatus", err)
	}
}

// A write must not be able to conclude another tenant's contract, and the
// policy must actually apply — FORCE ROW LEVEL SECURITY, added in migration
// 000003, is what makes that true for this superuser connection.
func TestPgStore_Transitions_AreTenantScoped(t *testing.T) {
	pool := openTestPool(t)
	s := store.NewPgStore(pool)

	c := newContract("le-us", "drafter-1")
	if err := s.CreateContract(tenantCtx("tenant-a"), c); err != nil {
		t.Fatalf("create: %v", err)
	}

	if _, err := s.SubmitReview(tenantCtx("tenant-b"), c.ContractID, "drafter-1"); !errors.Is(err, domain.ErrContractNotFound) {
		t.Fatalf("cross-tenant submit returned %v, want ErrContractNotFound", err)
	}
	if _, err := s.GetContract(tenantCtx("tenant-b"), c.ContractID); !errors.Is(err, domain.ErrContractNotFound) {
		t.Fatalf("cross-tenant read returned %v, want ErrContractNotFound", err)
	}
}
