package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"zoiko.io/compliance-risk-scoring-svc/internal/domain"
)

// assessmentColumns is the SELECT list scanAssessment expects, in order.
const assessmentColumns = `
	id::text, tenant_id, legal_entity_id, subject_type, subject_id, assessment_name,
	open_obligations_count, policy_violations_count, audit_exceptions_count,
	privacy_incidents_count, tax_penalties_count,
	composite_risk_score::text, risk_tier, model_version, input_hash,
	threshold_source, threshold_rule_id::text, threshold_rule_version,
	moderate_cutoff::text, high_cutoff::text, critical_cutoff::text,
	band_rationale, assurance_state,
	review_status, reviewed_by, reviewed_at, review_note,
	status, previous_score_id::text, score_delta::text, superseded_by_id::text,
	source_references, calculated_by, version,
	evaluated_at, created_at, updated_at, archived_at, archived_by, archive_reason`

func scanAssessment(row pgx.Row) (*domain.RiskScoreAssessment, error) {
	var a domain.RiskScoreAssessment
	var thresholdSource string
	var sourceReferencesJSON []byte

	err := row.Scan(
		&a.ID, &a.TenantID, &a.LegalEntityID, &a.SubjectType, &a.SubjectID, &a.AssessmentName,
		&a.OpenObligationsCount, &a.PolicyViolationsCount, &a.AuditExceptionsCount,
		&a.PrivacyIncidentsCount, &a.TaxPenaltiesCount,
		scanDecimal(&a.CompositeRiskScore), &a.RiskTier, &a.ModelVersion, &a.InputHash,
		&thresholdSource, &a.ThresholdsApplied.RuleID, &a.ThresholdsApplied.RuleVersion,
		scanDecimal(&a.ThresholdsApplied.Cutoffs.Moderate),
		scanDecimal(&a.ThresholdsApplied.Cutoffs.High),
		scanDecimal(&a.ThresholdsApplied.Cutoffs.Critical),
		&a.BandRationale, &a.AssuranceState,
		&a.ReviewStatus, &a.ReviewedBy, &a.ReviewedAt, &a.ReviewNote,
		&a.Status, &a.PreviousScoreID, scanNullableDecimal(&a.ScoreDelta), &a.SupersededByID,
		&sourceReferencesJSON, &a.CalculatedBy, &a.Version,
		&a.EvaluatedAt, &a.CreatedAt, &a.UpdatedAt, &a.ArchivedAt, &a.ArchivedBy, &a.ArchiveReason,
	)
	if err != nil {
		return nil, translatePgError(err)
	}

	a.ThresholdsApplied.Source = domain.ThresholdSource(thresholdSource)
	a.RequiresHumanReview = a.RiskTier.RequiresHumanReview()
	a.SourceReferences = []string{}
	if len(sourceReferencesJSON) > 0 {
		if err := json.Unmarshal(sourceReferencesJSON, &a.SourceReferences); err != nil {
			return nil, fmt.Errorf("decode source_references: %w", err)
		}
	}
	a.FactorBreakdowns = []domain.FactorBreakdown{}
	return &a, nil
}

