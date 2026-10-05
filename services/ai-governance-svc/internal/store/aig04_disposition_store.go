package store

import (
	"errors"
	"fmt"
	"time"

	"context"

	"github.com/jackc/pgx/v5"

	"zoiko.io/ai-governance-svc/internal/domain"
	"zoiko.io/ai-governance-svc/internal/middleware"
)

// AIG-04 Human Oversight, Output Disposition & Decision Boundary
// Service (ZS-SVC-X-001 §7). Additive alongside AIG-01/02; see
// migration 000005_aig04_output_disposition.

const dispositionColumns = `disposition_id, tenant_id, use_case_id, execution_ref, oversight_class, disposition_state,
	validation_failure_reason, required_reviewer_role, reviewer_principal_id, decision_reason, downstream_action_ref,
	rapid_decision_flag, superseded_by_disposition_id, created_at, created_by_principal_id, decided_at`

func scanDisposition(row pgx.Row) (*domain.OutputDisposition, error) {
	var d domain.OutputDisposition
	var oversightClass, state string
	var validationFailureReason, requiredReviewerRole, reviewerPrincipalID, decisionReason, downstreamActionRef, supersededBy *string
	if err := row.Scan(&d.DispositionID, &d.TenantID, &d.UseCaseID, &d.ExecutionRef, &oversightClass, &state,
		&validationFailureReason, &requiredReviewerRole, &reviewerPrincipalID, &decisionReason, &downstreamActionRef,
		&d.RapidDecisionFlag, &supersededBy, &d.CreatedAt, &d.CreatedByPrincipalID, &d.DecidedAt); err != nil {
		return nil, err
	}
	d.OversightClass = domain.OversightClass(oversightClass)
	d.DispositionState = domain.DispositionState(state)
	if validationFailureReason != nil {
		d.ValidationFailureReason = *validationFailureReason
	}
	if requiredReviewerRole != nil {
		d.RequiredReviewerRole = *requiredReviewerRole
	}
	if reviewerPrincipalID != nil {
		d.ReviewerPrincipalID = *reviewerPrincipalID
	}
	if decisionReason != nil {
		d.DecisionReason = *decisionReason
	}
	if downstreamActionRef != nil {
		d.DownstreamActionRef = *downstreamActionRef
	}
	if supersededBy != nil {
		d.SupersededByDispositionID = *supersededBy
	}
	return &d, nil
}

// CreateDisposition submits an AI output for disposition. The
// oversight class and initial state are computed from the use case's
// own operational_class (never caller-supplied), so a caller cannot
// assert its own output needs less oversight than its use case
// actually requires.
func (s *PgStore) CreateDisposition(ctx context.Context, req domain.CreateDispositionRequest, actor string) (*domain.OutputDisposition, error) {
	if req.UseCaseID == "" || req.ExecutionRef == "" {
		return nil, fmt.Errorf("use_case_id and execution_ref are required")
	}
	var out *domain.OutputDisposition
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		tenantID := middleware.TenantFromContext(ctx)
		uc, err := loadUseCaseForUpdate(ctx, tx, tenantID, req.UseCaseID)
		if err != nil {
			return err
		}
		if uc.LifecycleState != domain.UseCaseActive && uc.LifecycleState != domain.UseCaseLimited {
			return domain.ErrUseCaseNotActiveForOutput
		}

		oversightClass := domain.DetermineOversightClass(uc.OperationalClass, req.RequireDualControl)

		var state domain.DispositionState
		var validationFailureReason *string
		if req.ValidationFailureReason != "" {
			state = domain.DispositionBlocked
			validationFailureReason = &req.ValidationFailureReason
		} else {
			state = domain.InitialDispositionState(oversightClass)
		}

		var requiredRole *string
		if req.RequiredReviewerRole != "" {
			requiredRole = &req.RequiredReviewerRole
		}

		d, err := scanDisposition(tx.QueryRow(ctx, `
			INSERT INTO output_dispositions (tenant_id, use_case_id, execution_ref, oversight_class, disposition_state,
				validation_failure_reason, required_reviewer_role, created_by_principal_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
			RETURNING `+dispositionColumns,
			tenantID, req.UseCaseID, req.ExecutionRef, string(oversightClass), string(state),
			validationFailureReason, requiredRole, actor))
		if err != nil {
			return fmt.Errorf("insert output disposition: %w", err)
		}
		out = d
		return nil
	})
	return out, err
}

func (s *PgStore) GetDisposition(ctx context.Context, dispositionID string) (*domain.OutputDisposition, error) {
	var out *domain.OutputDisposition
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		d, err := scanDisposition(tx.QueryRow(ctx, `SELECT `+dispositionColumns+` FROM output_dispositions WHERE disposition_id = $1 AND tenant_id = $2`,
			dispositionID, middleware.TenantFromContext(ctx)))
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrDispositionNotFound
		}
		out = d
		return err
	})
	return out, err
}

