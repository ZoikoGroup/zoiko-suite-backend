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

const auditPlanColumns = `plan_id, engagement_id, tenant_id, version, status, scope_version_at_approval,
	created_by_principal_id, approved_by_principal_id, created_at, effective_from, effective_to`

func scanAuditPlan(row pgx.Row) (*domain.AuditPlan, error) {
	p := &domain.AuditPlan{}
	err := row.Scan(&p.PlanID, &p.EngagementID, &p.TenantID, &p.Version, &p.Status, &p.ScopeVersionAtApproval,
		&p.CreatedByPrincipalID, &p.ApprovedByPrincipalID, &p.CreatedAt, &p.EffectiveFrom, &p.EffectiveTo)
	return p, err
}

// CreateAuditPlan is a real idempotent create — one live plan per
// engagement, enforced by audit_plan_live_per_engagement.
func (s *PgStore) CreateAuditPlan(ctx context.Context, p domain.CreateAuditPlanParams) (*domain.AuditPlan, bool, error) {
	if p.CorrelationID == "" {
		return nil, false, fmt.Errorf("%w: correlation id is required", domain.ErrStoreUnavailable)
	}
	var out *domain.AuditPlan
	created := false
	err := s.withRLS(ctx, p.TenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `INSERT INTO audit_plans (plan_id, engagement_id, tenant_id, created_by_principal_id, correlation_id)
			VALUES ($1,$2,$3,$4,$5) ON CONFLICT DO NOTHING RETURNING `+auditPlanColumns,
			uuid.NewString(), p.EngagementID, p.TenantID, p.CreatedByPrincipalID, p.CorrelationID)
		var err error
		out, err = scanAuditPlan(row)
		if err == nil {
			created = true
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		out, err = scanAuditPlan(tx.QueryRow(ctx, `SELECT `+auditPlanColumns+` FROM audit_plans WHERE tenant_id=$1 AND correlation_id=$2`, p.TenantID, p.CorrelationID))
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrAuditPlanAlreadyExists
		}
		return err
	})
	if err != nil {
		if errors.Is(err, domain.ErrAuditPlanAlreadyExists) {
			return nil, false, err
		}
		return nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, created, nil
}

func (s *PgStore) GetAuditPlan(ctx context.Context, tenantID, planID string) (*domain.AuditPlan, error) {
	var out *domain.AuditPlan
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var err error
		out, err = scanAuditPlan(tx.QueryRow(ctx, `SELECT `+auditPlanColumns+` FROM audit_plans WHERE plan_id=$1 AND effective_to IS NULL`, planID))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrAuditPlanNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, nil
}

// GetAuditPlanByEngagement backs both the AUD-02 "plan for this
// engagement" read and AUD-01's own FIELDWORK completion gate.
func (s *PgStore) GetAuditPlanByEngagement(ctx context.Context, tenantID, engagementID string) (*domain.AuditPlan, error) {
	var out *domain.AuditPlan
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var err error
		out, err = scanAuditPlan(tx.QueryRow(ctx, `SELECT `+auditPlanColumns+` FROM audit_plans WHERE engagement_id=$1 AND effective_to IS NULL`, engagementID))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrAuditPlanNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, nil
}