// InsertAssessment writes a score row and its factor rows. ID, timestamps
// and Version must already be set by the caller.
func (q *TenantQueries) InsertAssessment(ctx context.Context, a *domain.RiskScoreAssessment) error {
	sourceReferencesJSON, err := json.Marshal(a.SourceReferences)
	if err != nil {
		return err
	}

	_, err = q.tx.Exec(ctx, `
		INSERT INTO risk_score_assessments (
			id, tenant_id, legal_entity_id, subject_type, subject_id, assessment_name,
			open_obligations_count, policy_violations_count, audit_exceptions_count,
			privacy_incidents_count, tax_penalties_count,
			composite_risk_score, risk_tier, model_version, input_hash,
			threshold_source, threshold_rule_id, threshold_rule_version,
			moderate_cutoff, high_cutoff, critical_cutoff,
			band_rationale, assurance_state, review_status,
			status, previous_score_id, score_delta, source_references,
			calculated_by, version, evaluated_at, created_at, updated_at
		) VALUES (
			$1, $2, $3, $4, $5, $6,
			$7, $8, $9, $10, $11,
			$12::numeric, $13, $14, $15,
			$16, $17::uuid, $18,
			$19::numeric, $20::numeric, $21::numeric,
			$22, $23, $24,
			$25, $26::uuid, $27::numeric, $28::jsonb,
			$29, $30, $31, $32, $33
		)`,
		a.ID, q.TenantID, a.LegalEntityID, string(a.SubjectType), a.SubjectID, a.AssessmentName,
		a.OpenObligationsCount, a.PolicyViolationsCount, a.AuditExceptionsCount,
		a.PrivacyIncidentsCount, a.TaxPenaltiesCount,
		a.CompositeRiskScore.String(), string(a.RiskTier), a.ModelVersion, a.InputHash,
		string(a.ThresholdsApplied.Source), a.ThresholdsApplied.RuleID, a.ThresholdsApplied.RuleVersion,
		a.ThresholdsApplied.Cutoffs.Moderate.String(), a.ThresholdsApplied.Cutoffs.High.String(),
		a.ThresholdsApplied.Cutoffs.Critical.String(),
		a.BandRationale, a.AssuranceState, string(a.ReviewStatus),
		string(a.Status), a.PreviousScoreID, nullableDecimalText(a.ScoreDelta), string(sourceReferencesJSON),
		a.CalculatedBy, a.Version, a.EvaluatedAt, a.CreatedAt, a.UpdatedAt,
	)
	if err != nil {
		return translatePgError(fmt.Errorf("insert risk_score_assessment: %w", err))
	}

	for i := range a.FactorBreakdowns {
		factor := &a.FactorBreakdowns[i]
		var band *string
		if factor.CategoryThresholdBand != nil {
			bandText := string(*factor.CategoryThresholdBand)
			band = &bandText
		}
		_, err := q.tx.Exec(ctx, `
			INSERT INTO risk_factor_breakdowns (
				id, tenant_id, assessment_id, risk_category, input_count, saturation_count,
				category_weight, raw_score, weighted_score, category_threshold_band,
				risk_driver_summary, created_at
			) VALUES ($1, $2, $3, $4, $5, $6, $7::numeric, $8::numeric, $9::numeric, $10, $11, $12)`,
			factor.ID, q.TenantID, a.ID, string(factor.RiskCategory), factor.InputCount, factor.SaturationCount,
			factor.CategoryWeight.String(), factor.RawScore.String(), factor.WeightedScore.String(), band,
			factor.RiskDriverSummary, factor.CreatedAt,
		)
		if err != nil {
			return translatePgError(fmt.Errorf("insert risk_factor_breakdown: %w", err))
		}
	}
	return nil
}

// GetAssessment loads one score with its factors. lockForUpdate takes a
// row lock for the rest of the transaction (used before a state change).
func (q *TenantQueries) GetAssessment(ctx context.Context, id string, lockForUpdate bool) (*domain.RiskScoreAssessment, error) {
	query := `SELECT ` + assessmentColumns + `
		FROM risk_score_assessments
		WHERE tenant_id = $1 AND id = $2::uuid`
	if lockForUpdate {
		query += ` FOR UPDATE`
	}

	assessment, err := scanAssessment(q.tx.QueryRow(ctx, query, q.TenantID, id))
	if err != nil {
		return nil, err
	}

	factors, err := q.listFactors(ctx, id)
	if err != nil {
		return nil, err
	}
	assessment.FactorBreakdowns = factors
	return assessment, nil
}

func (q *TenantQueries) listFactors(ctx context.Context, assessmentID string) ([]domain.FactorBreakdown, error) {
	rows, err := q.tx.Query(ctx, `
		SELECT id::text, risk_category, COALESCE(input_count, 0), COALESCE(saturation_count, 0),
		       category_weight::text, raw_score::text, weighted_score::text,
		       category_threshold_band, COALESCE(risk_driver_summary, ''), created_at
		FROM risk_factor_breakdowns
		WHERE tenant_id = $1 AND assessment_id = $2::uuid
		ORDER BY category_weight DESC, risk_category`, q.TenantID, assessmentID)
	if err != nil {
		return nil, fmt.Errorf("list factors: %w", err)
	}
	defer rows.Close()

	factors := []domain.FactorBreakdown{}
	for rows.Next() {
		var factor domain.FactorBreakdown
		var band *string
		if err := rows.Scan(
			&factor.ID, &factor.RiskCategory, &factor.InputCount, &factor.SaturationCount,
			scanDecimal(&factor.CategoryWeight), scanDecimal(&factor.RawScore), scanDecimal(&factor.WeightedScore),
			&band, &factor.RiskDriverSummary, &factor.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan factor: %w", err)
		}
		if band != nil {
			tier := domain.RiskTier(*band)
			factor.CategoryThresholdBand = &tier
		}
		factors = append(factors, factor)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate factors: %w", err)
	}
	return factors, nil
}

// FindActiveScoreForSubject returns the subject's ACTIVE score, locked, or
// ErrNotFound when the subject has never been scored (or all its scores are
// archived).
func (q *TenantQueries) FindActiveScoreForSubject(ctx context.Context, subjectType domain.SubjectType, subjectID string) (*domain.RiskScoreAssessment, error) {
	return scanAssessment(q.tx.QueryRow(ctx, `SELECT `+assessmentColumns+`
		FROM risk_score_assessments
		WHERE tenant_id = $1 AND subject_type = $2 AND subject_id = $3 AND status = 'ACTIVE'
		FOR UPDATE`, q.TenantID, string(subjectType), subjectID))
}

