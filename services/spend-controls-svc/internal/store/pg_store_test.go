package store_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/spend-controls-svc/internal/domain"
	svcmiddleware "zoiko.io/spend-controls-svc/internal/middleware"
	"zoiko.io/spend-controls-svc/internal/store"
)

// This service shipped with no store tests at all, which is why the
// non-atomic threshold check survived: nothing exercised two concurrent spend
// checks against one policy, and a single-threaded test passes happily.
//
// WARNING: this suite DROPs its tables. Point TEST_DATABASE_URL at the
// spend_controls database a running service uses and it deletes that data, which
// afterwards looks like a service bug rather than a test. The name is checked
// before anything is dropped.
func openTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("Skipping Postgres integration test: TEST_DATABASE_URL not set")
	}
	if !strings.Contains(dsn, "_test") {
		t.Fatal(`refusing to run: TEST_DATABASE_URL must name a database containing "_test" ` +
			`because this suite DROPs its tables. Use spend_controls_test, not spend_controls.`)
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("failed to connect to postgres: %v", err)
	}
	t.Cleanup(pool.Close)

	_, filename, _, _ := runtime.Caller(0)
	base := filepath.Dir(filename)

	// consumptions first: it has an FK onto policies.
	_, _ = pool.Exec(ctx, `DROP TABLE IF EXISTS spend_consumptions CASCADE;`)
	_, _ = pool.Exec(ctx, `DROP TABLE IF EXISTS spend_policies CASCADE;`)

	sql, err := os.ReadFile(filepath.Join(base, "../../deployments/migrations/000001_initial_schema.up.sql"))
	if err != nil {
		t.Fatalf("failed to read migration: %v", err)
	}
	if _, err := pool.Exec(ctx, string(sql)); err != nil {
		t.Fatalf("failed to apply migration: %v", err)
	}

	return pool
}

func seedPolicy(t *testing.T, s *store.PgStore, ctx context.Context, tenantID, entity, category, period, currency string, threshold float64) *domain.SpendPolicy {
	t.Helper()
	now := time.Now().UTC()
	p := &domain.SpendPolicy{
		SpendPolicyID:        uuid.NewString(),
		TenantID:             tenantID,
		LegalEntityID:        entity,
		Category:             category,
		Period:               period,
		ThresholdAmount:      threshold,
		CurrencyCode:         currency,
		ActiveFlag:           true,
		CreatedByPrincipalID: "test-admin",
		CreatedAt:            now,
		UpdatedAt:            now,
	}
	if _, err := s.CreatePolicy(ctx, p); err != nil {
		t.Fatalf("CreatePolicy failed: %v", err)
	}
	return p
}

func evaluation(entity, category, currency, correlationID string, amount float64) domain.SpendEvaluation {
	return domain.SpendEvaluation{
		LegalEntityID: entity,
		Category:      category,
		Amount:        amount,
		CurrencyCode:  currency,
		CorrelationID: correlationID,
		PrincipalID:   "principal-1",
	}
}