// RecordMateriality is the real enforcement of "materiality changes
// trigger dependency analysis": against an APPROVED plan, in the SAME
// transaction that inserts the new materiality version, it demotes the
// plan back to REVIEWED and flips every risk still linked to it into
// PENDING_REASSESSMENT — forcing ApprovePlan to run again before
// MarkFieldworkComplete's own gate can pass.
func (s *PgStore) RecordMateriality(ctx context.Context, p domain.RecordMaterialityParams) (*domain.MaterialityRecord, error) {
	if p.Rationale == "" {
		return nil, fmt.Errorf("%w: rationale is required", domain.ErrStoreUnavailable)
	}
	var out *domain.MaterialityRecord
	err := s.withRLS(ctx, p.TenantID, func(tx pgx.Tx) error {
		plan, err := scanAuditPlan(tx.QueryRow(ctx, `SELECT `+auditPlanColumns+` FROM audit_plans WHERE plan_id=$1 AND effective_to IS NULL FOR UPDATE`, p.PlanID))
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrAuditPlanNotFound
		}
		if err != nil {
			return err
		}

		now := time.Now().UTC()
		if _, err := tx.Exec(ctx, `UPDATE materiality_records SET effective_to=$1 WHERE plan_id=$2 AND effective_to IS NULL`, now, p.PlanID); err != nil {
			return err
		}
		row := tx.QueryRow(ctx, `INSERT INTO materiality_records
			(materiality_id, plan_id, tenant_id, overall_materiality, performance_materiality, clearly_trivial_threshold, rationale, created_by_principal_id, effective_from)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
			RETURNING materiality_id, plan_id, tenant_id, overall_materiality, performance_materiality, clearly_trivial_threshold, rationale, created_by_principal_id, created_at, effective_from, effective_to`,
			uuid.NewString(), p.PlanID, p.TenantID, p.OverallMateriality, p.PerformanceMateriality, p.ClearlyTrivialThreshold, p.Rationale, p.ActorPrincipalID, now)
		m := &domain.MaterialityRecord{}
		if err := row.Scan(&m.MaterialityID, &m.PlanID, &m.TenantID, &m.OverallMateriality, &m.PerformanceMateriality,
			&m.ClearlyTrivialThreshold, &m.Rationale, &m.CreatedByPrincipalID, &m.CreatedAt, &m.EffectiveFrom, &m.EffectiveTo); err != nil {
			return err
		}
		out = m

		if plan.Status == domain.AuditPlanApproved {
			if _, err := tx.Exec(ctx, `UPDATE audit_plans SET status=$1 WHERE plan_id=$2`, domain.AuditPlanReviewed, p.PlanID); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE risk_assessments SET coverage_status=$1
				WHERE plan_id=$2 AND effective_to IS NULL AND coverage_status=$3`,
				domain.RiskCoveragePendingReassessment, p.PlanID, domain.RiskCoverageCovered); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, domain.ErrAuditPlanNotFound) {
			return nil, err
		}
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, nil
}

const riskColumns = `risk_id, plan_id, engagement_id, tenant_id, description, risk_level, is_significant, status, coverage_status,
	assessed_by_principal_id, assessed_at, created_by_principal_id, created_at, effective_from, effective_to`

func scanRisk(row pgx.Row) (*domain.RiskAssessment, error) {
	r := &domain.RiskAssessment{}
	err := row.Scan(&r.RiskID, &r.PlanID, &r.EngagementID, &r.TenantID, &r.Description, &r.RiskLevel, &r.IsSignificant, &r.Status, &r.CoverageStatus,
		&r.AssessedByPrincipalID, &r.AssessedAt, &r.CreatedByPrincipalID, &r.CreatedAt, &r.EffectiveFrom, &r.EffectiveTo)
	return r, err
}

func (s *PgStore) IdentifyRisk(ctx context.Context, p domain.IdentifyRiskParams) (*domain.RiskAssessment, bool, error) {
	if p.CorrelationID == "" {
		return nil, false, fmt.Errorf("%w: correlation id is required", domain.ErrStoreUnavailable)
	}
	var out *domain.RiskAssessment
	created := false
	err := s.withRLS(ctx, p.TenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `INSERT INTO risk_assessments (risk_id, plan_id, engagement_id, tenant_id, description, risk_level, created_by_principal_id, correlation_id)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT DO NOTHING RETURNING `+riskColumns,
			uuid.NewString(), p.PlanID, p.EngagementID, p.TenantID, p.Description, p.RiskLevel, p.CreatedByPrincipalID, p.CorrelationID)
		var err error
		out, err = scanRisk(row)
		if err == nil {
			created = true
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		out, err = scanRisk(tx.QueryRow(ctx, `SELECT `+riskColumns+` FROM risk_assessments WHERE tenant_id=$1 AND correlation_id=$2`, p.TenantID, p.CorrelationID))
		return err
	})
	if err != nil {
		return nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, created, nil
}

func (s *PgStore) GetRiskAssessment(ctx context.Context, tenantID, riskID string) (*domain.RiskAssessment, error) {
	var out *domain.RiskAssessment
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var err error
		out, err = scanRisk(tx.QueryRow(ctx, `SELECT `+riskColumns+` FROM risk_assessments WHERE risk_id=$1 AND effective_to IS NULL`, riskID))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrAuditRiskNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, nil
}

func (s *PgStore) ListRisksByPlan(ctx context.Context, tenantID, planID string) ([]*domain.RiskAssessment, error) {
	var out []*domain.RiskAssessment
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+riskColumns+` FROM risk_assessments WHERE plan_id=$1 AND effective_to IS NULL ORDER BY created_at`, planID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			r, err := scanRisk(rows)
			if err != nil {
				return err
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, nil
}

func (s *PgStore) LinkAssertion(ctx context.Context, p domain.LinkAssertionParams) error {
	if p.AssertionCode == "" {
		return fmt.Errorf("%w: assertion_code is required", domain.ErrStoreUnavailable)
	}
	err := s.withRLS(ctx, p.TenantID, func(tx pgx.Tx) error {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM risk_assessments WHERE risk_id=$1 AND effective_to IS NULL)`, p.RiskID).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return domain.ErrAuditRiskNotFound
		}
		_, err := tx.Exec(ctx, `INSERT INTO assertion_links (assertion_link_id, risk_id, tenant_id, assertion_code) VALUES ($1,$2,$3,$4)`,
			uuid.NewString(), p.RiskID, p.TenantID, p.AssertionCode)
		return err
	})
	if errors.Is(err, domain.ErrAuditRiskNotFound) {
		return err
	}
	if err != nil {
		return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return nil
}

