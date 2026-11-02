package store_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"go.uber.org/zap"

	"zoiko.io/forecasting-svc/internal/domain"
	"zoiko.io/forecasting-svc/internal/outbox"
)

// FIN-02 forecast lifecycle, raw RLS guarantees and the outbox relay, as the
// NOSUPERUSER NOBYPASSRLS app role.

func (f *fixture) createForecast(org string) *domain.Forecast {
	f.t.Helper()
	req := domain.GenerateForecastRequest{
		LegalEntityID: "LE-1001", ModelName: "Q3 cash flow", Domain: domain.DomainCashFlow,
		HorizonPeriods: 6, HistoricalData: []float64{10000, 12000, 11500, 13000, 14500, 16000}, HistoricalStartDate: "2025-01-01",
	}
	req.ApplyDefaults()
	if err := req.Validate(); err != nil {
		f.t.Fatalf("forecast request invalid: %v", err)
	}
	created, err := f.s.CreateForecast(f.ctx, f.cmd(org, requester, "forecast.generate", 0), req)
	if err != nil {
		f.t.Fatalf("create forecast: %v", err)
	}
	return created
}

func TestForecast_LifecycleMakerCheckerAndSupersede(t *testing.T) {
	f := newFixture(t)
	fc := f.createForecast(orgA)
	if fc.Status != domain.ForecastCalculated || fc.Version != 1 || len(fc.Projections) != 6 || fc.Projections[0].ProjectedAmount <= 0 {
		t.Fatalf("created forecast: %+v", fc)
	}

	growth := 0.10
	recalc, err := f.s.RecalculateForecast(f.ctx, f.cmd(orgA, requester, "forecast.recalculate", fc.Version), fc.ID,
		domain.RecalculateForecastRequest{GrowthRateAdjustment: &growth})
	if err != nil || recalc.Version != 2 || recalc.Projections[0].ProjectedAmount <= fc.Projections[0].ProjectedAmount {
		t.Fatalf("recalculate: %+v err=%v", recalc, err)
	}
	if _, err := f.s.RecalculateForecast(f.ctx, f.cmd(orgA, requester, "forecast.recalculate", 1), fc.ID,
		domain.RecalculateForecastRequest{GrowthRateAdjustment: &growth}); errCode(err) != "stale_version" {
		t.Fatalf("stale recalculate: %v", err)
	}

	sub, err := f.s.SubmitForecast(f.ctx, f.cmd(orgA, requester, "forecast.submit", recalc.Version), fc.ID)
	if err != nil || sub.Status != domain.ForecastReviewed {
		t.Fatalf("submit: %+v err=%v", sub, err)
	}
	if _, err := f.s.ApproveForecast(f.ctx, f.cmd(orgA, requester, "forecast.approve", sub.Version), fc.ID); errCode(err) != "segregation_of_duties" {
		t.Fatalf("maker approving own forecast: %v", err)
	}
	app, err := f.s.ApproveForecast(f.ctx, f.cmd(orgA, planner, "forecast.approve", sub.Version), fc.ID)
	if err != nil || app.Status != domain.ForecastApproved {
		t.Fatalf("approve: %+v err=%v", app, err)
	}
	if _, err := f.s.ArchiveForecast(f.ctx, f.cmd(orgA, requester, "forecast.archive", app.Version), fc.ID); errCode(err) != "invalid_transition" {
		t.Fatalf("archive approved: %v", err)
	}
	pub, err := f.s.PublishForecast(f.ctx, f.cmd(orgA, planner, "forecast.publish", app.Version), fc.ID)
	if err != nil || pub.Status != domain.ForecastPublished {
		t.Fatalf("publish: %+v err=%v", pub, err)
	}

	// Calculated content of a published forecast is immutable in the DB.
	if _, err := f.admin.Exec(f.ctx, "UPDATE forecast_models SET horizon_periods = 1 WHERE id = $1", fc.ID); err == nil {
		t.Fatalf("trigger must reject changing a published forecast")
	}

	re, err := f.s.Reforecast(f.ctx, f.cmd(orgA, requester, "forecast.reforecast", 0), fc.ID, domain.ReforecastRequest{})
	if err != nil || re.PriorForecastID != fc.ID || re.Status != domain.ForecastCalculated {
		t.Fatalf("reforecast: %+v err=%v", re, err)
	}
	re, _ = f.s.SubmitForecast(f.ctx, f.cmd(orgA, requester, "forecast.submit", re.Version), re.ID)
	re, _ = f.s.ApproveForecast(f.ctx, f.cmd(orgA, planner, "forecast.approve", re.Version), re.ID)
	if _, err := f.s.PublishForecast(f.ctx, f.cmd(orgA, planner, "forecast.publish", re.Version), re.ID); err != nil {
		t.Fatalf("publish reforecast: %v", err)
	}
	prior, _ := f.s.GetForecast(f.ctx, orgA, fc.ID)
	if prior.Status != domain.ForecastSuperseded {
		t.Fatalf("prior forecast should be SUPERSEDED: %s", prior.Status)
	}

	list, _, err := f.s.ListForecasts(f.ctx, orgA, domain.ForecastListFilter{LegalEntityID: "LE-1001", Page: domain.PageRequest{Limit: 1}})
	if err != nil || len(list) != 1 {
		t.Fatalf("list page: %d err=%v", len(list), err)
	}
	if _, err := f.s.GetForecast(f.ctx, orgB, fc.ID); !errors.Is(err, domain.ErrForecastNotFound) {
		t.Fatalf("cross-tenant get: %v", err)
	}
}

