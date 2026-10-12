package store

import (
	"context"
	"errors"
	"math"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"zoiko.io/project-accounting-svc/internal/domain"
	svcmiddleware "zoiko.io/project-accounting-svc/internal/middleware"
)

const milestoneColumns = `
	milestone_id, project_id, name, amount, status, achievement_evidence_ref,
	achieved_at, achieved_by_principal_id, approved_at, approved_by_principal_id,
	created_at, created_by_principal_id`

func scanMilestone(row pgx.Row) (*domain.Milestone, error) {
	var m domain.Milestone
	var evidence *string
	if err := row.Scan(&m.MilestoneID, &m.ProjectID, &m.Name, &m.Amount, &m.Status, &evidence,
		&m.AchievedAt, &m.AchievedByPrincipalID, &m.ApprovedAt, &m.ApprovedByPrincipalID,
		&m.CreatedAt, &m.CreatedByPrincipalID); err != nil {
		return nil, err
	}
	if evidence != nil {
		m.AchievementEvidenceRef = *evidence
	}
	return &m, nil
}

// DefineMilestone inserts a PLANNED milestone. UNIQUE(tenant, project, name)
// refuses a duplicate name.
func (s *PgStore) DefineMilestone(ctx context.Context, m *domain.Milestone) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO project_milestones (milestone_id, tenant_id, project_id, name, amount, status, created_at, created_by_principal_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		`, m.MilestoneID, tenantID, m.ProjectID, m.Name, m.Amount, domain.MilestoneStatusPlanned, m.CreatedAt, m.CreatedByPrincipalID)
		if err != nil {
			if isUniqueViolation(err) {
				return domain.ErrDuplicateMilestoneName
			}
			return err
		}
		m.Status = domain.MilestoneStatusPlanned
		return nil
	})
}

func (s *PgStore) GetMilestone(ctx context.Context, milestoneID string) (*domain.Milestone, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}
	var m *domain.Milestone
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var err error
		m, err = scanMilestone(tx.QueryRow(ctx, `SELECT `+milestoneColumns+` FROM project_milestones WHERE milestone_id = $1 AND tenant_id = $2`, milestoneID, tenantID))
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrMilestoneNotFound
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	return m, nil
}

func (s *PgStore) ListMilestones(ctx context.Context, projectID string) ([]domain.Milestone, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}
	var out []domain.Milestone
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+milestoneColumns+` FROM project_milestones WHERE tenant_id = $1 AND project_id = $2 ORDER BY created_at, name`, tenantID, projectID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			m, err := scanMilestone(rows)
			if err != nil {
				return err
			}
			out = append(out, *m)
		}
		return rows.Err()
	})
	return out, err
}

// MarkMilestoneAchieved moves PLANNED -> ACHIEVED. Evidence is mandatory.
// It does NOT make the milestone count toward revenue — that needs a
// separate approval by a different principal.
func (s *PgStore) MarkMilestoneAchieved(ctx context.Context, milestoneID, principalID, evidenceRef string, at time.Time) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}
	if strings.TrimSpace(evidenceRef) == "" {
		return domain.ErrMilestoneEvidenceRequired
	}
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var status string
		err := tx.QueryRow(ctx, `SELECT status FROM project_milestones WHERE milestone_id = $1 AND tenant_id = $2 FOR UPDATE`, milestoneID, tenantID).Scan(&status)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrMilestoneNotFound
		}
		if err != nil {
			return err
		}
		if status != domain.MilestoneStatusPlanned {
			return domain.ErrInvalidMilestoneTransition
		}
		_, err = tx.Exec(ctx, `
			UPDATE project_milestones SET status = $1, achievement_evidence_ref = $2, achieved_at = $3, achieved_by_principal_id = $4
			WHERE milestone_id = $5 AND tenant_id = $6
		`, domain.MilestoneStatusAchieved, evidenceRef, at, principalID, milestoneID, tenantID)
		return err
	})
}

// ApproveMilestoneAchievement records the approval that makes an ACHIEVED
// milestone count toward revenue. The approver must differ from the
// principal who marked it achieved (SoD).
func (s *PgStore) ApproveMilestoneAchievement(ctx context.Context, milestoneID, principalID string, at time.Time) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var status string
		var achievedBy *string
		var approvedAt *time.Time
		err := tx.QueryRow(ctx, `
			SELECT status, achieved_by_principal_id, approved_at FROM project_milestones
			WHERE milestone_id = $1 AND tenant_id = $2 FOR UPDATE
		`, milestoneID, tenantID).Scan(&status, &achievedBy, &approvedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrMilestoneNotFound
		}
		if err != nil {
			return err
		}
		if status != domain.MilestoneStatusAchieved || approvedAt != nil {
			return domain.ErrInvalidMilestoneTransition
		}
		if achievedBy != nil && *achievedBy == principalID {
			return domain.ErrSelfApprovalNotPermittedMilestone
		}
		_, err = tx.Exec(ctx, `
			UPDATE project_milestones SET approved_at = $1, approved_by_principal_id = $2
			WHERE milestone_id = $3 AND tenant_id = $4
		`, at, principalID, milestoneID, tenantID)
		return err
	})
}

// ListRunMilestones returns the insert-only evidence of which milestones a
// recognition run was built from.
func (s *PgStore) ListRunMilestones(ctx context.Context, runID string) ([]domain.RunMilestone, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}
	var out []domain.RunMilestone
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT run_id, milestone_id, amount, included_at FROM project_recognition_run_milestones WHERE tenant_id = $1 AND run_id = $2 ORDER BY milestone_id`, tenantID, runID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var rm domain.RunMilestone
			if err := rows.Scan(&rm.RunID, &rm.MilestoneID, &rm.Amount, &rm.IncludedAt); err != nil {
				return err
			}
			out = append(out, rm)
		}
		return rows.Err()
	})
	return out, err
}