func (s *PgStore) DesignAuditResponse(ctx context.Context, p domain.DesignAuditResponseParams) error {
	if p.Description == "" {
		return fmt.Errorf("%w: description is required", domain.ErrStoreUnavailable)
	}
	err := s.withRLS(ctx, p.TenantID, func(tx pgx.Tx) error {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM risk_assessments WHERE risk_id=$1 AND effective_to IS NULL)`, p.RiskID).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return domain.ErrAuditRiskNotFound
		}
		_, err := tx.Exec(ctx, `INSERT INTO planned_procedures (procedure_id, risk_id, tenant_id, description, designed_by_principal_id) VALUES ($1,$2,$3,$4,$5)`,
			uuid.NewString(), p.RiskID, p.TenantID, p.Description, p.DesignedByPrincipalID)
		return err
	})
	if errors.Is(err, domain.ErrAuditRiskNotFound) {
		return err
	}
	if err != nil {
		return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return nil
}

// AssessRisk is the real, DB-enforced form of "every risk links to
// assertions/process and response": the CAS predicate below only matches
// a row that already has at least one assertion_links row and one
// planned_procedures row. Coverage starts PENDING; a HIGH risk only
// reaches COVERED via MarkRiskCovered-equivalent evidence elsewhere (not
// modeled as a separate command in v1 — reassessed via ReviseRiskAssessment
// or left for AUD-01's own completion gate to observe).
func (s *PgStore) AssessRisk(ctx context.Context, p domain.AssessRiskParams) (*domain.RiskAssessment, bool, error) {
	if p.CorrelationID == "" {
		return nil, false, fmt.Errorf("%w: correlation id is required", domain.ErrStoreUnavailable)
	}
	var out *domain.RiskAssessment
	changed := false
	err := s.withRLS(ctx, p.TenantID, func(tx pgx.Tx) error {
		var priorRiskID string
		err := tx.QueryRow(ctx, `SELECT risk_id FROM risk_assessments WHERE tenant_id=$1 AND correlation_id=$2 AND status=$3`, p.TenantID, p.CorrelationID, domain.RiskStatusAssessed).Scan(&priorRiskID)
		if err == nil {
			out, err = scanRisk(tx.QueryRow(ctx, `SELECT `+riskColumns+` FROM risk_assessments WHERE risk_id=$1`, priorRiskID))
			return err
		}

		now := time.Now().UTC()
		row := tx.QueryRow(ctx, `UPDATE risk_assessments SET status=$1, assessed_by_principal_id=$2, assessed_at=$3, correlation_id=$4
			WHERE risk_id=$5 AND effective_to IS NULL AND status=$6
				AND EXISTS(SELECT 1 FROM assertion_links WHERE risk_id=$5)
				AND EXISTS(SELECT 1 FROM planned_procedures WHERE risk_id=$5)
			RETURNING `+riskColumns,
			domain.RiskStatusAssessed, p.ActorPrincipalID, now, p.CorrelationID, p.RiskID, domain.RiskStatusIdentified)
		out, err = scanRisk(row)
		if errors.Is(err, pgx.ErrNoRows) {
			var exists bool
			if chkErr := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM risk_assessments WHERE risk_id=$1 AND effective_to IS NULL)`, p.RiskID).Scan(&exists); chkErr != nil {
				return chkErr
			}
			if !exists {
				return domain.ErrAuditRiskNotFound
			}
			var hasAssertion, hasProcedure bool
			if chkErr := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM assertion_links WHERE risk_id=$1)`, p.RiskID).Scan(&hasAssertion); chkErr != nil {
				return chkErr
			}
			if chkErr := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM planned_procedures WHERE risk_id=$1)`, p.RiskID).Scan(&hasProcedure); chkErr != nil {
				return chkErr
			}
			if !hasAssertion || !hasProcedure {
				return domain.ErrAuditRiskRequiresAssertionAndResponse
			}
			return domain.ErrAuditRiskInvalidState
		}
		if err != nil {
			return err
		}
		changed = true
		return nil
	})
	if errors.Is(err, domain.ErrAuditRiskNotFound) || errors.Is(err, domain.ErrAuditRiskInvalidState) || errors.Is(err, domain.ErrAuditRiskRequiresAssertionAndResponse) {
		return nil, false, err
	}
	if err != nil {
		return nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, changed, nil
}

