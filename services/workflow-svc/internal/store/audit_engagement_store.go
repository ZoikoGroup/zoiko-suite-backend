package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"zoiko.io/workflow-svc/internal/domain"
)

const auditEngagementColumns = `engagement_id, tenant_id, legal_entity_id, engagement_code, engagement_type,
	reporting_period_start, reporting_period_end, framework_profile_id, framework_profile_version,
	methodology_id, methodology_version, responsible_partner_id, scope_summary, acceptance_document_id, acceptance_document_version,
	status, scope_version, created_by_principal_id, created_at, effective_from, effective_to`

func scanAuditEngagement(row pgx.Row) (*domain.AuditEngagement, error) {
	e := &domain.AuditEngagement{}
	err := row.Scan(&e.EngagementID, &e.TenantID, &e.LegalEntityID, &e.EngagementCode, &e.EngagementType,
		&e.ReportingPeriodStart, &e.ReportingPeriodEnd, &e.FrameworkProfileID, &e.FrameworkProfileVersion,
		&e.MethodologyID, &e.MethodologyVersion, &e.ResponsiblePartnerID, &e.ScopeSummary, &e.AcceptanceDocumentID, &e.AcceptanceDocumentVersion,
		&e.Status, &e.ScopeVersion, &e.CreatedByPrincipalID, &e.CreatedAt, &e.EffectiveFrom, &e.EffectiveTo)
	return e, err
}

func (s *PgStore) CreateAuditEngagement(ctx context.Context, p domain.CreateAuditEngagementParams) (*domain.AuditEngagement, bool, error) {
	if p.CorrelationID == "" {
		return nil, false, fmt.Errorf("%w: correlation id is required", domain.ErrStoreUnavailable)
	}
	var out *domain.AuditEngagement
	created := false
	err := s.withRLS(ctx, p.TenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `INSERT INTO audit_engagements (
			engagement_id, tenant_id, legal_entity_id, engagement_code, engagement_type,
			reporting_period_start, reporting_period_end, framework_profile_id, framework_profile_version,
			methodology_id, methodology_version, responsible_partner_id, scope_summary,
			created_by_principal_id, correlation_id)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)
			ON CONFLICT DO NOTHING RETURNING `+auditEngagementColumns,
			uuid.NewString(), p.TenantID, p.LegalEntityID, p.EngagementCode, p.EngagementType,
			p.ReportingPeriodStart, p.ReportingPeriodEnd, p.FrameworkProfileID, p.FrameworkProfileVersion,
			p.MethodologyID, p.MethodologyVersion, p.ResponsiblePartnerID, p.ScopeSummary,
			p.CreatedByPrincipalID, p.CorrelationID)
		var err error
		out, err = scanAuditEngagement(row)
		if err == nil {
			created = true
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		out, err = scanAuditEngagement(tx.QueryRow(ctx, `SELECT `+auditEngagementColumns+` FROM audit_engagements
			WHERE tenant_id=$1 AND correlation_id=$2`, p.TenantID, p.CorrelationID))
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrAuditEngagementDuplicateCode
		}
		return err
	})
	if err != nil {
		if errors.Is(err, domain.ErrAuditEngagementDuplicateCode) {
			return nil, false, err
		}
		return nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, created, nil
}

func (s *PgStore) GetAuditEngagement(ctx context.Context, tenantID, engagementID string) (*domain.AuditEngagement, error) {
	var out *domain.AuditEngagement
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var err error
		out, err = scanAuditEngagement(tx.QueryRow(ctx, `SELECT `+auditEngagementColumns+` FROM audit_engagements
			WHERE engagement_id=$1 AND effective_to IS NULL`, engagementID))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrAuditEngagementNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, nil
}