func TestForecast_IdempotentReplay(t *testing.T) {
	f := newFixture(t)
	req := domain.GenerateForecastRequest{LegalEntityID: "LE-1001", ModelName: "m", Domain: domain.DomainPayroll,
		HistoricalData: []float64{1, 2, 3}, HistoricalStartDate: "2025-01-01"}
	req.ApplyDefaults()
	cmd := f.cmd(orgA, requester, "forecast.generate", 0)
	first, err := f.s.CreateForecast(f.ctx, cmd, req)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	_, err = f.s.CreateForecast(f.ctx, cmd, req)
	var replay *domain.IdempotentReplay
	if !errors.As(err, &replay) || replay.ResourceID != first.ID {
		t.Fatalf("replay: %v", err)
	}
}

// RLS is enforced by Postgres itself: unfiltered SELECTs as the app role see
// only the declared tenant, WITH CHECK rejects a cross-tenant write, and
// every table is ENABLE + FORCE ROW LEVEL SECURITY.
func TestRLS_EnforcedByDatabase(t *testing.T) {
	f := newFixture(t)
	f.registerRelease(orgA, "rls", "internal-forecast", "v1.0")
	f.suggestDrivers(orgA, baseDriversRequest("rls"))
	f.createForecast(orgA)
	ctx := context.Background()

	for _, table := range []string{"forecast_models", "forecast_projections", "forecast_model_releases", "forecast_assist_jobs",
		"suggested_drivers", "evidence_references", "idempotency_keys", "outbox_events"} {
		count := func(tenant string) int {
			tx, err := f.app.Begin(ctx)
			if err != nil {
				t.Fatalf("begin: %v", err)
			}
			defer func() { _ = tx.Rollback(ctx) }()
			if tenant != "" {
				if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenant); err != nil {
					t.Fatalf("set tenant: %v", err)
				}
			}
			var n int
			if err := tx.QueryRow(ctx, "SELECT COUNT(*) FROM "+table).Scan(&n); err != nil {
				t.Fatalf("count %s: %v", table, err)
			}
			return n
		}
		if a, b, none := count(orgA), count(orgB), count(""); a == 0 || b != 0 || none != 0 {
			t.Fatalf("%s: RLS not enforced (asA=%d asB=%d none=%d)", table, a, b, none)
		}
	}

	tx, err := f.app.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	_, _ = tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", orgB)
	if _, err := tx.Exec(ctx, `INSERT INTO forecast_model_releases (release_id, tenant_id, domain_name, model_provider, model_version, created_by)
		VALUES ('fmr_00000000-0000-0000-0000-000000000001', $1, 'd', 'p', 'v', 'x')`, orgA); err == nil {
		t.Fatalf("WITH CHECK must reject a row written for another tenant")
	}

	rows, err := f.admin.Query(ctx, `SELECT relname, relrowsecurity, relforcerowsecurity FROM pg_class
		WHERE relnamespace = current_schema()::regnamespace AND relkind = 'r'`)
	if err != nil {
		t.Fatalf("pg_class: %v", err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var name string
		var enabled, forced bool
		if err := rows.Scan(&name, &enabled, &forced); err != nil {
			t.Fatalf("scan: %v", err)
		}
		n++
		if !enabled || !forced {
			t.Fatalf("table %s: rowsecurity=%v force=%v", name, enabled, forced)
		}
	}
	if n < 10 {
		t.Fatalf("expected at least 10 tables, saw %d", n)
	}
}

type rawPublisher struct {
	mu   sync.Mutex
	fail bool
	keys []string
}

func (p *rawPublisher) PublishRaw(_ context.Context, key string, _ []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.fail {
		return errors.New("broker down")
	}
	p.keys = append(p.keys, key)
	return nil
}

// The relay drains every tenant's committed events as the app role (via the
// app.outbox_relay policy disjunct) and retries failed publishes.
func TestOutboxRelay_DrainsAllTenants(t *testing.T) {
	f := newFixture(t)
	f.createForecast(orgA)
	f.createForecast(orgB)

	pub := &rawPublisher{fail: true}
	relay := outbox.NewRelay(f.app, pub, 0, 50, zap.NewNop())
	if n, err := relay.RelayOnce(f.ctx); err != nil || n != 0 {
		t.Fatalf("failing relay: n=%d err=%v", n, err)
	}
	var retried int
	_ = f.admin.QueryRow(f.ctx, "SELECT COUNT(*) FROM outbox_events WHERE publish_state = 'Pending' AND attempt_count = 1").Scan(&retried)
	if retried != 2 {
		t.Fatalf("retries scheduled: %d", retried)
	}
	if _, err := f.admin.Exec(f.ctx, "UPDATE outbox_events SET next_attempt_at = NOW()"); err != nil {
		t.Fatalf("reset backoff: %v", err)
	}
	pub.fail = false
	if n, err := relay.RelayOnce(f.ctx); err != nil || n != 2 {
		t.Fatalf("relay: n=%d err=%v", n, err)
	}
	seen := map[string]bool{}
	for _, k := range pub.keys {
		seen[k] = true
	}
	if !seen[orgA] || !seen[orgB] {
		t.Fatalf("relay must publish both tenants: %v", pub.keys)
	}
	var visible int
	if err := f.app.QueryRow(f.ctx, "SELECT COUNT(*) FROM outbox_events").Scan(&visible); err != nil || visible != 0 {
		t.Fatalf("unscoped app-role outbox read: %d err=%v", visible, err)
	}
}
