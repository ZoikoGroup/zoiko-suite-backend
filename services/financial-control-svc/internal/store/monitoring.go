package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"zoiko.io/financial-control-svc/internal/domain"
)

// Monitoring computes the ZS-CONTROL-001 s28 signals for an entity and, optionally, one
// period. Everything is derived on read from authoritative rows, so the numbers can never
// drift from the runs and exceptions they describe, and the caller-supplied `now` makes
// the aging figures reproducible. Signals the platform cannot honestly produce are
// reported as unavailable with the reason, never as zero.
func (s *PgStore) Monitoring(ctx context.Context, tenantID, legalEntityID, periodID string, now time.Time) (*domain.MonitoringSnapshot, error) {
	m := &domain.MonitoringSnapshot{LegalEntityID: legalEntityID, PeriodID: periodID, AsOf: now.UTC(),
		ExceptionsByAssertion: []domain.CountAmount{}, ExceptionsByCategory: []domain.CountAmount{},
		RootCauses: []domain.CountAmount{}, Unavailable: domain.UnavailableSignals()}
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		// Completion: how many controls exist, how many have a run in scope, how many actually executed.
		if err := tx.QueryRow(ctx, `
			WITH latest AS (
				SELECT DISTINCT ON (control_definition_id) control_definition_id, lifecycle_state, result_state
				FROM control_runs
				WHERE tenant_id = $1 AND legal_entity_id = $2 AND ($3 = '' OR period_id = $3)
				ORDER BY control_definition_id, created_at DESC, run_id DESC)
			SELECT (SELECT COUNT(*) FROM control_definitions WHERE tenant_id = $1),
			       COUNT(*),
			       COUNT(*) FILTER (WHERE result_state <> 'NOT_EVALUATED'),
			       COUNT(*) FILTER (WHERE lifecycle_state = 'FAILED' OR result_state = 'INDETERMINATE'),
			       COUNT(*) FILTER (WHERE lifecycle_state = 'CERTIFIED')
			FROM latest`, tenantID, legalEntityID, periodID).
			Scan(&m.Completion.DefinitionsTotal, &m.Completion.WithRun, &m.Completion.Executed,
				&m.Completion.FailedOrIndeterminate, &m.Completion.Certified); err != nil {
			return err
		}

		// Run duration and lag.
		if err := tx.QueryRow(ctx, `
			SELECT COALESCE(AVG(EXTRACT(EPOCH FROM (completed_at - started_at))),0)::float8,
			       COALESCE(MAX(EXTRACT(EPOCH FROM (completed_at - started_at))),0)::float8,
			       COUNT(*)
			FROM control_runs
			WHERE tenant_id = $1 AND legal_entity_id = $2 AND ($3 = '' OR period_id = $3)
			  AND started_at IS NOT NULL AND completed_at IS NOT NULL`, tenantID, legalEntityID, periodID).
			Scan(&m.RunDuration.AvgSeconds, &m.RunDuration.MaxSeconds, &m.RunDuration.Samples); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `
			SELECT COALESCE(MAX(EXTRACT(EPOCH FROM ($4::timestamptz - created_at))),0)::float8
			FROM control_runs
			WHERE tenant_id = $1 AND legal_entity_id = $2 AND ($3 = '' OR period_id = $3)
			  AND lifecycle_state IN ('SCHEDULED','PREPARING')`, tenantID, legalEntityID, periodID, now).
			Scan(&m.RunDuration.OldestUnstartedSeconds); err != nil {
			return err
		}

		// Exceptions of the latest run of each control (an older run's findings are superseded).
		const scope = `
			WITH latest AS (
				SELECT DISTINCT ON (control_definition_id) run_id FROM control_runs
				WHERE tenant_id = $1 AND legal_entity_id = $2 AND ($3 = '' OR period_id = $3)
				ORDER BY control_definition_id, created_at DESC, run_id DESC),
			ex AS (
				SELECT e.* FROM control_exceptions e JOIN latest l ON l.run_id = e.run_id WHERE e.tenant_id = $1)`
		if err := tx.QueryRow(ctx, scope+`
			SELECT COUNT(*),
			  COUNT(*) FILTER (WHERE state NOT IN ('CLOSED','WAIVED_UNDER_AUTHORITY','CARRIED_FORWARD_UNDER_AUTHORITY')),
			  COUNT(*) FILTER (WHERE state NOT IN ('CLOSED','WAIVED_UNDER_AUTHORITY','CARRIED_FORWARD_UNDER_AUTHORITY') AND due_at < $4),
			  COUNT(*) FILTER (WHERE category = 'LATE_DATA'),
			  COUNT(*) FILTER (WHERE state NOT IN ('CLOSED','WAIVED_UNDER_AUTHORITY','CARRIED_FORWARD_UNDER_AUTHORITY') AND $4 - created_at <= interval '7 days'),
			  COUNT(*) FILTER (WHERE state NOT IN ('CLOSED','WAIVED_UNDER_AUTHORITY','CARRIED_FORWARD_UNDER_AUTHORITY') AND $4 - created_at > interval '7 days'  AND $4 - created_at <= interval '30 days'),
			  COUNT(*) FILTER (WHERE state NOT IN ('CLOSED','WAIVED_UNDER_AUTHORITY','CARRIED_FORWARD_UNDER_AUTHORITY') AND $4 - created_at > interval '30 days' AND $4 - created_at <= interval '90 days'),
			  COUNT(*) FILTER (WHERE state NOT IN ('CLOSED','WAIVED_UNDER_AUTHORITY','CARRIED_FORWARD_UNDER_AUTHORITY') AND $4 - created_at > interval '90 days')
			FROM ex`, tenantID, legalEntityID, periodID, now).
			Scan(&m.Exceptions.Total, &m.Exceptions.Unresolved, &m.Exceptions.SLABreached, &m.Exceptions.LateData,
				&m.Aging.UpTo7Days, &m.Aging.Days8To30, &m.Aging.Days31To90, &m.Aging.Over90Days); err != nil {
			return err
		}

		// Recurrence: same control, reason and an overlapping record already raised earlier.
		if err := tx.QueryRow(ctx, scope+`
			SELECT COUNT(*) FROM ex e JOIN control_runs r ON r.run_id = e.run_id
			WHERE EXISTS (
				SELECT 1 FROM control_exceptions p JOIN control_runs pr ON pr.run_id = p.run_id
				WHERE p.tenant_id = $1 AND pr.tenant_id = $1 AND pr.legal_entity_id = $2
				  AND pr.control_definition_id = r.control_definition_id AND p.reason_code = e.reason_code
				  AND p.record_ids && e.record_ids AND p.exception_id <> e.exception_id AND p.created_at < e.created_at)`,
			tenantID, legalEntityID, periodID).Scan(&m.Exceptions.Recurring); err != nil {
			return err
		}

		for _, q := range []struct {
			dst *[]domain.CountAmount
			sql string
		}{
			{&m.ExceptionsByAssertion, `SELECT assertion, currency::text, COUNT(*), COALESCE(SUM(exposure),0)::text FROM ex
				WHERE state NOT IN ('CLOSED','WAIVED_UNDER_AUTHORITY','CARRIED_FORWARD_UNDER_AUTHORITY')
				GROUP BY assertion, currency ORDER BY assertion, currency`},
			{&m.ExceptionsByCategory, `SELECT category, currency::text, COUNT(*), COALESCE(SUM(exposure),0)::text FROM ex
				WHERE state NOT IN ('CLOSED','WAIVED_UNDER_AUTHORITY','CARRIED_FORWARD_UNDER_AUTHORITY')
				GROUP BY category, currency ORDER BY category, currency`},
			// Root causes are read from the resolving transitions of the same exceptions.
			{&m.RootCauses, `SELECT rc.code, e.currency::text, COUNT(*), COALESCE(SUM(e.exposure),0)::text
				FROM ex e JOIN LATERAL (SELECT root_cause_code AS code FROM exception_transitions
					WHERE tenant_id = $1 AND exception_id = e.exception_id AND root_cause_code <> ''
					ORDER BY transition_id DESC LIMIT 1) rc ON true
				GROUP BY rc.code, e.currency ORDER BY rc.code, e.currency`},
		} {
			rows, err := tx.Query(ctx, scope+q.sql, tenantID, legalEntityID, periodID)
			if err != nil {
				return err
			}
			for rows.Next() {
				var c domain.CountAmount
				var ccy string
				if err := rows.Scan(&c.Key, &ccy, &c.Count, &c.Exposure); err != nil {
					rows.Close()
					return err
				}
				c.Currency = ccy
				*q.dst = append(*q.dst, c)
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return err
			}
		}

		// Certification latency: READY_TO_CERTIFY -> CERTIFIED, over certified runs in scope.
		return tx.QueryRow(ctx, `
			SELECT COALESCE(AVG(EXTRACT(EPOCH FROM (c.occurred_at - rdy.occurred_at))),0)::float8, COUNT(*)
			FROM control_runs r
			JOIN LATERAL (SELECT MAX(occurred_at) AS occurred_at FROM control_run_transitions
				WHERE tenant_id = r.tenant_id AND run_id = r.run_id AND dimension = 'LIFECYCLE' AND to_state = 'READY_TO_CERTIFY') rdy ON rdy.occurred_at IS NOT NULL
			JOIN LATERAL (SELECT MAX(occurred_at) AS occurred_at FROM control_run_transitions
				WHERE tenant_id = r.tenant_id AND run_id = r.run_id AND dimension = 'LIFECYCLE' AND to_state = 'CERTIFIED') c ON c.occurred_at IS NOT NULL
			WHERE r.tenant_id = $1 AND r.legal_entity_id = $2 AND ($3 = '' OR r.period_id = $3)`,
			tenantID, legalEntityID, periodID).Scan(&m.CertificationLatency.AvgSeconds, &m.CertificationLatency.Samples)
	})
	if err != nil {
		return nil, err
	}
	m.Derive()
	return m, nil
}