// THE test this fix exists for.
//
// Ten spend checks are fired simultaneously against a policy whose threshold
// admits exactly three of them. Before the fix each check summed consumption in
// its own transaction, so all ten read a prior total of 0, all ten concluded they
// fit, and all ten were recorded — the threshold was exceeded more than
// threefold. Now the policy row is locked for the duration of each evaluation, so
// they serialise and the total can never pass the threshold.
func TestEvaluateSpend_ConcurrentChecksCannotOverspend(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantID := uuid.NewString()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	const (
		threshold = 300.0
		amount    = 100.0
		attempts  = 10
	)
	policy := seedPolicy(t, s, ctx, tenantID, "le-1", "PROCUREMENT", "MONTHLY", "GBP", threshold)

	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		allowed  int
		blocked  int
		failures []error
	)
	start := make(chan struct{})

	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // release them together, to maximise the overlap
			d, err := s.EvaluateSpend(ctx, evaluation("le-1", "PROCUREMENT", "GBP", uuid.NewString(), amount))
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				failures = append(failures, err)
				return
			}
			if d.Outcome == "ALLOWED" {
				allowed++
			} else {
				blocked++
			}
		}(i)
	}
	close(start)
	wg.Wait()

	for _, err := range failures {
		t.Errorf("concurrent evaluation failed: %v", err)
	}

	// The invariant that matters: recorded ALLOWED spend never exceeds the
	// threshold. Asserted against the database, not the counters.
	total, err := s.SumConsumption(ctx, policy.SpendPolicyID, "GBP", time.Time{})
	if err != nil {
		t.Fatalf("SumConsumption failed: %v", err)
	}
	if total > threshold {
		t.Fatalf("threshold overspent: %v recorded against a threshold of %v (%d allowed of %d concurrent attempts)",
			total, threshold, allowed, attempts)
	}
	if allowed != int(threshold/amount) {
		t.Errorf("expected exactly %d of %d concurrent checks to be allowed, got %d (blocked %d)",
			int(threshold/amount), attempts, allowed, blocked)
	}
}

// A blocked attempt is recorded so the refusal is auditable, but must never
// consume the budget it was refused.
func TestEvaluateSpend_BlockedIsRecordedButNotCounted(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantID := uuid.NewString()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	policy := seedPolicy(t, s, ctx, tenantID, "le-1", "PROCUREMENT", "MONTHLY", "GBP", 100)

	d, err := s.EvaluateSpend(ctx, evaluation("le-1", "PROCUREMENT", "GBP", uuid.NewString(), 5000))
	if err != nil {
		t.Fatalf("EvaluateSpend failed: %v", err)
	}
	if d.Outcome != "BLOCKED" {
		t.Fatalf("expected BLOCKED, got %s", d.Outcome)
	}
	if d.ConsumptionID == "" {
		t.Fatal("a refusal must leave a queryable record")
	}

	rows, err := s.ListConsumptions(ctx, "le-1", policy.SpendPolicyID)
	if err != nil {
		t.Fatalf("ListConsumptions failed: %v", err)
	}
	if len(rows) != 1 || rows[0].DecisionOutcome != "BLOCKED" {
		t.Fatalf("expected one BLOCKED row, got %+v", rows)
	}

	total, err := s.SumConsumption(ctx, policy.SpendPolicyID, "GBP", time.Time{})
	if err != nil {
		t.Fatalf("SumConsumption failed: %v", err)
	}
	if total != 0 {
		t.Fatalf("a blocked attempt must not count toward the total, got %v", total)
	}
}

// Nothing in this platform holds an FX rate, so a check in another currency
// cannot be compared with the policy's threshold.
func TestEvaluateSpend_CurrencyMismatch_Refused(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantID := uuid.NewString()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	seedPolicy(t, s, ctx, tenantID, "le-1", "PROCUREMENT", "MONTHLY", "GBP", 10000)

	_, err := s.EvaluateSpend(ctx, evaluation("le-1", "PROCUREMENT", "USD", uuid.NewString(), 9000))
	if !errors.Is(err, domain.ErrCurrencyMismatch) {
		t.Fatalf("expected ErrCurrencyMismatch, got %v", err)
	}
}

