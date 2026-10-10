package store

import (
	"context"
	"encoding/json"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/payment-run-svc/internal/domain"
	"zoiko.io/payment-run-svc/internal/middleware"
	"zoiko.io/payment-run-svc/internal/outbox"
)

// These tests need a fully migrated database (TEST_DATABASE_URL). Run them
// both as the owner and as a NOSUPERUSER NOBYPASSRLS role: row-level security
// only bites for the latter, and it is the role the service runs as in compose.

func openStore(t *testing.T) (*PgStore, *pgxpool.Pool) {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("Skipping Postgres integration test: TEST_DATABASE_URL not set")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return NewPgStore(pool, zap.NewNop()), pool
}

// settledInstruction creates a one-payee run (net 90, withheld 10) and settles
// its instruction, returning the ids.
func settledInstruction(t *testing.T, s *PgStore, tenant string) (runID, instructionID string) {
	t.Helper()
	ctx := middleware.WithTenant(context.Background(), tenant)
	run, ins, err := s.CreateRun(ctx, tenant, domain.CreateRunRequest{
		LegalEntityID: uuid.NewString(), PayingBankAccountRef: "acct", Currency: "USD", ValueDate: time.Now(), PaymentMethod: "ACH",
	}, []domain.RunInstruction{{
		AuthorizationID: "auth-" + uuid.NewString(), AuthorizationFingerprint: "fp", PayeeRef: "payee", NetAmount: 90, Currency: "USD",
		Payables: []domain.InstructionPayable{{PayableSource: "AP_INVOICE", SourceReference: "inv-" + uuid.NewString(), GrossAmount: 100, WithholdingAmount: 10, NetAmount: 90}},
	}}, "p1")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if _, applied, err := s.ReconcileInstruction(ctx, domain.ReconcileInstructionRequest{
		InstructionID: ins[0].InstructionID, ExternalStatus: domain.InstructionSettled, ProviderEventRef: "bnk07:" + ins[0].InstructionID + ":SETTLED",
	}, "p1"); err != nil || !applied {
		t.Fatalf("settle: applied=%v err=%v", applied, err)
	}
	return run.RunID, ins[0].InstructionID
}

func TestSettlement_RecordsBalancedPostingRequestAndOutboxEvent_Once(t *testing.T) {
	s, pool := openStore(t)
	tenant := uuid.NewString()
	ctx := middleware.WithTenant(context.Background(), tenant)
	runID, insID := settledInstruction(t, s, tenant)

	reqs, err := s.ListAccountingRequests(ctx, runID)
	if err != nil || len(reqs) != 1 {
		t.Fatalf("expected one posting request, got %d (%v)", len(reqs), err)
	}
	if reqs[0].Status != "PENDING" || reqs[0].SourceEventID != "ap11:settle:"+insID {
		t.Fatalf("unexpected request %+v", reqs[0])
	}

	// The posting is balanced: Dr payable control 100 = Cr clearing 90 + Cr withholding 10.
	var payload []byte
	if err := s.withTenant(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT request_payload FROM accounting_posting_requests WHERE instruction_id = $1`, insID).Scan(&payload)
	}); err != nil {
		t.Fatal(err)
	}
	var p postingRequest
	if err := json.Unmarshal(payload, &p); err != nil {
		t.Fatal(err)
	}
	var dr, cr float64
	for _, l := range p.Lines {
		dr += l.DebitAmount
		cr += l.CreditAmount
	}
	if dr != 100 || cr != 100 || len(p.Lines) != 3 {
		t.Fatalf("expected a balanced 3-line posting of 100, got dr=%v cr=%v lines=%+v", dr, cr, p.Lines)
	}

	// A different provider event ref for the same settlement still requests nothing twice.
	if _, _, err := s.ReconcileInstruction(ctx, domain.ReconcileInstructionRequest{
		InstructionID: insID, ExternalStatus: domain.InstructionSettled, ProviderEventRef: "bnk07:other-ref",
	}, "p1"); err != nil {
		t.Fatal(err)
	}
	if reqs, _ = s.ListAccountingRequests(ctx, runID); len(reqs) != 1 {
		t.Fatalf("a replayed settlement must not request a second posting, got %d", len(reqs))
	}

	// The instruction event was written to the outbox in the same transaction.
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM outbox_events WHERE aggregate_id = $1 AND event_type = $2`, insID, domain.EventInstructionSettled).Scan(&n); err != nil || n < 1 {
		t.Fatalf("expected a settled event in the outbox, got %d (%v)", n, err)
	}
}

func TestAccountingRequests_TenantIsolation(t *testing.T) {
	s, _ := openStore(t)
	tenantA, tenantB := uuid.NewString(), uuid.NewString()
	runA, _ := settledInstruction(t, s, tenantA)

	got, err := s.ListAccountingRequests(middleware.WithTenant(context.Background(), tenantB), runA)
	if err != nil || len(got) != 0 {
		t.Fatalf("tenant B must not see tenant A's posting requests (RLS only bites as a restricted role), got %d (%v)", len(got), err)
	}
	if n, err := s.RequeueAccountingRequests(middleware.WithTenant(context.Background(), tenantB), runA); err != nil || n != 0 {
		t.Fatalf("tenant B must not requeue tenant A's requests, got %d (%v)", n, err)
	}
}

