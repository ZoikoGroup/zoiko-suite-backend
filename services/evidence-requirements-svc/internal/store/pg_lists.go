package store

// Console read models (ER-3): the registers behind the /admin/evidence tabs
// and the KPI summary. Each list takes an offset and a limit; the handler asks
// for page_size+1 rows so it can tell whether a next page exists, and binds
// the offset into a signed cursor (identity-envelope/apicontract, API-4).
//
// Every statement carries the explicit tenant predicate as well as running
// under withRLS, for the reason given in this package's doc comment. Every
// ORDER BY ends on the primary key, so offset paging is stable: two rows with
// the same due date or evaluation time cannot swap between pages.

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"zoiko.io/evidence-requirements-svc/internal/domain"
)

// ListRequirementsPage is the Requirements register. f.AsOf zero means the
// full history, retired requirements included.
func (s *PgStore) ListRequirementsPage(ctx context.Context, tenantID string, f domain.ListRequirementsFilter, offset, limit int) ([]domain.EvidenceRequirement, error) {
	var asOf *time.Time
	if !f.AsOf.IsZero() {
		t := f.AsOf.UTC()
		asOf = &t
	}
	out := []domain.EvidenceRequirement{}
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT `+requirementColumns+`
			FROM evidence_requirements
			WHERE tenant_id = $1
			  AND ($2 = '' OR legal_entity_id::text = $2)
			  AND ($3 = '' OR domain_code = $3)
			  AND ($4 = '' OR action_type = $4)
			  AND ($5 = '' OR evidence_type = $5)
			  AND ($6::timestamptz IS NULL
			       OR (effective_from <= $6 AND (effective_to IS NULL OR effective_to > $6)))
			ORDER BY domain_code, action_type, evidence_type, effective_from DESC, evidence_requirement_id
			OFFSET $7 LIMIT $8`,
			tenantID, f.LegalEntityID, f.DomainCode, f.ActionType, f.EvidenceType, asOf, offset, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			r, err := scanRequirement(rows)
			if err != nil {
				return err
			}
			out = append(out, *r)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, mapPgError(err)
	}
	return out, nil
}

// ListEvaluationsPage is the Evaluations register (and, filtered on
// outcome=MISSING, the Missing tab): newest first.
func (s *PgStore) ListEvaluationsPage(ctx context.Context, tenantID string, f domain.ListEvaluationsFilter, offset, limit int) ([]domain.EvidenceEvaluation, error) {
	var since *time.Time
	if !f.Since.IsZero() {
		t := f.Since.UTC()
		since = &t
	}
	out := []domain.EvidenceEvaluation{}
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT `+evaluationColumns+`
			FROM evidence_evaluations
			WHERE tenant_id = $1
			  AND ($2 = '' OR legal_entity_id::text = $2)
			  AND ($3 = '' OR domain_code = $3)
			  AND ($4 = '' OR action_type = $4)
			  AND ($5 = '' OR outcome = $5)
			  AND ($6::timestamptz IS NULL OR evaluated_at >= $6)
			ORDER BY evaluated_at DESC, evaluation_id DESC
			OFFSET $7 LIMIT $8`,
			tenantID, f.LegalEntityID, f.DomainCode, f.ActionType, f.Outcome, since, offset, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			e, err := scanEvaluation(rows)
			if err != nil {
				return err
			}
			out = append(out, *e)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, mapPgError(err)
	}
	return out, nil
}

// ListEvidenceRequestsPage is the Evidence requests (PBC) register: soonest
// due first. Without a status filter and with f.OpenOnly it keeps the route's
// original meaning, requests not yet SATISFIED or CLOSED.
func (s *PgStore) ListEvidenceRequestsPage(ctx context.Context, tenantID string, f domain.ListEvidenceRequestsFilter, offset, limit int) ([]*domain.EvidenceRequest, error) {
	out := []*domain.EvidenceRequest{}
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+evidenceRequestColumns+` FROM evidence_requests
			WHERE tenant_id = $1
			  AND ($2 = '' OR legal_entity_id::text = $2)
			  AND ($3 = '' OR status = $3)
			  AND (NOT $4 OR status NOT IN ('SATISFIED','CLOSED'))
			ORDER BY due_at, request_id
			OFFSET $5 LIMIT $6`,
			tenantID, f.LegalEntityID, f.Status, f.OpenOnly && f.Status == "", offset, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			r, err := scanEvidenceRequest(rows)
			if err != nil {
				return err
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, mapPgError(err)
	}
	return out, nil
}