// The running total must only add amounts in the policy's own currency —
// summing mixed currencies produces a number that is not money in any of them.
func TestSumConsumption_IgnoresOtherCurrencies(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantID := uuid.NewString()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	gbp := seedPolicy(t, s, ctx, tenantID, "le-1", "PROCUREMENT", "MONTHLY", "GBP", 10000)
	// A second policy, same entity but a different category, denominated in USD.
	usd := seedPolicy(t, s, ctx, tenantID, "le-1", "TRAVEL", "MONTHLY", "USD", 10000)

	if _, err := s.EvaluateSpend(ctx, evaluation("le-1", "PROCUREMENT", "GBP", uuid.NewString(), 100)); err != nil {
		t.Fatalf("GBP check failed: %v", err)
	}
	if _, err := s.EvaluateSpend(ctx, evaluation("le-1", "TRAVEL", "USD", uuid.NewString(), 7000)); err != nil {
		t.Fatalf("USD check failed: %v", err)
	}

	gbpTotal, err := s.SumConsumption(ctx, gbp.SpendPolicyID, "GBP", time.Time{})
	if err != nil {
		t.Fatalf("SumConsumption failed: %v", err)
	}
	if gbpTotal != 100 {
		t.Fatalf("expected the GBP total to be 100, got %v", gbpTotal)
	}
	usdTotal, err := s.SumConsumption(ctx, usd.SpendPolicyID, "USD", time.Time{})
	if err != nil {
		t.Fatalf("SumConsumption failed: %v", err)
	}
	if usdTotal != 7000 {
		t.Fatalf("expected the USD total to be 7000, got %v", usdTotal)
	}
}

// A retry replays the stored decision instead of evaluating again, so the same
// spend is never booked twice.
func TestEvaluateSpend_ReplayIsIdempotent(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantID := uuid.NewString()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	policy := seedPolicy(t, s, ctx, tenantID, "le-1", "PROCUREMENT", "MONTHLY", "GBP", 10000)

	corr := uuid.NewString()
	first, err := s.EvaluateSpend(ctx, evaluation("le-1", "PROCUREMENT", "GBP", corr, 2000))
	if err != nil {
		t.Fatalf("first evaluation failed: %v", err)
	}
	second, err := s.EvaluateSpend(ctx, evaluation("le-1", "PROCUREMENT", "GBP", corr, 2000))
	if err != nil {
		t.Fatalf("replay failed: %v", err)
	}

	if !second.Replayed {
		t.Error("expected the retry to be flagged as a replay")
	}
	if second.ConsumptionID != first.ConsumptionID {
		t.Errorf("replay resolved to a different record: %s vs %s", second.ConsumptionID, first.ConsumptionID)
	}
	if second.Policy == nil {
		t.Error("a replayed decision must still report the policy it was judged against")
	}

	total, err := s.SumConsumption(ctx, policy.SpendPolicyID, "GBP", time.Time{})
	if err != nil {
		t.Fatalf("SumConsumption failed: %v", err)
	}
	if total != 2000 {
		t.Fatalf("a replay must not book the spend twice, total is %v", total)
	}
}

// PER_TRANSACTION judges each spend on its own, so history is irrelevant.
func TestEvaluateSpend_PerTransactionIgnoresHistory(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantID := uuid.NewString()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	seedPolicy(t, s, ctx, tenantID, "le-1", "PROCUREMENT", "PER_TRANSACTION", "GBP", 500)

	for i := 0; i < 3; i++ {
		d, err := s.EvaluateSpend(ctx, evaluation("le-1", "PROCUREMENT", "GBP", uuid.NewString(), 400))
		if err != nil {
			t.Fatalf("evaluation %d failed: %v", i, err)
		}
		if d.Outcome != "ALLOWED" {
			t.Fatalf("attempt %d: each transaction is judged alone, expected ALLOWED, got %s", i, d.Outcome)
		}
		if d.PriorConsumption != 0 {
			t.Fatalf("attempt %d: PER_TRANSACTION must not accumulate, got prior=%v", i, d.PriorConsumption)
		}
	}
}

// No policy is a distinct outcome from a policy that permits: nothing is
// constraining the spend, and nothing is recorded because there is no budget.
func TestEvaluateSpend_NoPolicy(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantID := uuid.NewString()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	d, err := s.EvaluateSpend(ctx, evaluation("le-1", "NOTHING_CONFIGURED", "GBP", uuid.NewString(), 999))
	if err != nil {
		t.Fatalf("EvaluateSpend failed: %v", err)
	}
	if d.Outcome != "ALLOWED" || d.Basis != "no_policy_configured" {
		t.Fatalf("expected ALLOWED/no_policy_configured, got %s/%s", d.Outcome, d.Basis)
	}
	if d.ConsumptionID != "" {
		t.Error("with no policy there is no budget to consume, so nothing should be recorded")
	}
}