// The dispatcher has no request tenant. It must still claim requests from every
// tenant (this is what fails under NOBYPASSRLS without migration 000008), post
// each once, and never re-post a POSTED request.
func TestDispatchPending_CrossTenant_PostsOnce_AndHandlesFailures(t *testing.T) {
	s, _ := openStore(t)
	tenantA, tenantB := uuid.NewString(), uuid.NewString()
	runA, insA := settledInstruction(t, s, tenantA)
	runB, insB := settledInstruction(t, s, tenantB)
	srcA, srcB := "ap11:settle:"+insA, "ap11:settle:"+insB

	var mu sync.Mutex
	calls := map[string]int{}
	post := func(_ context.Context, r PostingRequest) PostingResult {
		mu.Lock()
		defer mu.Unlock()
		switch r.SourceEventID {
		case srcA:
			calls[srcA]++
			return PostingResult{Final: true, Posted: true, ExecutionID: "exec-A"}
		case srcB:
			calls[srcB]++
			return PostingResult{Final: true, Err: "ledger refused (422): no ACC-02 mapping"}
		default:
			return PostingResult{Err: "not mine"} // another test's leftover row: transient, retried later
		}
	}
	if _, err := s.DispatchPending(context.Background(), 1000, post); err != nil {
		t.Fatalf("DispatchPending: %v", err)
	}

	ra, _ := s.ListAccountingRequests(middleware.WithTenant(context.Background(), tenantA), runA)
	rb, _ := s.ListAccountingRequests(middleware.WithTenant(context.Background(), tenantB), runB)
	if len(ra) != 1 || ra[0].Status != "POSTED" || ra[0].PostingExecutionID != "exec-A" {
		t.Fatalf("tenant A's request should be POSTED, got %+v", ra)
	}
	if len(rb) != 1 || rb[0].Status != "QUARANTINED" || rb[0].LastError == "" {
		t.Fatalf("tenant B's refused request should be QUARANTINED with the reason, got %+v", rb)
	}

	// A second pass never posts A again and never touches B (not PENDING).
	if _, err := s.DispatchPending(context.Background(), 1000, post); err != nil {
		t.Fatal(err)
	}
	if calls[srcA] != 1 || calls[srcB] != 1 {
		t.Fatalf("each request must be posted exactly once, got %v", calls)
	}

	// Requeue returns QUARANTINED to PENDING, leaves POSTED alone, and the
	// dispatcher then posts it.
	ctxA := middleware.WithTenant(context.Background(), tenantA)
	if n, err := s.RequeueAccountingRequests(ctxA, runA); err != nil || n != 0 {
		t.Fatalf("a POSTED request must never be requeued, got %d (%v)", n, err)
	}
	ctxB := middleware.WithTenant(context.Background(), tenantB)
	if n, err := s.RequeueAccountingRequests(ctxB, runB); err != nil || n != 1 {
		t.Fatalf("expected 1 requeued, got %d (%v)", n, err)
	}
	post2 := func(_ context.Context, r PostingRequest) PostingResult {
		if r.SourceEventID == srcB {
			return PostingResult{Final: true, Posted: true, ExecutionID: "exec-B"}
		}
		return PostingResult{Err: "not mine"}
	}
	if _, err := s.DispatchPending(context.Background(), 1000, post2); err != nil {
		t.Fatal(err)
	}
	if rb, _ = s.ListAccountingRequests(ctxB, runB); len(rb) != 1 || rb[0].Status != "POSTED" {
		t.Fatalf("requeued request should now be POSTED, got %+v", rb)
	}
}

func TestDispatchPending_TransientFailure_CountsAttemptAndStaysPending(t *testing.T) {
	s, _ := openStore(t)
	tenant := uuid.NewString()
	runID, insID := settledInstruction(t, s, tenant)
	src := "ap11:settle:" + insID

	if _, err := s.DispatchPending(context.Background(), 1000, func(_ context.Context, r PostingRequest) PostingResult {
		return PostingResult{Err: "ledger answered 503"}
	}); err != nil {
		t.Fatal(err)
	}
	got, _ := s.ListAccountingRequests(middleware.WithTenant(context.Background(), tenant), runID)
	if len(got) != 1 || got[0].SourceEventID != src || got[0].Status != "PENDING" || got[0].Attempts != 1 {
		t.Fatalf("a transient failure must count an attempt and stay PENDING, got %+v", got)
	}
}

// The relay publishes unpublished outbox rows and marks them published.
func TestOutboxRelay_PublishesAndMarksPublished(t *testing.T) {
	s, pool := openStore(t)
	tenant := uuid.NewString()
	_, insID := settledInstruction(t, s, tenant)

	pub := &recordingPublisher{}
	outbox.NewRelay(pool, pub, time.Second, 1000, zap.NewNop()).RelayOnce(context.Background())

	if !pub.saw(insID) {
		t.Fatalf("relay did not publish the settled instruction's event; published %d events", len(pub.ids))
	}
	var unpublished int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM outbox_events WHERE aggregate_id = $1 AND published_at IS NULL`, insID).Scan(&unpublished); err != nil || unpublished != 0 {
		t.Fatalf("published rows must be marked published, %d still pending (%v)", unpublished, err)
	}
}

type recordingPublisher struct {
	mu  sync.Mutex
	ids []string
}

func (p *recordingPublisher) PublishOutbox(_ context.Context, _ string, aggregateID string, _ []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ids = append(p.ids, aggregateID)
	return nil
}

func (p *recordingPublisher) saw(id string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, v := range p.ids {
		if v == id {
			return true
		}
	}
	return false
}
