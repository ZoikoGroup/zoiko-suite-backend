package store_test

import (
	"context"
	"errors"
	"testing"

	"zoiko.io/anomaly-detection-svc/internal/domain"
)

// Rule-engine surface (/v1/anomalies) and raw RLS guarantees, as the
// NOSUPERUSER NOBYPASSRLS app role. Ports the intent of the pre-rewrite
// handler_test.go (detect → status update → get → list; create/list rules)
// onto the real store.

func TestRuleEngine_DetectUpdateGetList(t *testing.T) {
	f := newFixture(t)

	rule, err := f.s.CreateRule(f.ctx, f.cmd(orgA, submitter, "POST /v1/anomalies/rules", nil), domain.CreateRuleRequest{
		RuleName: "High Payroll Variance Limit", DomainName: "WORKFORCE", MetricType: "gross_pay_variance",
		ThresholdValue: 25, ZScoreCutoff: 3.5,
	})
	if err != nil || rule.Resource.ZScoreCutoff != 3.5 || !rule.Resource.IsActive {
		t.Fatalf("create rule: %+v err=%v", rule.Resource, err)
	}
	rules, _, err := f.s.ListRules(f.ctx, orgA, domain.RuleFilter{DomainName: "WORKFORCE"}, domain.PageRequest{Limit: 50})
	if err != nil || len(rules) != 1 {
		t.Fatalf("list rules: %d err=%v", len(rules), err)
	}

	detectCmd := f.cmd(orgA, submitter, "POST /v1/anomalies/detect", nil)
	detectCmd.LegalEntityID = "entity-001"
	detected, err := f.s.DetectAnomaly(f.ctx, detectCmd, domain.DetectAnomalyRequest{
		DomainName: "FINANCE", SourceEntityID: "tx-9901", RuleID: rule.Resource.RuleID, MetricType: "transaction_amount",
		ObservedValue: 500000, ExpectedValue: 15000, StdDeviation: 5000,
	})
	if err != nil {
		t.Fatalf("detect: %v", err)
	}
	rec := detected.Resource
	if rec.Severity != domain.SeverityCritical || rec.Status != domain.StatusOpen || rec.LegalEntityID != "entity-001" || rec.DetectedBy != submitter {
		t.Fatalf("detected record: %+v", rec)
	}

	// Replay returns the same record.
	replay, err := f.s.DetectAnomaly(f.ctx, detectCmd, domain.DetectAnomalyRequest{
		DomainName: "FINANCE", SourceEntityID: "tx-9901", RuleID: rule.Resource.RuleID, MetricType: "transaction_amount",
		ObservedValue: 500000, ExpectedValue: 15000, StdDeviation: 5000,
	})
	if err != nil || !replay.Replayed || replay.Resource.AnomalyID != rec.AnomalyID {
		t.Fatalf("detect replay: %+v err=%v", replay, err)
	}

	statusRoute := "POST /v1/anomalies/{anomalyID}/status"
	inv, err := f.s.UpdateAnomalyStatus(f.ctx, f.cmd(orgA, submitter, statusRoute, ver(rec.Version)), rec.AnomalyID,
		domain.UpdateStatusRequest{Status: domain.StatusUnderInvestigation, ResolutionNotes: "reviewing velocity logs"})
	if err != nil || inv.Resource.Status != domain.StatusUnderInvestigation {
		t.Fatalf("to UNDER_INVESTIGATION: %+v err=%v", inv.Resource, err)
	}
	// SoD: the detector cannot disposition.
	if _, err := f.s.UpdateAnomalyStatus(f.ctx, f.cmd(orgA, submitter, statusRoute, ver(inv.Resource.Version)), rec.AnomalyID,
		domain.UpdateStatusRequest{Status: domain.StatusConfirmedAnomaly, ResolutionNotes: "self"}); !errors.Is(err, domain.ErrSegregationOfDuties) {
		t.Fatalf("detector dispositioning: %v", err)
	}
	// Illegal transition: UNDER_INVESTIGATION -> RESOLVED.
	if _, err := f.s.UpdateAnomalyStatus(f.ctx, f.cmd(orgA, reviewer, statusRoute, ver(inv.Resource.Version)), rec.AnomalyID,
		domain.UpdateStatusRequest{Status: domain.StatusResolved, ResolutionNotes: "x"}); !errors.Is(err, domain.ErrInvalidStatusTransition) {
		t.Fatalf("illegal transition: %v", err)
	}
	conf, err := f.s.UpdateAnomalyStatus(f.ctx, f.cmd(orgA, reviewer, statusRoute, ver(inv.Resource.Version)), rec.AnomalyID,
		domain.UpdateStatusRequest{Status: domain.StatusConfirmedAnomaly, ResolutionNotes: "confirmed"})
	if err != nil || conf.Resource.Status != domain.StatusConfirmedAnomaly || conf.Resource.InvestigatedBy != reviewer {
		t.Fatalf("confirm: %+v err=%v", conf.Resource, err)
	}

	got, err := f.s.GetAnomaly(f.ctx, orgA, rec.AnomalyID)
	if err != nil || got.Version != conf.Resource.Version {
		t.Fatalf("get: %+v err=%v", got, err)
	}
	list, _, err := f.s.ListAnomalies(f.ctx, orgA, domain.AnomalyFilter{LegalEntityID: "entity-001"}, domain.PageRequest{Limit: 50})
	if err != nil || len(list) != 1 {
		t.Fatalf("list anomalies: %d err=%v", len(list), err)
	}

	// Outbox: detected + dispositioned, written in the command transactions.
	var events int
	if err := f.admin.QueryRow(f.ctx, "SELECT COUNT(*) FROM anomaly_outbox WHERE aggregate_id = $1", rec.AnomalyID).Scan(&events); err != nil || events != 2 {
		t.Fatalf("outbox rows for record: %d err=%v", events, err)
	}

	// Cross tenant: 404, and the rule cannot be referenced from tenant B.
	if _, err := f.s.GetAnomaly(f.ctx, orgB, rec.AnomalyID); !errors.Is(err, domain.ErrAnomalyRecordNotFound) {
		t.Fatalf("cross-tenant get anomaly: %v", err)
	}
	if _, err := f.s.GetRule(f.ctx, orgB, rule.Resource.RuleID); !errors.Is(err, domain.ErrAnomalyRuleNotFound) {
		t.Fatalf("cross-tenant get rule: %v", err)
	}
	if _, err := f.s.DetectAnomaly(f.ctx, f.cmd(orgB, submitter, "POST /v1/anomalies/detect", nil), domain.DetectAnomalyRequest{
		DomainName: "FINANCE", SourceEntityID: "tx-1", RuleID: rule.Resource.RuleID, MetricType: "m", ObservedValue: 1, ExpectedValue: 1,
	}); !errors.Is(err, domain.ErrAnomalyRuleReferenceInvalid) {
		t.Fatalf("cross-tenant rule reference: %v", err)
	}
}