func (s *PgStore) SubmitAuditEngagementAcceptance(ctx context.Context, p domain.SubmitAuditEngagementAcceptanceParams) (*domain.AuditEngagement, bool, error) {
	if p.EvidenceDocumentID == "" || p.EvidenceDocumentVersion < 1 {
		return nil, false, domain.ErrAuditAcceptanceEvidenceRequired
	}
	if p.CorrelationID == "" {
		return nil, false, fmt.Errorf("%w: correlation id is required", domain.ErrStoreUnavailable)
	}
	var out *domain.AuditEngagement
	changed := false
	err := s.withRLS(ctx, p.TenantID, func(tx pgx.Tx) error {
		var priorEngagementID string
		err := tx.QueryRow(ctx, `SELECT engagement_id FROM audit_engagement_transitions WHERE tenant_id=$1 AND correlation_id=$2`, p.TenantID, p.CorrelationID).Scan(&priorEngagementID)
		if err == nil {
			if priorEngagementID != p.EngagementID {
				return domain.ErrAuditEngagementInvalidState
			}
			out, err = scanAuditEngagement(tx.QueryRow(ctx, `SELECT `+auditEngagementColumns+` FROM audit_engagements WHERE engagement_id=$1`, p.EngagementID))
			return err
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}

		row := tx.QueryRow(ctx, `UPDATE audit_engagements
			SET status=$1, acceptance_document_id=$2, acceptance_document_version=$3
			WHERE engagement_id=$4 AND status=$5 AND effective_to IS NULL RETURNING `+auditEngagementColumns,
			domain.AuditEngagementAcceptanceReview, p.EvidenceDocumentID, p.EvidenceDocumentVersion, p.EngagementID, domain.AuditEngagementProposed)
		out, err = scanAuditEngagement(row)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrAuditEngagementInvalidState
		}
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO audit_engagement_transitions
			(engagement_id, tenant_id, from_status, to_status, actor_principal_id, evidence_document_id, evidence_document_version, correlation_id, occurred_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`, p.EngagementID, p.TenantID,
			domain.AuditEngagementProposed, domain.AuditEngagementAcceptanceReview, p.ActorPrincipalID, p.EvidenceDocumentID, p.EvidenceDocumentVersion, p.CorrelationID, time.Now().UTC())
		if err == nil {
			changed = true
		}
		return err
	})
	if errors.Is(err, domain.ErrAuditEngagementInvalidState) || errors.Is(err, domain.ErrAuditAcceptanceEvidenceRequired) {
		return nil, false, err
	}
	if err != nil {
		return nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, changed, nil
}

// TransitionAuditEngagement performs a compare-and-swap lifecycle transition
// and records the state change in the same transaction. The correlation ID is
// the command idempotency key: a retry returns the post-transition record
// without appending another historical fact.
func (s *PgStore) TransitionAuditEngagement(ctx context.Context, p domain.TransitionAuditEngagementParams) (*domain.AuditEngagement, bool, error) {
	if len(p.ExpectedStatuses) == 0 || p.NextStatus == "" || p.CorrelationID == "" {
		return nil, false, domain.ErrAuditEngagementInvalidState
	}
	var out *domain.AuditEngagement
	changed := false
	err := s.withRLS(ctx, p.TenantID, func(tx pgx.Tx) error {
		var priorEngagementID string
		err := tx.QueryRow(ctx, `SELECT engagement_id FROM audit_engagement_transitions WHERE tenant_id=$1 AND correlation_id=$2`, p.TenantID, p.CorrelationID).Scan(&priorEngagementID)
		if err == nil {
			if priorEngagementID != p.EngagementID {
				return domain.ErrAuditEngagementInvalidState
			}
			out, err = scanAuditEngagement(tx.QueryRow(ctx, `SELECT `+auditEngagementColumns+` FROM audit_engagements WHERE engagement_id=$1`, p.EngagementID))
			return err
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}

		current, err := scanAuditEngagement(tx.QueryRow(ctx, `SELECT `+auditEngagementColumns+` FROM audit_engagements WHERE engagement_id=$1 AND effective_to IS NULL FOR UPDATE`, p.EngagementID))
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrAuditEngagementNotFound
		}
		if err != nil {
			return err
		}
		allowed := false
		for _, status := range p.ExpectedStatuses {
			if current.Status == status {
				allowed = true
				break
			}
		}
		if !allowed {
			return domain.ErrAuditEngagementInvalidState
		}

		out, err = scanAuditEngagement(tx.QueryRow(ctx, `UPDATE audit_engagements SET status=$1 WHERE engagement_id=$2 RETURNING `+auditEngagementColumns, p.NextStatus, p.EngagementID))
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO audit_engagement_transitions
			(engagement_id, tenant_id, from_status, to_status, actor_principal_id, correlation_id, occurred_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7)`, p.EngagementID, p.TenantID, current.Status, p.NextStatus, p.ActorPrincipalID, p.CorrelationID, time.Now().UTC())
		if err == nil {
			changed = true
		}
		return err
	})
	if errors.Is(err, domain.ErrAuditEngagementNotFound) || errors.Is(err, domain.ErrAuditEngagementInvalidState) {
		return nil, false, err
	}
	if err != nil {
		return nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, changed, nil
}