// MarkSuperseded moves an ACTIVE score to SUPERSEDED, pointing at its successor.
func (q *TenantQueries) MarkSuperseded(ctx context.Context, id, supersededByID string, now time.Time) error {
	tag, err := q.tx.Exec(ctx, `
		UPDATE risk_score_assessments
		SET status = 'SUPERSEDED', superseded_by_id = $3::uuid, version = version + 1, updated_at = $4
		WHERE tenant_id = $1 AND id = $2::uuid AND status = 'ACTIVE'`,
		q.TenantID, id, supersededByID, now)
	if err != nil {
		return translatePgError(fmt.Errorf("supersede score: %w", err))
	}
	if tag.RowsAffected() != 1 {
		return ErrNotFound
	}
	return nil
}

// RecordReview stores a review disposition.
func (q *TenantQueries) RecordReview(ctx context.Context, id string, status domain.ReviewStatus, reviewerID, note string, now time.Time) error {
	tag, err := q.tx.Exec(ctx, `
		UPDATE risk_score_assessments
		SET review_status = $3, reviewed_by = $4, reviewed_at = $5, review_note = $6,
		    version = version + 1, updated_at = $5
		WHERE tenant_id = $1 AND id = $2::uuid`,
		q.TenantID, id, string(status), reviewerID, now, note)
	if err != nil {
		return fmt.Errorf("record review: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrNotFound
	}
	return nil
}

// ArchiveAssessment moves a score to ARCHIVED. It never deletes.
func (q *TenantQueries) ArchiveAssessment(ctx context.Context, id, archivedBy string, reason *string, now time.Time) error {
	tag, err := q.tx.Exec(ctx, `
		UPDATE risk_score_assessments
		SET status = 'ARCHIVED', archived_at = $3, archived_by = $4, archive_reason = $5,
		    version = version + 1, updated_at = $3
		WHERE tenant_id = $1 AND id = $2::uuid AND status <> 'ARCHIVED'`,
		q.TenantID, id, now, archivedBy, reason)
	if err != nil {
		return fmt.Errorf("archive score: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrNotFound
	}
	return nil
}

// AssessmentFilter narrows ListAssessments. Empty fields do not filter.
type AssessmentFilter struct {
	LegalEntityID string
	RiskTier      string
	Status        string
	ReviewStatus  string
	SubjectType   string
	SubjectID     string
}

// ListAssessments returns one page of scores (without factor rows), newest
// first, plus the cursor for the next page (nil on the last page).
func (q *TenantQueries) ListAssessments(ctx context.Context, filter AssessmentFilter, page PageRequest) ([]domain.RiskScoreAssessment, *PageCursor, error) {
	conditions := []string{"tenant_id = $1"}
	args := []any{q.TenantID}

	addCondition := func(column, value string) {
		if value == "" {
			return
		}
		args = append(args, value)
		conditions = append(conditions, fmt.Sprintf("%s = $%d", column, len(args)))
	}
	addCondition("legal_entity_id", filter.LegalEntityID)
	addCondition("risk_tier", filter.RiskTier)
	addCondition("status", filter.Status)
	addCondition("review_status", filter.ReviewStatus)
	addCondition("subject_type", filter.SubjectType)
	addCondition("subject_id", filter.SubjectID)

	if page.Cursor != nil {
		args = append(args, page.Cursor.CreatedAt, page.Cursor.ID)
		conditions = append(conditions, fmt.Sprintf("(created_at, id) < ($%d, $%d::uuid)", len(args)-1, len(args)))
	}

	args = append(args, page.Limit+1)
	query := `SELECT ` + assessmentColumns + `
		FROM risk_score_assessments
		WHERE ` + strings.Join(conditions, " AND ") + `
		ORDER BY created_at DESC, id DESC
		LIMIT $` + fmt.Sprint(len(args))

	rows, err := q.tx.Query(ctx, query, args...)
	if err != nil {
		return nil, nil, fmt.Errorf("list scores: %w", err)
	}
	defer rows.Close()

	assessments := []domain.RiskScoreAssessment{}
	for rows.Next() {
		assessment, err := scanAssessment(rows)
		if err != nil {
			return nil, nil, fmt.Errorf("scan score: %w", err)
		}
		assessments = append(assessments, *assessment)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("iterate scores: %w", err)
	}

	return trimPage(assessments, page.Limit, func(a domain.RiskScoreAssessment) PageCursor {
		return PageCursor{CreatedAt: a.CreatedAt, ID: a.ID}
	})
}

// trimPage drops the look-ahead row and derives the next cursor from the
// last row kept.
func trimPage[T any](rows []T, limit int, cursorOf func(T) PageCursor) ([]T, *PageCursor, error) {
	if len(rows) <= limit {
		return rows, nil, nil
	}
	kept := rows[:limit]
	next := cursorOf(kept[len(kept)-1])
	return kept, &next, nil
}