// GetSummary reads the console's KPI row in one transaction. It returns an
// error rather than a partial summary when any count cannot be read, so the
// console shows "unavailable" instead of a misleading 0.
func (s *PgStore) GetSummary(ctx context.Context, tenantID string) (*domain.EvidenceSummary, error) {
	out := &domain.EvidenceSummary{
		RequirementsByEvidenceType: map[string]int{},
		RequirementsByDomain:       map[string]int{},
		UncoveredActions:           []domain.UncoveredAction{},
	}
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `
			SELECT
			  (SELECT count(*) FROM evidence_requirements
			    WHERE tenant_id = $1 AND effective_from <= now()
			      AND (effective_to IS NULL OR effective_to > now())),
			  (SELECT count(*) FROM evidence_evaluations
			    WHERE tenant_id = $1 AND outcome = 'SATISFIED' AND evaluated_at >= now() - interval '30 days'),
			  (SELECT count(*) FROM evidence_evaluations
			    WHERE tenant_id = $1 AND outcome = 'MISSING' AND evaluated_at >= now() - interval '30 days'),
			  (SELECT count(*) FROM evidence_evaluations
			    WHERE tenant_id = $1 AND outcome = 'NO_REQUIREMENTS_DEFINED' AND evaluated_at >= now() - interval '30 days'),
			  (SELECT count(*) FROM (SELECT DISTINCT domain_code, action_type FROM evidence_evaluations
			    WHERE tenant_id = $1 AND outcome = 'NO_REQUIREMENTS_DEFINED' AND evaluated_at >= now() - interval '30 days') d),
			  (SELECT count(*) FROM evidence_requests
			    WHERE tenant_id = $1 AND status NOT IN ('SATISFIED','CLOSED')),
			  (SELECT count(*) FROM evidence_requests
			    WHERE tenant_id = $1 AND status NOT IN ('SATISFIED','CLOSED') AND due_at < now()),
			  outbox_unpublished_count(interval '5 minutes')`, tenantID).Scan(
			&out.ActiveRequirements, &out.EvaluationsSatisfied30d, &out.EvaluationsMissing30d,
			&out.EvaluationsNoRequirements30d, &out.ActionsWithNoRequirements30d,
			&out.OpenPBCRequests, &out.OverduePBCRequests, &out.UnpublishedOutboxOlderThan5Minutes); err != nil {
			return err
		}

		if err := countInto(ctx, tx, out.RequirementsByEvidenceType, `
			SELECT evidence_type, count(*) FROM evidence_requirements
			WHERE tenant_id = $1 AND effective_from <= now() AND (effective_to IS NULL OR effective_to > now())
			GROUP BY evidence_type`, tenantID); err != nil {
			return err
		}
		if err := countInto(ctx, tx, out.RequirementsByDomain, `
			SELECT domain_code, count(*) FROM evidence_requirements
			WHERE tenant_id = $1 AND effective_from <= now() AND (effective_to IS NULL OR effective_to > now())
			GROUP BY domain_code`, tenantID); err != nil {
			return err
		}

		rows, err := tx.Query(ctx, `
			SELECT domain_code, action_type, count(*), max(evaluated_at)
			FROM evidence_evaluations
			WHERE tenant_id = $1 AND outcome = 'NO_REQUIREMENTS_DEFINED' AND evaluated_at >= now() - interval '30 days'
			GROUP BY domain_code, action_type
			ORDER BY count(*) DESC, domain_code, action_type
			LIMIT 20`, tenantID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var u domain.UncoveredAction
			if err := rows.Scan(&u.DomainCode, &u.ActionType, &u.Evaluations, &u.LastAt); err != nil {
				return err
			}
			out.UncoveredActions = append(out.UncoveredActions, u)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, mapPgError(err)
	}
	return out, nil
}

func countInto(ctx context.Context, tx pgx.Tx, into map[string]int, sql, tenantID string) error {
	rows, err := tx.Query(ctx, sql, tenantID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var k string
		var n int
		if err := rows.Scan(&k, &n); err != nil {
			return err
		}
		into[k] = n
	}
	return rows.Err()
}

// OutboxUnpublished counts events written to the outbox and not yet published
// (the evidence_outbox_unpublished gauge). Platform-wide: the outbox has no
// tenant column, and the count goes through the SECURITY DEFINER function of
// migration 000011 because the runtime role cannot SELECT the table.
func (s *PgStore) OutboxUnpublished(ctx context.Context) (int64, error) {
	var n int64
	err := s.pool.QueryRow(ctx, `SELECT outbox_unpublished_count()`).Scan(&n)
	return n, err
}