// freezeMilestoneRun is FreezeAndCalculate's MILESTONE branch (called inside
// its transaction). cumulative revenue = the sum of milestones that are
// ACHIEVED and APPROVED at this instant — read with FOR SHARE so a
// concurrent approval cannot slip in between the sum and the evidence rows.
// percent_complete, period revenue, cost, margin and balance follow the
// percentage-of-completion branch's rules exactly; no estimate-to-complete
// is required and none is recorded.
func freezeMilestoneRun(ctx context.Context, tx pgx.Tx, tenantID, runID, projectID string, contractValue float64, billedToDate *float64, at time.Time) error {
	rows, err := tx.Query(ctx, `
		SELECT milestone_id, amount FROM project_milestones
		WHERE tenant_id = $1 AND project_id = $2 AND status = 'ACHIEVED' AND approved_at IS NOT NULL
		ORDER BY milestone_id FOR SHARE
	`, tenantID, projectID)
	if err != nil {
		return err
	}
	var ids []string
	var amounts []float64
	var total float64
	for rows.Next() {
		var id string
		var amt float64
		if err := rows.Scan(&id, &amt); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
		amounts = append(amounts, amt)
		total += amt
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	cumulativeRevenue := roundCentsSigned(total)
	if cumulativeRevenue > contractValue {
		return domain.ErrMilestonesExceedContractValue
	}

	var itdCost float64
	if err := tx.QueryRow(ctx, `
		SELECT COALESCE(SUM(amount), 0) FROM project_cost_entries WHERE tenant_id = $1 AND project_id = $2 AND status != 'REVERSED'
	`, tenantID, projectID).Scan(&itdCost); err != nil {
		return err
	}
	var priorCumulative float64
	if err := tx.QueryRow(ctx, `
		SELECT COALESCE(MAX(cumulative_recognized_revenue), 0) FROM project_recognition_runs
		WHERE tenant_id = $1 AND project_id = $2 AND status NOT IN ($3, $4, $5)
	`, tenantID, projectID, domain.RecognitionRunStatusDraft, domain.RecognitionRunStatusPopulationFrozen, domain.RecognitionRunStatusSuperseded).Scan(&priorCumulative); err != nil {
		return err
	}

	percentComplete := cumulativeRevenue / contractValue
	periodRevenue := roundCentsSigned(cumulativeRevenue - priorCumulative)
	recognizedCost := roundCentsSigned(itdCost)
	margin := roundCentsSigned(cumulativeRevenue - recognizedCost)
	billed := 0.0
	if billedToDate != nil {
		billed = *billedToDate
	}
	balanceAmount := roundCentsSigned(cumulativeRevenue - billed)
	balanceType := domain.BalanceTypeNone
	if balanceAmount > 0 {
		balanceType = domain.BalanceTypeContractAsset
	} else if balanceAmount < 0 {
		balanceType = domain.BalanceTypeContractLiability
	}

	tag, err := tx.Exec(ctx, `
		UPDATE project_recognition_runs SET
			status = $1, frozen_at = $2, calculated_at = $2,
			itd_cost_incurred = $3, percent_complete = $4,
			cumulative_recognized_revenue = $5, period_recognized_revenue = $6, recognized_cost = $7, margin = $8,
			balance_type = $9, balance_amount = $10
		WHERE run_id = $11 AND status = $12 AND tenant_id = $13
	`, domain.RecognitionRunStatusCalculated, at, itdCost, percentComplete,
		cumulativeRevenue, periodRevenue, recognizedCost, margin,
		balanceType, balanceAmount, runID, domain.RecognitionRunStatusDraft, tenantID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrInvalidRecognitionRunTransition
	}
	for i, id := range ids {
		if _, err := tx.Exec(ctx, `
			INSERT INTO project_recognition_run_milestones (run_id, milestone_id, tenant_id, amount, included_at)
			VALUES ($1, $2, $3, $4, $5)
		`, runID, id, tenantID, amounts[i], at); err != nil {
			return err
		}
	}
	return nil
}

// currentRecognitionMethod returns the recognition_method of the project's
// current financial profile, or "" when none is set (treated as
// percentage-of-completion by the caller, i.e. unchanged legacy behaviour).
func currentRecognitionMethod(ctx context.Context, tx pgx.Tx, tenantID, projectID string) (string, error) {
	var method string
	err := tx.QueryRow(ctx, `
		SELECT recognition_method FROM project_financial_profiles
		WHERE tenant_id = $1 AND project_id = $2 AND effective_to IS NULL
	`, tenantID, projectID).Scan(&method)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return method, err
}

// roundCentsSigned rounds to the nearest cent, symmetric for negatives.
// The shared roundCents truncates toward zero after adding 0.5, which turns
// -600.00 into -599.99; milestone runs routinely produce negative balances
// (billed ahead of recognition = contract liability), so they use this. Same
// result as roundCents for every non-negative input.
func roundCentsSigned(v float64) float64 {
	return math.Round(v*100) / 100
}