// RLS is enforced by Postgres itself, not only by the WHERE tenant_id
// filters: as the app role, unfiltered SELECTs see only the declared tenant,
// a write for another tenant is rejected by WITH CHECK, and every table is
// ENABLE + FORCE ROW LEVEL SECURITY.
func TestRLS_EnforcedByDatabase(t *testing.T) {
	f := newFixture(t)
	f.registerModel(orgA, "rls", "internal-anomaly", "v1.0", 5000, 500)
	f.runDetection(orgA, baseRunRequest("rls", 6000))

	ctx := context.Background()
	for _, table := range []string{"anomaly_models", "detection_runs", "anomaly_signals", "anomaly_outbox", "idempotency_keys"} {
		var seenAsA, seenAsB, seenNoTenant int
		count := func(tenant string, dest *int) {
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
			if err := tx.QueryRow(ctx, "SELECT COUNT(*) FROM "+table).Scan(dest); err != nil {
				t.Fatalf("count %s: %v", table, err)
			}
		}
		count(orgA, &seenAsA)
		count(orgB, &seenAsB)
		count("", &seenNoTenant)
		if seenAsA == 0 || seenAsB != 0 || seenNoTenant != 0 {
			t.Fatalf("%s: RLS not enforced (asA=%d asB=%d none=%d)", table, seenAsA, seenAsB, seenNoTenant)
		}
	}

	tx, err := f.app.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", orgB); err != nil {
		t.Fatalf("set tenant: %v", err)
	}
	_, err = tx.Exec(ctx, `INSERT INTO anomaly_detection_rules (rule_id, tenant_id, rule_name, domain_name, metric_type, threshold_value)
		VALUES ('arule-00000000-0000-0000-0000-000000000001', $1, 'x', 'd', 'm', 1)`, orgA)
	if err == nil {
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
	if n < 8 {
		t.Fatalf("expected at least 8 tables, saw %d", n)
	}
}