func loadDispositionForUpdate(ctx context.Context, tx pgx.Tx, tenantID, dispositionID string) (*domain.OutputDisposition, error) {
	d, err := scanDisposition(tx.QueryRow(ctx, `SELECT `+dispositionColumns+` FROM output_dispositions WHERE disposition_id = $1 AND tenant_id = $2 FOR UPDATE`,
		dispositionID, tenantID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrDispositionNotFound
	}
	return d, err
}

// DecideDisposition is the ONLY path that can move a disposition to
// ACCEPTED or REJECTED. For O2/O3 (qualified/dual-control review), the
// reviewer must differ from whoever submitted the output — §7.2's
// "no self-bypass." rapid_decision_flag records (never blocks) a
// decision made within one second of submission, per NP-37.
func (s *PgStore) DecideDisposition(ctx context.Context, dispositionID string, req domain.DecideDispositionRequest, reviewer string) (*domain.OutputDisposition, error) {
	decision := domain.DispositionState(req.Decision)
	if decision != domain.DispositionAccepted && decision != domain.DispositionRejected {
		return nil, domain.ErrInvalidDispositionDecision
	}
	var out *domain.OutputDisposition
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		tenantID := middleware.TenantFromContext(ctx)
		d, err := loadDispositionForUpdate(ctx, tx, tenantID, dispositionID)
		if err != nil {
			return err
		}
		if d.DispositionState != domain.DispositionDraftAssistive && d.DispositionState != domain.DispositionReviewRequired {
			return domain.ErrDispositionNotDecidable
		}
		if (d.OversightClass == domain.OversightQualifiedReview || d.OversightClass == domain.OversightDualControl) &&
			reviewer == d.CreatedByPrincipalID {
			return domain.ErrReviewerCannotBeSubmitter
		}

		now := time.Now().UTC()
		rapid := now.Sub(d.CreatedAt) < time.Second

		var reasonPtr *string
		if req.Reason != "" {
			reasonPtr = &req.Reason
		}

		updated, err := scanDisposition(tx.QueryRow(ctx, `
			UPDATE output_dispositions SET disposition_state = $2, reviewer_principal_id = $3, decision_reason = $4,
				decided_at = $5, rapid_decision_flag = $6
			WHERE disposition_id = $1 RETURNING `+dispositionColumns,
			dispositionID, string(decision), reviewer, reasonPtr, now, rapid))
		if err != nil {
			return fmt.Errorf("decide output disposition: %w", err)
		}
		out = updated
		return nil
	})
	return out, err
}

// SupersedeDisposition replaces a decided disposition with a fresh
// one, in the same transaction — never an in-place rewrite of the
// original decision.
func (s *PgStore) SupersedeDisposition(ctx context.Context, dispositionID string, req domain.SupersedeDispositionRequest, actor string) (*domain.OutputDisposition, error) {
	if req.ExecutionRef == "" {
		return nil, fmt.Errorf("execution_ref is required")
	}
	var out *domain.OutputDisposition
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		tenantID := middleware.TenantFromContext(ctx)
		old, err := loadDispositionForUpdate(ctx, tx, tenantID, dispositionID)
		if err != nil {
			return err
		}
		if old.DispositionState != domain.DispositionAccepted && old.DispositionState != domain.DispositionRejected {
			return domain.ErrDispositionNotSupersedable
		}

		uc, err := loadUseCaseForUpdate(ctx, tx, tenantID, old.UseCaseID)
		if err != nil {
			return err
		}
		oversightClass := domain.DetermineOversightClass(uc.OperationalClass, req.RequireDualControl)

		var state domain.DispositionState
		var validationFailureReason *string
		if req.ValidationFailureReason != "" {
			state = domain.DispositionBlocked
			validationFailureReason = &req.ValidationFailureReason
		} else {
			state = domain.InitialDispositionState(oversightClass)
		}
		var requiredRole *string
		if req.RequiredReviewerRole != "" {
			requiredRole = &req.RequiredReviewerRole
		}

		newD, err := scanDisposition(tx.QueryRow(ctx, `
			INSERT INTO output_dispositions (tenant_id, use_case_id, execution_ref, oversight_class, disposition_state,
				validation_failure_reason, required_reviewer_role, created_by_principal_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
			RETURNING `+dispositionColumns,
			tenantID, old.UseCaseID, req.ExecutionRef, string(oversightClass), string(state),
			validationFailureReason, requiredRole, actor))
		if err != nil {
			return fmt.Errorf("insert superseding disposition: %w", err)
		}

		if _, err := tx.Exec(ctx, `UPDATE output_dispositions SET disposition_state = 'SUPERSEDED', superseded_by_disposition_id = $2 WHERE disposition_id = $1`,
			dispositionID, newD.DispositionID); err != nil {
			return fmt.Errorf("mark disposition superseded: %w", err)
		}
		out = newD
		return nil
	})
	return out, err
}
