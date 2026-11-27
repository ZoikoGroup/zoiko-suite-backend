package store_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"
	"zoiko.io/compliance-risk-scoring-svc/internal/domain"
	"zoiko.io/compliance-risk-scoring-svc/internal/outbox"
	"zoiko.io/compliance-risk-scoring-svc/internal/store"
)

func connect(t *testing.T) (*store.Store, *pgxpool.Pool) {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping Postgres integration test")
	}
	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return store.New(pool), pool
}

// requireRLSRole: a superuser or BYPASSRLS role sees every row regardless
// of policy, which would make an isolation test pass vacuously.
func requireRLSRole(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	var super, bypass bool
	if err := pool.QueryRow(context.Background(),
		`SELECT rolsuper, rolbypassrls FROM pg_roles WHERE rolname = current_user`).Scan(&super, &bypass); err != nil {
		t.Fatal(err)
	}
	if super || bypass {
		t.Skip("TEST_DATABASE_URL role is SUPERUSER or BYPASSRLS; RLS isolation cannot be proven with it")
	}
}

func sampleAssessment(tenantID string) *domain.RiskScoreAssessment {
	now := time.Now().UTC().Truncate(time.Microsecond)
	entity := "LE-" + uuid.NewString()[:8]
	subject := domain.ScoreSubject{LegalEntityID: entity, Type: domain.SubjectLegalEntity, ID: entity}
	inputs := domain.ScoringInputs{OpenObligationsCount: 2}
	thresholds := domain.ScoringThresholds{Composite: domain.AppliedThresholds{
		Source: domain.ThresholdSourcePlatformDefault, Cutoffs: domain.CurrentModel.DefaultBands}}
	result := domain.ComputeScore(domain.CurrentModel, subject, inputs, thresholds)
	a := &domain.RiskScoreAssessment{
		ID: uuid.NewString(), TenantID: tenantID, LegalEntityID: entity, SubjectType: subject.Type, SubjectID: subject.ID,
		AssessmentName: "rls", OpenObligationsCount: 2, CompositeRiskScore: result.CompositeScore, RiskTier: result.Tier,
		ModelVersion: result.ModelVersion, InputHash: result.InputHash, ThresholdsApplied: thresholds.Composite,
		BandRationale: result.BandRationale, AssuranceState: domain.AssuranceAdvisory, ReviewStatus: domain.ReviewNotRequired,
		Status: domain.ScoreActive, SourceReferences: []string{}, CalculatedBy: "p1", Version: 1,
		EvaluatedAt: now, CreatedAt: now, UpdatedAt: now,
	}
	for _, f := range result.Factors {
		a.FactorBreakdowns = append(a.FactorBreakdowns, domain.FactorBreakdown{
			ID: uuid.NewString(), RiskCategory: f.Category, InputCount: f.InputCount, SaturationCount: f.SaturationCount,
			CategoryWeight: f.Weight, RawScore: f.RawScore, WeightedScore: f.WeightedScore, RiskDriverSummary: f.DriverSummary,
			CreatedAt: now,
		})
	}
	return a
}

