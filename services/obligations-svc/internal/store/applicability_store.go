// Applicability decision persistence — doc7 §E2.
package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"go.uber.org/zap"

	"zoiko.io/obligations-svc/internal/domain"
)

const applicabilityDecisionColumns = `
	applicability_decision_id,
	obligation_id,
	jurisdiction_code,
	entity_ref,
	activity_ref,
	product_process_ref,
	decision,
	source_rule_ref,
	source_rule_version,
	effective_from,
	effective_to,
	facts_used,
	confidence,
	uncertainty_notes,
	decided_by_principal_id,
	decided_by_system,
	review_required,
	created_at,
	created_by_principal_id`

func scanApplicabilityDecision(row pgx.Row) (*domain.ApplicabilityDecision, error) {
	d := &domain.ApplicabilityDecision{}
	err := row.Scan(
		&d.ApplicabilityDecisionID,
		&d.ObligationID,
		&d.JurisdictionCode,
		&d.EntityRef,
		&d.ActivityRef,
		&d.ProductProcessRef,
		&d.Decision,
		&d.SourceRuleRef,
		&d.SourceRuleVersion,
		&d.EffectiveFrom,
		&d.EffectiveTo,
		&d.FactsUsed,
		&d.Confidence,
		&d.UncertaintyNotes,
		&d.DecidedByPrincipalID,
		&d.DecidedBySystem,
		&d.ReviewRequired,
		&d.CreatedAt,
		&d.CreatedByPrincipalID,
	)
	return d, err
}

// CreateApplicabilityDecision records a new applicability decision.
// Append-only — validates the parent obligation exists first, same pattern
// as CreateFilingRequirement.
func (s *PgStore) CreateApplicabilityDecision(ctx context.Context, params domain.CreateApplicabilityDecisionParams) (*domain.ApplicabilityDecision, error) {
	if _, err := s.FindObligationByID(ctx, params.ObligationID); err != nil {
		return nil, err
	}
	if params.ApplicabilityDecisionID == "" {
		params.ApplicabilityDecisionID = uuid.New().String()
	}
	if len(params.FactsUsed) == 0 {
		params.FactsUsed = []byte(`{}`)
	}

	const query = `
		INSERT INTO applicability_decisions (
			applicability_decision_id, tenant_id, obligation_id, jurisdiction_code, entity_ref,
			activity_ref, product_process_ref, decision, source_rule_ref, source_rule_version,
			effective_from, effective_to, facts_used, confidence, uncertainty_notes,
			decided_by_principal_id, decided_by_system, review_required, created_by_principal_id
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19)
		RETURNING ` + applicabilityDecisionColumns + `;`

	var d *domain.ApplicabilityDecision
	err := s.withTenantTx(ctx, func(tx pgx.Tx, tenantID string) error {
		var scanErr error
		d, scanErr = scanApplicabilityDecision(tx.QueryRow(ctx, query,
			params.ApplicabilityDecisionID, tenantID, params.ObligationID, params.JurisdictionCode, params.EntityRef,
			nullableStr(params.ActivityRef), nullableStr(params.ProductProcessRef), params.Decision, params.SourceRuleRef, params.SourceRuleVersion,
			params.EffectiveFrom, params.EffectiveTo, params.FactsUsed, params.Confidence, nullableStr(params.UncertaintyNotes),
			nullableStr(params.DecidedByPrincipalID), nullableStr(params.DecidedBySystem), params.ReviewRequired, params.CreatedByPrincipalID,
		))
		if scanErr != nil {
			s.log.Error("pg CreateApplicabilityDecision failed", zap.Error(scanErr))
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, scanErr)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return d, nil
}

// ListApplicabilityDecisions returns every decision ever recorded for an
// (obligation_id, jurisdiction_code, entity_ref) scope, newest first —
// doc7 §E2's full versioned history, not just the current answer.
func (s *PgStore) ListApplicabilityDecisions(ctx context.Context, obligationID, jurisdictionCode, entityRef string) ([]*domain.ApplicabilityDecision, error) {
	if _, err := s.FindObligationByID(ctx, obligationID); err != nil {
		return nil, err
	}

	const query = `
		SELECT ` + applicabilityDecisionColumns + `
		FROM applicability_decisions
		WHERE obligation_id = $1 AND jurisdiction_code = $2 AND entity_ref = $3 AND tenant_id = $4
		ORDER BY effective_from DESC, created_at DESC;`

	var out []*domain.ApplicabilityDecision
	err := s.withTenantTx(ctx, func(tx pgx.Tx, tenantID string) error {
		rows, qErr := tx.Query(ctx, query, obligationID, jurisdictionCode, entityRef, tenantID)
		if qErr != nil {
			s.log.Error("pg ListApplicabilityDecisions failed", zap.Error(qErr))
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, qErr)
		}
		defer rows.Close()

		for rows.Next() {
			d, scanErr := scanApplicabilityDecision(rows)
			if scanErr != nil {
				s.log.Error("pg ListApplicabilityDecisions scan failed", zap.Error(scanErr))
				return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, scanErr)
			}
			out = append(out, d)
		}
		if rowsErr := rows.Err(); rowsErr != nil {
			s.log.Error("pg ListApplicabilityDecisions rows error", zap.Error(rowsErr))
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, rowsErr)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// FindCurrentApplicability answers doc7 §E2's core question for a scope
// right now: the most recent decision whose effective window covers now,
// or UNASSESSED if no decision row exists at all — deliberately distinct
// from a stored NOT_APPLICABLE decision (see domain package doc comment).
func (s *PgStore) FindCurrentApplicability(ctx context.Context, obligationID, jurisdictionCode, entityRef string) (*domain.CurrentApplicability, error) {
	if _, err := s.FindObligationByID(ctx, obligationID); err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	const query = `
		SELECT ` + applicabilityDecisionColumns + `
		FROM applicability_decisions
		WHERE obligation_id = $1 AND jurisdiction_code = $2 AND entity_ref = $3
		  AND effective_from <= $4 AND (effective_to IS NULL OR effective_to > $4)
		  AND tenant_id = $5
		ORDER BY effective_from DESC, created_at DESC
		LIMIT 1;`

	var out *domain.CurrentApplicability
	err := s.withTenantTx(ctx, func(tx pgx.Tx, tenantID string) error {
		d, scanErr := scanApplicabilityDecision(tx.QueryRow(ctx, query, obligationID, jurisdictionCode, entityRef, now, tenantID))
		if errors.Is(scanErr, pgx.ErrNoRows) {
			out = &domain.CurrentApplicability{
				ObligationID:     obligationID,
				JurisdictionCode: jurisdictionCode,
				EntityRef:        entityRef,
				Status:           "UNASSESSED",
			}
			return nil
		}
		if scanErr != nil {
			s.log.Error("pg FindCurrentApplicability failed", zap.Error(scanErr))
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, scanErr)
		}
		out = &domain.CurrentApplicability{
			ObligationID:     obligationID,
			JurisdictionCode: jurisdictionCode,
			EntityRef:        entityRef,
			Status:           d.Decision,
			Decision:         d,
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func nullableStr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