// MarkSignificantRisk always writes the caller's own principal ID — see
// risk_significant_requires_actor's own CHECK constraint. There is no
// batch/algorithmic path anywhere in this store that can set
// is_significant=true without one.
func (s *PgStore) MarkSignificantRisk(ctx context.Context, p domain.MarkSignificantRiskParams) (*domain.RiskAssessment, error) {
	var out *domain.RiskAssessment
	err := s.withRLS(ctx, p.TenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `UPDATE risk_assessments SET is_significant=true, assessed_by_principal_id=COALESCE(assessed_by_principal_id, $1)
			WHERE risk_id=$2 AND effective_to IS NULL RETURNING `+riskColumns, p.ActorPrincipalID, p.RiskID)
		var err error
		out, err = scanRisk(row)
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrAuditRiskNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, nil
}

// ApprovePlan collapses the doc's own Draft→Prepared→Reviewed step into
// one real transition — no command in AUD-02's own named list reaches
// PREPARED/REVIEWED independently, so approval is the first and only
// gate, same doc-vs-command collapse used throughout this build. Snapshots
// the engagement's current scope_version so a later AmendScope can detect
// staleness.
func (s *PgStore) ApprovePlan(ctx context.Context, p domain.ApprovePlanParams) (*domain.AuditPlan, bool, error) {
	if p.CorrelationID == "" {
		return nil, false, fmt.Errorf("%w: correlation id is required", domain.ErrStoreUnavailable)
	}
	var out *domain.AuditPlan
	changed := false
	err := s.withRLS(ctx, p.TenantID, func(tx pgx.Tx) error {
		var priorPlanID string
		err := tx.QueryRow(ctx, `SELECT plan_id FROM audit_plan_transitions WHERE tenant_id=$1 AND correlation_id=$2`, p.TenantID, p.CorrelationID).Scan(&priorPlanID)
		if err == nil {
			out, err = scanAuditPlan(tx.QueryRow(ctx, `SELECT `+auditPlanColumns+` FROM audit_plans WHERE plan_id=$1`, priorPlanID))
			return err
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}

		current, err := scanAuditPlan(tx.QueryRow(ctx, `SELECT `+auditPlanColumns+` FROM audit_plans WHERE plan_id=$1 AND effective_to IS NULL FOR UPDATE`, p.PlanID))
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrAuditPlanNotFound
		}
		if err != nil {
			return err
		}
		if current.Status == domain.AuditPlanApproved {
			return domain.ErrAuditPlanInvalidState
		}
		if current.CreatedByPrincipalID == p.ActorPrincipalID {
			return domain.ErrAuditPlanSelfApproval
		}

		var scopeVersion int
		if err := tx.QueryRow(ctx, `SELECT scope_version FROM audit_engagements WHERE engagement_id=$1`, current.EngagementID).Scan(&scopeVersion); err != nil {
			return err
		}

		out, err = scanAuditPlan(tx.QueryRow(ctx, `UPDATE audit_plans SET status=$1, approved_by_principal_id=$2, scope_version_at_approval=$3
			WHERE plan_id=$4 RETURNING `+auditPlanColumns,
			domain.AuditPlanApproved, p.ActorPrincipalID, scopeVersion, p.PlanID))
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO audit_plan_transitions (plan_id, tenant_id, from_status, to_status, actor_principal_id, correlation_id)
			VALUES ($1,$2,$3,$4,$5,$6)`, p.PlanID, p.TenantID, current.Status, domain.AuditPlanApproved, p.ActorPrincipalID, p.CorrelationID)
		if err == nil {
			changed = true
		}
		return err
	})
	if errors.Is(err, domain.ErrAuditPlanNotFound) || errors.Is(err, domain.ErrAuditPlanInvalidState) || errors.Is(err, domain.ErrAuditPlanSelfApproval) {
		return nil, false, err
	}
	if err != nil {
		return nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, changed, nil
}

// GetAuditEngagementFieldworkGates is AUD-01's own real completion-gate
// query for the FIELDWORK stage: the plan must be APPROVED and no HIGH
// risk still linked to it may be uncovered.
func (s *PgStore) GetAuditEngagementFieldworkGates(ctx context.Context, tenantID, engagementID string) (planApproved bool, noUnresolvedHighRisk bool, err error) {
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var status *string
		if scanErr := tx.QueryRow(ctx, `SELECT status FROM audit_plans WHERE engagement_id=$1 AND effective_to IS NULL`, engagementID).Scan(&status); scanErr != nil {
			if errors.Is(scanErr, pgx.ErrNoRows) {
				planApproved = false
			} else {
				return scanErr
			}
		} else {
			planApproved = status != nil && *status == domain.AuditPlanApproved
		}
		var unresolvedCount int
		if scanErr := tx.QueryRow(ctx, `SELECT COUNT(*) FROM risk_assessments WHERE engagement_id=$1 AND effective_to IS NULL AND risk_level=$2 AND coverage_status<>$3`,
			engagementID, domain.RiskLevelHigh, domain.RiskCoverageCovered).Scan(&unresolvedCount); scanErr != nil {
			return scanErr
		}
		noUnresolvedHighRisk = unresolvedCount == 0
		return nil
	})
	if err != nil {
		return false, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return planApproved, noUnresolvedHighRisk, nil
}