func TestRLSIsolatesTenants(t *testing.T) {
	s, pool := connect(t)
	requireRLSRole(t, pool)
	ctx := context.Background()
	tenantA, tenantB := "tenant-"+uuid.NewString(), "tenant-"+uuid.NewString()

	a := sampleAssessment(tenantA)
	if err := s.InTenantTx(ctx, tenantA, func(q *store.TenantQueries) error { return q.InsertAssessment(ctx, a) }); err != nil {
		t.Fatalf("insert as owner: %v", err)
	}

	// The store's own query for tenant B cannot see it.
	err := s.InTenantTx(ctx, tenantB, func(q *store.TenantQueries) error {
		_, err := q.GetAssessment(ctx, a.ID, false)
		return err
	})
	if err != store.ErrNotFound {
		t.Fatalf("tenant B must not see tenant A's score, got err=%v", err)
	}

	// Raw SQL with NO tenant predicate: only RLS stands between the
	// tenants. Scoped to B -> 0 rows; no tenant set at all -> 0 rows.
	for _, scope := range []string{tenantB, ""} {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", scope); err != nil {
			t.Fatal(err)
		}
		for _, table := range []string{"risk_score_assessments", "risk_factor_breakdowns", "risk_outbox_events", "risk_idempotency_records"} {
			var n int
			if err := tx.QueryRow(ctx, "SELECT COUNT(*) FROM "+table+" WHERE tenant_id = $1", tenantA).Scan(&n); err != nil {
				t.Fatalf("%s: %v", table, err)
			}
			if n != 0 {
				t.Fatalf("scope %q saw %d of tenant A's rows in %s", scope, n, table)
			}
		}
		_ = tx.Rollback(ctx)
	}

	// WITH CHECK: a transaction scoped to B cannot write a row labelled A.
	smuggled := sampleAssessment(tenantA)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantB); err != nil {
		t.Fatal(err)
	}
	_, err = tx.Exec(ctx, `INSERT INTO risk_threshold_rules (id, tenant_id, rule_name, risk_category, moderate_threshold,
		high_threshold, critical_threshold, status, effective_from, created_by, updated_by)
		VALUES ($1, $2, 'x', 'COMPOSITE', 10, 20, 30, 'DRAFT', CURRENT_DATE, 'p', 'p')`, smuggled.ID, tenantA)
	if err == nil || !strings.Contains(err.Error(), "row-level security") {
		t.Fatalf("cross-tenant insert must be rejected by RLS WITH CHECK, got %v", err)
	}
}

func TestScoresCannotBeDeleted(t *testing.T) {
	s, pool := connect(t)
	ctx := context.Background()
	tenantID := "tenant-" + uuid.NewString()
	a := sampleAssessment(tenantID)
	if err := s.InTenantTx(ctx, tenantID, func(q *store.TenantQueries) error { return q.InsertAssessment(ctx, a) }); err != nil {
		t.Fatal(err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	_, _ = tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID)
	if _, err := tx.Exec(ctx, "DELETE FROM risk_factor_breakdowns WHERE assessment_id = $1", a.ID); err == nil {
		t.Fatal("factor breakdowns must be immutable (delete blocked by trigger)")
	}
}

type recordingWriter struct {
	tenants []string
	fail    bool
}

func (w *recordingWriter) WriteEnvelope(_ context.Context, tenantID string, _ []byte) error {
	if w.fail {
		return context.DeadlineExceeded
	}
	w.tenants = append(w.tenants, tenantID)
	return nil
}

// TestOutboxRelayDeliversAcrossTenants: the relay (app.outbox_relay) sees
// every tenant's events under FORCE RLS, marks them published, and a failed
// publish leaves the event pending for retry.
func TestOutboxRelayDeliversAcrossTenants(t *testing.T) {
	s, _ := connect(t)
	ctx := context.Background()
	tenants := []string{"tenant-" + uuid.NewString(), "tenant-" + uuid.NewString()}
	for _, tenantID := range tenants {
		err := s.InTenantTx(ctx, tenantID, func(q *store.TenantQueries) error {
			return q.EnqueueEvent(ctx, store.OutboxEvent{ID: uuid.NewString(), EventType: "risk_score.calculated",
				AggregateType: "RiskScoreAssessment", AggregateID: uuid.NewString(), Envelope: []byte(`{}`), CreatedAt: time.Now()})
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	failing := outbox.NewRelay(s, &recordingWriter{fail: true}, zap.NewNop(), time.Second)
	if _, err := failing.RelayOnce(ctx); err != nil {
		t.Fatal(err)
	}
	for _, tenantID := range tenants {
		if n, _ := s.CountPendingOutboxEvents(ctx, tenantID); n != 1 {
			t.Fatalf("failed publish must leave the event pending; tenant %s pending=%d", tenantID, n)
		}
	}

	// Retry is backed off (next_attempt_at); drain until our events are out.
	writer := &recordingWriter{}
	relay := outbox.NewRelay(s, writer, zap.NewNop(), time.Second)
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := relay.RelayOnce(ctx); err != nil {
			t.Fatal(err)
		}
		a, _ := s.CountPendingOutboxEvents(ctx, tenants[0])
		b, _ := s.CountPendingOutboxEvents(ctx, tenants[1])
		if a == 0 && b == 0 {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("relay did not deliver both tenants' events; delivered to %v", writer.tenants)
}