// active_flag was written TRUE on create and never changed by anything, so two
// limits for the same category both read as active while only the newest was ever
// enforced. The supersede happens in the same transaction as the insert, so
// concurrent writers cannot leave zero or two.
func TestCreatePolicy_SupersedesPriorLimitAtomically(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantID := uuid.NewString()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	seedPolicy(t, s, ctx, tenantID, "le-1", "PROCUREMENT", "MONTHLY", "GBP", 100)
	newer := seedPolicy(t, s, ctx, tenantID, "le-1", "PROCUREMENT", "MONTHLY", "GBP", 900)

	active, err := s.ListPolicies(ctx, "le-1", "PROCUREMENT", true)
	if err != nil {
		t.Fatalf("ListPolicies failed: %v", err)
	}
	if len(active) != 1 {
		t.Fatalf("expected exactly one limit in force, got %d", len(active))
	}
	if active[0].SpendPolicyID != newer.SpendPolicyID {
		t.Error("the newest limit should be the one in force")
	}

	// The superseded row is kept, so the history stays answerable.
	all, err := s.ListPolicies(ctx, "le-1", "PROCUREMENT", false)
	if err != nil {
		t.Fatalf("ListPolicies failed: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("expected the superseded row to be kept, got %d rows", len(all))
	}

	// A different category is untouched by the supersede.
	seedPolicy(t, s, ctx, tenantID, "le-1", "TRAVEL", "MONTHLY", "GBP", 50)
	travel, err := s.ListPolicies(ctx, "le-1", "TRAVEL", true)
	if err != nil {
		t.Fatalf("ListPolicies failed: %v", err)
	}
	if len(travel) != 1 {
		t.Fatalf("a supersede must be scoped to its own category, got %d", len(travel))
	}
}

// Withdrawing a limit leaves the row and its consumption history in place, and the
// category stops being governed.
func TestDeactivatePolicy_WithdrawsAndStopsGoverning(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantID := uuid.NewString()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	policy := seedPolicy(t, s, ctx, tenantID, "le-1", "PROCUREMENT", "MONTHLY", "GBP", 100)

	if _, err := s.EvaluateSpend(ctx, evaluation("le-1", "PROCUREMENT", "GBP", uuid.NewString(), 50)); err != nil {
		t.Fatalf("pre-withdrawal check failed: %v", err)
	}

	if err := s.DeactivatePolicy(ctx, policy.SpendPolicyID); err != nil {
		t.Fatalf("DeactivatePolicy failed: %v", err)
	}

	// Nothing in force, so a spend the limit would have refused is unevaluated.
	d, err := s.EvaluateSpend(ctx, evaluation("le-1", "PROCUREMENT", "GBP", uuid.NewString(), 99999))
	if err != nil {
		t.Fatalf("post-withdrawal check failed: %v", err)
	}
	if d.Basis != "no_policy_configured" {
		t.Fatalf("expected the category to be ungoverned, got basis %q", d.Basis)
	}

	// The consumption recorded while it WAS in force survives.
	rows, err := s.ListConsumptions(ctx, "le-1", policy.SpendPolicyID)
	if err != nil {
		t.Fatalf("ListConsumptions failed: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("withdrawing a limit must not discard its history, got %d rows", len(rows))
	}

	// A second withdrawal reports that nothing was in force to withdraw.
	if err := s.DeactivatePolicy(ctx, policy.SpendPolicyID); !errors.Is(err, domain.ErrPolicyNotFound) {
		t.Errorf("expected ErrPolicyNotFound on a repeat withdrawal, got %v", err)
	}
}

// The meter and the enforcement must agree.
//
// The console used to compute committed spend by summing every consumption row in
// the browser with NO period window, while enforcement summed only from the start
// of the current month. A row recorded before this month therefore counted toward
// the meter but not toward the decision — so a budget could read as exhausted while
// the next check would in fact be permitted. The aggregate applies the same window,
// in SQL, beside the enforcement query.
func TestPolicyUsageTotals_AppliesTheEnforcementWindow(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantID := uuid.NewString()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	policy := seedPolicy(t, s, ctx, tenantID, "le-1", "PROCUREMENT", "MONTHLY", "GBP", 1000)

	// One spend inside the window, through the normal path.
	if _, err := s.EvaluateSpend(ctx, evaluation("le-1", "PROCUREMENT", "GBP", uuid.NewString(), 200)); err != nil {
		t.Fatalf("in-window check failed: %v", err)
	}
	// One backdated to before the window, which only SQL can create.
	if _, err := pool.Exec(ctx, `
		INSERT INTO spend_consumptions (
			consumption_id, tenant_id, legal_entity_id, spend_policy_id, amount,
			currency_code, correlation_id, decision_outcome, recorded_by_principal_id, recorded_at
		) VALUES ($1,$2,'le-1',$3,777,'GBP',$4,'ALLOWED','test', now() - interval '70 days')
	`, uuid.NewString(), tenantID, policy.SpendPolicyID, uuid.NewString()); err != nil {
		t.Fatalf("failed to backdate a consumption: %v", err)
	}

	totals, err := s.PolicyUsageTotals(ctx, "le-1", "PROCUREMENT")
	if err != nil {
		t.Fatalf("PolicyUsageTotals failed: %v", err)
	}
	if len(totals) != 1 {
		t.Fatalf("expected one policy's totals, got %d", len(totals))
	}
	if totals[0].Consumed != 200 {
		t.Fatalf("expected only the in-window 200 to count, got %v — the 70-day-old row must be excluded, exactly as enforcement excludes it", totals[0].Consumed)
	}

	// And it agrees with what a decision reports as prior consumption.
	d, err := s.EvaluateSpend(ctx, evaluation("le-1", "PROCUREMENT", "GBP", uuid.NewString(), 1))
	if err != nil {
		t.Fatalf("comparison check failed: %v", err)
	}
	if d.PriorConsumption != totals[0].Consumed {
		t.Fatalf("the meter (%v) and the decision (%v) disagree about committed spend", totals[0].Consumed, d.PriorConsumption)
	}
}

// Refusals are counted, and a policy with no activity still appears with zeroes —
// a missing row would leave the console unable to draw a meter at all.
func TestPolicyUsageTotals_CountsRefusalsAndIncludesIdlePolicies(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantID := uuid.NewString()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	seedPolicy(t, s, ctx, tenantID, "le-1", "BUSY", "MONTHLY", "GBP", 100)
	seedPolicy(t, s, ctx, tenantID, "le-1", "IDLE", "MONTHLY", "GBP", 100)

	if _, err := s.EvaluateSpend(ctx, evaluation("le-1", "BUSY", "GBP", uuid.NewString(), 5000)); err != nil {
		t.Fatalf("refused check failed: %v", err)
	}

	totals, err := s.PolicyUsageTotals(ctx, "le-1", "")
	if err != nil {
		t.Fatalf("PolicyUsageTotals failed: %v", err)
	}
	if len(totals) != 2 {
		t.Fatalf("expected both policies, got %d", len(totals))
	}
	for _, tt := range totals {
		if tt.Consumed != 0 {
			t.Errorf("a refusal must not count as committed spend, got %v", tt.Consumed)
		}
	}
	refusals := 0
	for _, tt := range totals {
		refusals += tt.RefusedCount
	}
	if refusals != 1 {
		t.Errorf("expected exactly one refusal counted, got %d", refusals)
	}
}

// Withdrawn limits are excluded: the console draws meters for what is in force.
func TestPolicyUsageTotals_ExcludesWithdrawnLimits(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantID := uuid.NewString()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	policy := seedPolicy(t, s, ctx, tenantID, "le-1", "PROCUREMENT", "MONTHLY", "GBP", 100)

	if err := s.DeactivatePolicy(ctx, policy.SpendPolicyID); err != nil {
		t.Fatalf("DeactivatePolicy failed: %v", err)
	}

	totals, err := s.PolicyUsageTotals(ctx, "le-1", "")
	if err != nil {
		t.Fatalf("PolicyUsageTotals failed: %v", err)
	}
	if len(totals) != 0 {
		t.Fatalf("a withdrawn limit must not appear in usage, got %d", len(totals))
	}
}

// One tenant must never see or affect another's policies and consumption.
func TestStore_TenantIsolation(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantA := uuid.NewString()
	tenantB := uuid.NewString()
	ctxA := svcmiddleware.WithTenant(context.Background(), tenantA)
	ctxB := svcmiddleware.WithTenant(context.Background(), tenantB)

	seedPolicy(t, s, ctxA, tenantA, "le-shared", "PROCUREMENT", "MONTHLY", "GBP", 10000)
	if _, err := s.EvaluateSpend(ctxA, evaluation("le-shared", "PROCUREMENT", "GBP", uuid.NewString(), 2500)); err != nil {
		t.Fatalf("tenant A check failed: %v", err)
	}

	// Same entity and category, different tenant: A's policy must be invisible,
	// so B's check finds nothing configured rather than A's threshold.
	d, err := s.EvaluateSpend(ctxB, evaluation("le-shared", "PROCUREMENT", "GBP", uuid.NewString(), 999999))
	if err != nil {
		t.Fatalf("tenant B check failed: %v", err)
	}
	if d.Basis != "no_policy_configured" {
		t.Fatalf("tenant B must not see tenant A's policy, got basis %q", d.Basis)
	}

	policiesB, err := s.ListPolicies(ctxB, "", "", true)
	if err != nil {
		t.Fatalf("ListPolicies failed: %v", err)
	}
	if len(policiesB) != 0 {
		t.Fatalf("tenant B must see no policies, got %d", len(policiesB))
	}
	consumptionsB, err := s.ListConsumptions(ctxB, "", "")
	if err != nil {
		t.Fatalf("ListConsumptions failed: %v", err)
	}
	if len(consumptionsB) != 0 {
		t.Fatalf("tenant B must see no consumption, got %d", len(consumptionsB))
	}
}

// Every store method must reject a request that carried no tenant scope, and do
// it distinguishably from a broken database.
func TestStore_NoTenantScope_IsDistinctError(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)
	ctx := context.Background() // deliberately no tenant

	if _, err := s.CreatePolicy(ctx, &domain.SpendPolicy{}); !errors.Is(err, domain.ErrTenantMissing) {
		t.Errorf("CreatePolicy: expected ErrTenantMissing, got %v", err)
	}
	if _, err := s.ListPolicies(ctx, "", "", true); !errors.Is(err, domain.ErrTenantMissing) {
		t.Errorf("ListPolicies: expected ErrTenantMissing, got %v", err)
	}
	if _, err := s.ListConsumptions(ctx, "", ""); !errors.Is(err, domain.ErrTenantMissing) {
		t.Errorf("ListConsumptions: expected ErrTenantMissing, got %v", err)
	}
	if _, err := s.EvaluateSpend(ctx, evaluation("le-1", "PROCUREMENT", "GBP", uuid.NewString(), 1)); !errors.Is(err, domain.ErrTenantMissing) {
		t.Errorf("EvaluateSpend: expected ErrTenantMissing, got %v", err)
	}
	if _, err := s.SumConsumption(ctx, uuid.NewString(), "GBP", time.Time{}); !errors.Is(err, domain.ErrTenantMissing) {
		t.Errorf("SumConsumption: expected ErrTenantMissing, got %v", err)
	}
}