// AmendAuditEngagementScope is the concrete enforcement of "scope/framework
// changes invalidate dependent approvals": in the SAME transaction that
// bumps scope_version, it demotes any APPROVED audit_plans row for this
// engagement back to REVIEWED via UPDATE ... WHERE scope_version_at_approval
// < the new version — a plain SQL statement against a table this service
// already owns, not a cross-service call.
func (s *PgStore) AmendAuditEngagementScope(ctx context.Context, p domain.AmendAuditEngagementScopeParams) (*domain.AuditEngagement, bool, error) {
	if p.CorrelationID == "" {
		return nil, false, fmt.Errorf("%w: correlation id is required", domain.ErrStoreUnavailable)
	}
	var out *domain.AuditEngagement
	changed := false
	err := s.withRLS(ctx, p.TenantID, func(tx pgx.Tx) error {
		var priorEngagementID string
		err := tx.QueryRow(ctx, `SELECT engagement_id FROM audit_engagement_transitions WHERE tenant_id=$1 AND correlation_id=$2`, p.TenantID, p.CorrelationID).Scan(&priorEngagementID)
		if err == nil {
			out, err = scanAuditEngagement(tx.QueryRow(ctx, `SELECT `+auditEngagementColumns+` FROM audit_engagements WHERE engagement_id=$1`, p.EngagementID))
			return err
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}

		current, err := scanAuditEngagement(tx.QueryRow(ctx, `SELECT `+auditEngagementColumns+` FROM audit_engagements WHERE engagement_id=$1 AND effective_to IS NULL FOR UPDATE`, p.EngagementID))
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrAuditEngagementNotFound
		}
		if err != nil {
			return err
		}
		switch current.Status {
		case domain.AuditEngagementProposed, domain.AuditEngagementAcceptanceReview, domain.AuditEngagementAccepted, domain.AuditEngagementActive:
		default:
			return domain.ErrAuditEngagementInvalidState
		}

		row := tx.QueryRow(ctx, `UPDATE audit_engagements
			SET scope_summary=$1, framework_profile_id=$2, framework_profile_version=$3, methodology_id=$4, methodology_version=$5, scope_version=scope_version+1
			WHERE engagement_id=$6 RETURNING `+auditEngagementColumns,
			p.ScopeSummary, p.FrameworkProfileID, p.FrameworkProfileVersion, p.MethodologyID, p.MethodologyVersion, p.EngagementID)
		out, err = scanAuditEngagement(row)
		if err != nil {
			return err
		}

		if _, err := tx.Exec(ctx, `UPDATE audit_plans SET status=$1
			WHERE engagement_id=$2 AND effective_to IS NULL AND status=$3 AND (scope_version_at_approval IS NULL OR scope_version_at_approval < $4)`,
			domain.AuditPlanReviewed, p.EngagementID, domain.AuditPlanApproved, out.ScopeVersion); err != nil {
			return err
		}

		_, err = tx.Exec(ctx, `INSERT INTO audit_engagement_transitions
			(engagement_id, tenant_id, from_status, to_status, actor_principal_id, correlation_id, occurred_at)
			VALUES ($1,$2,$3,$3,$4,$5,$6)`, p.EngagementID, p.TenantID, current.Status, p.ActorPrincipalID, p.CorrelationID, time.Now().UTC())
		if err == nil {
			changed = true
		}
		return err
	})
	if errors.Is(err, domain.ErrAuditEngagementNotFound) || errors.Is(err, domain.ErrAuditEngagementInvalidState) {
		return nil, false, err
	}
	if err != nil {
		return nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, changed, nil
}

// GetAuditEngagementCompletionGates composes the three gate-query methods
// AUD-02/AUD-07/AUD-09 each expose — this is the real "why can't I
// proceed" answer, not a status flag. stage is "FIELDWORK" or "REPORT".
func (s *PgStore) GetAuditEngagementCompletionGates(ctx context.Context, tenantID, engagementID, stage string) ([]domain.CompletionGate, error) {
	switch stage {
	case "FIELDWORK":
		planApproved, noUnresolvedHighRisk, err := s.GetAuditEngagementFieldworkGates(ctx, tenantID, engagementID)
		if err != nil {
			return nil, err
		}
		workpapersLocked, err := s.GetAuditEngagementRequiredWorkpapersLocked(ctx, tenantID, engagementID)
		if err != nil {
			return nil, err
		}
		return []domain.CompletionGate{
			{Name: "plan_approved", Satisfied: planApproved},
			{Name: "no_unresolved_high_risk_coverage_gaps", Satisfied: noUnresolvedHighRisk},
			{Name: "required_workpapers_locked", Satisfied: workpapersLocked},
		}, nil
	case "REPORT":
		allSignOffsValid, noUnresolvedNotes, err := s.GetAuditEngagementReportGates(ctx, tenantID, engagementID)
		if err != nil {
			return nil, err
		}
		return []domain.CompletionGate{
			{Name: "all_sign_offs_valid", Satisfied: allSignOffsValid},
			{Name: "no_unresolved_mandatory_review_notes", Satisfied: noUnresolvedNotes},
		}, nil
	default:
		return nil, fmt.Errorf("%w: unknown completion gate stage %q", domain.ErrStoreUnavailable, stage)
	}
}
