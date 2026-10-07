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

const reviewScopeColumns = `review_scope_id, engagement_id, tenant_id, target_type, target_id, initiated_by_principal_id, created_at`

func scanReviewScope(row pgx.Row) (*domain.ReviewScope, error) {
	rs := &domain.ReviewScope{}
	err := row.Scan(&rs.ReviewScopeID, &rs.EngagementID, &rs.TenantID, &rs.TargetType, &rs.TargetID, &rs.InitiatedByPrincipalID, &rs.CreatedAt)
	return rs, err
}

func (s *PgStore) OpenReview(ctx context.Context, p domain.OpenReviewParams) (*domain.ReviewScope, bool, error) {
	if p.CorrelationID == "" {
		return nil, false, fmt.Errorf("%w: correlation id is required", domain.ErrStoreUnavailable)
	}
	if p.InitiatedByPrincipalID == "" {
		return nil, false, fmt.Errorf("%w: initiated_by_principal_id is required", domain.ErrStoreUnavailable)
	}
	var out *domain.ReviewScope
	created := false
	err := s.withRLS(ctx, p.TenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `INSERT INTO review_scopes (review_scope_id, engagement_id, tenant_id, target_type, target_id, initiated_by_principal_id, correlation_id)
			VALUES ($1,$2,$3,$4,$5,$6,$7) ON CONFLICT DO NOTHING RETURNING `+reviewScopeColumns,
			uuid.NewString(), p.EngagementID, p.TenantID, p.TargetType, p.TargetID, p.InitiatedByPrincipalID, p.CorrelationID)
		var err error
		out, err = scanReviewScope(row)
		if err == nil {
			created = true
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		out, err = scanReviewScope(tx.QueryRow(ctx, `SELECT `+reviewScopeColumns+` FROM review_scopes WHERE tenant_id=$1 AND correlation_id=$2`, p.TenantID, p.CorrelationID))
		if errors.Is(err, pgx.ErrNoRows) {
			out, err = scanReviewScope(tx.QueryRow(ctx, `SELECT `+reviewScopeColumns+` FROM review_scopes WHERE engagement_id=$1 AND target_type=$2 AND target_id=$3`, p.EngagementID, p.TargetType, p.TargetID))
		}
		return err
	})
	if err != nil {
		return nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, created, nil
}

func (s *PgStore) GetReviewScope(ctx context.Context, tenantID, reviewScopeID string) (*domain.ReviewScope, error) {
	var out *domain.ReviewScope
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var err error
		out, err = scanReviewScope(tx.QueryRow(ctx, `SELECT `+reviewScopeColumns+` FROM review_scopes WHERE review_scope_id=$1`, reviewScopeID))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrReviewScopeNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, nil
}

// AssignReviewer refuses "no self-review where prohibited" at assignment
// time — the earliest point this can be caught.
func (s *PgStore) AssignReviewer(ctx context.Context, p domain.AssignReviewerParams) (*domain.ReviewAssignment, error) {
	if p.ReviewerPrincipalID == "" || p.Role == "" {
		return nil, fmt.Errorf("%w: reviewer_principal_id and role are required", domain.ErrStoreUnavailable)
	}
	var out *domain.ReviewAssignment
	err := s.withRLS(ctx, p.TenantID, func(tx pgx.Tx) error {
		var initiatedBy string
		if err := tx.QueryRow(ctx, `SELECT initiated_by_principal_id FROM review_scopes WHERE review_scope_id=$1`, p.ReviewScopeID).Scan(&initiatedBy); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrReviewScopeNotFound
			}
			return err
		}
		if initiatedBy == p.ReviewerPrincipalID {
			return domain.ErrSelfReviewNotAllowed
		}
		row := tx.QueryRow(ctx, `INSERT INTO review_assignments (assignment_id, review_scope_id, tenant_id, reviewer_principal_id, role)
			VALUES ($1,$2,$3,$4,$5) RETURNING assignment_id, review_scope_id, tenant_id, reviewer_principal_id, role, created_at`,
			uuid.NewString(), p.ReviewScopeID, p.TenantID, p.ReviewerPrincipalID, p.Role)
		a := &domain.ReviewAssignment{}
		if err := row.Scan(&a.AssignmentID, &a.ReviewScopeID, &a.TenantID, &a.ReviewerPrincipalID, &a.Role, &a.CreatedAt); err != nil {
			return err
		}
		out = a
		return nil
	})
	if errors.Is(err, domain.ErrReviewScopeNotFound) || errors.Is(err, domain.ErrSelfReviewNotAllowed) {
		return nil, err
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, nil
}

const reviewNoteColumns = `review_note_id, review_scope_id, tenant_id, raised_by_principal_id, body, mandatory, status, response_body, resolved_by_principal_id, created_at, resolved_at`

func scanReviewNote(row pgx.Row) (*domain.ReviewNote, error) {
	n := &domain.ReviewNote{}
	err := row.Scan(&n.ReviewNoteID, &n.ReviewScopeID, &n.TenantID, &n.RaisedByPrincipalID, &n.Body, &n.Mandatory, &n.Status, &n.ResponseBody, &n.ResolvedByPrincipalID, &n.CreatedAt, &n.ResolvedAt)
	return n, err
}

func (s *PgStore) RaiseReviewNote(ctx context.Context, p domain.RaiseReviewNoteParams) (*domain.ReviewNote, error) {
	if p.Body == "" {
		return nil, fmt.Errorf("%w: body is required", domain.ErrStoreUnavailable)
	}
	var out *domain.ReviewNote
	err := s.withRLS(ctx, p.TenantID, func(tx pgx.Tx) error {
		var err error
		out, err = scanReviewNote(tx.QueryRow(ctx, `INSERT INTO review_notes (review_note_id, review_scope_id, tenant_id, raised_by_principal_id, body, mandatory)
			VALUES ($1,$2,$3,$4,$5,$6) RETURNING `+reviewNoteColumns,
			uuid.NewString(), p.ReviewScopeID, p.TenantID, p.RaisedByPrincipalID, p.Body, p.Mandatory))
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, nil
}

func (s *PgStore) RespondToReviewNote(ctx context.Context, p domain.RespondToReviewNoteParams) (*domain.ReviewNote, error) {
	if p.ResponseBody == "" {
		return nil, fmt.Errorf("%w: response_body is required", domain.ErrStoreUnavailable)
	}
	var out *domain.ReviewNote
	err := s.withRLS(ctx, p.TenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `UPDATE review_notes SET status=$1, response_body=$2 WHERE review_note_id=$3 AND status=$4 RETURNING `+reviewNoteColumns,
			domain.ReviewNoteResponded, p.ResponseBody, p.ReviewNoteID, domain.ReviewNoteOpen)
		var err error
		out, err = scanReviewNote(row)
		if errors.Is(err, pgx.ErrNoRows) {
			var exists bool
			if chkErr := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM review_notes WHERE review_note_id=$1)`, p.ReviewNoteID).Scan(&exists); chkErr != nil {
				return chkErr
			}
			if !exists {
				return domain.ErrReviewNoteNotFound
			}
			return domain.ErrReviewNoteInvalidState
		}
		return err
	})
	if errors.Is(err, domain.ErrReviewNoteNotFound) || errors.Is(err, domain.ErrReviewNoteInvalidState) {
		return nil, err
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, nil
}

// ResolveReviewNote is deliberately callable from OPEN or RESPONDED —
// resolution is the reviewer's own act of closing the note, not
// conditioned on a prior response.
func (s *PgStore) ResolveReviewNote(ctx context.Context, p domain.ResolveReviewNoteParams) (*domain.ReviewNote, error) {
	var out *domain.ReviewNote
	err := s.withRLS(ctx, p.TenantID, func(tx pgx.Tx) error {
		now := time.Now().UTC()
		row := tx.QueryRow(ctx, `UPDATE review_notes SET status=$1, resolved_by_principal_id=$2, resolved_at=$3
			WHERE review_note_id=$4 AND status IN ($5,$6) RETURNING `+reviewNoteColumns,
			domain.ReviewNoteResolved, p.ActorPrincipalID, now, p.ReviewNoteID, domain.ReviewNoteOpen, domain.ReviewNoteResponded)
		var err error
		out, err = scanReviewNote(row)
		if errors.Is(err, pgx.ErrNoRows) {
			var exists bool
			if chkErr := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM review_notes WHERE review_note_id=$1)`, p.ReviewNoteID).Scan(&exists); chkErr != nil {
				return chkErr
			}
			if !exists {
				return domain.ErrReviewNoteNotFound
			}
			return domain.ErrReviewNoteInvalidState
		}
		return err
	})
	if errors.Is(err, domain.ErrReviewNoteNotFound) || errors.Is(err, domain.ErrReviewNoteInvalidState) {
		return nil, err
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, nil
}

const signOffColumns = `sign_off_id, review_scope_id, tenant_id, workflow_instance_id, signed_by_principal_id, content_fingerprint, status, signed_at, invalidated_at, invalidation_reason`

func scanSignOff(row pgx.Row) (*domain.SignOff, error) {
	so := &domain.SignOff{}
	err := row.Scan(&so.SignOffID, &so.ReviewScopeID, &so.TenantID, &so.WorkflowInstanceID, &so.SignedByPrincipalID, &so.ContentFingerprint,
		&so.Status, &so.SignedAt, &so.InvalidatedAt, &so.InvalidationReason)
	return so, err
}

// SignOff is the real enforcement point for three mandatory controls at
// once: "role/assignment checked server-side" (the actor must be a
// recorded reviewer for this scope), "unresolved mandatory notes block
// sign-off" (a real COUNT query, not a status flag), and "no self-review"
// (reused from the generic engine's own SoD — a fresh, single-stage
// WorkflowInstance is created with InitiatedBy=the scope's responsible
// party and the actor as its sole approver stage, then immediately
// resolved via the existing SubmitAction APPROVE path, so
// ErrInitiatorCannotBeApprover fires unchanged if they collide).
func (s *PgStore) SignOff(ctx context.Context, p domain.SignOffParams) (*domain.SignOff, error) {
	if p.CorrelationID == "" {
		return nil, fmt.Errorf("%w: correlation id is required", domain.ErrStoreUnavailable)
	}
	if p.ContentFingerprint == "" {
		return nil, fmt.Errorf("%w: content_fingerprint is required", domain.ErrStoreUnavailable)
	}

	var out *domain.SignOff
	err := s.withRLS(ctx, p.TenantID, func(tx pgx.Tx) error {
		var priorSignOffID string
		err := tx.QueryRow(ctx, `SELECT sign_off_id FROM sign_offs WHERE tenant_id=$1 AND correlation_id=$2`, p.TenantID, p.CorrelationID).Scan(&priorSignOffID)
		if err == nil {
			out, err = scanSignOff(tx.QueryRow(ctx, `SELECT `+signOffColumns+` FROM sign_offs WHERE sign_off_id=$1`, priorSignOffID))
			return err
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}

		scope, err := scanReviewScope(tx.QueryRow(ctx, `SELECT `+reviewScopeColumns+` FROM review_scopes WHERE review_scope_id=$1`, p.ReviewScopeID))
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrReviewScopeNotFound
		}
		if err != nil {
			return err
		}
		if scope.InitiatedByPrincipalID == p.ActorPrincipalID {
			return domain.ErrSelfReviewNotAllowed
		}
		var legalEntityID string
		if err := tx.QueryRow(ctx, `SELECT legal_entity_id FROM audit_engagements WHERE engagement_id=$1`, scope.EngagementID).Scan(&legalEntityID); err != nil {
			return err
		}

		var assigned bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM review_assignments WHERE review_scope_id=$1 AND reviewer_principal_id=$2)`,
			p.ReviewScopeID, p.ActorPrincipalID).Scan(&assigned); err != nil {
			return err
		}
		if !assigned {
			return domain.ErrReviewAssignmentRequired
		}

		var unresolvedMandatory int
		if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM review_notes WHERE review_scope_id=$1 AND mandatory=true AND status<>$2`,
			p.ReviewScopeID, domain.ReviewNoteResolved).Scan(&unresolvedMandatory); err != nil {
			return err
		}
		if unresolvedMandatory > 0 {
			return domain.ErrReviewNoteMandatoryUnresolved
		}

		workflowInstanceID := uuid.NewString()
		if _, err := tx.Exec(ctx, `INSERT INTO workflow_instances (workflow_instance_id, tenant_id, legal_entity_id, workflow_type, workflow_status, current_stage, initiated_by, correlation_id, started_at)
			VALUES ($1,$2,$3,$4,'PENDING',1,$5,$6,NOW())`,
			workflowInstanceID, p.TenantID, legalEntityID, "AUDIT_REVIEW_SIGNOFF", scope.InitiatedByPrincipalID, workflowInstanceID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO workflow_stages (workflow_stage_id, workflow_instance_id, stage_order, approver_principal_id, stage_status)
			VALUES ($1,$2,1,$3,'PENDING')`, uuid.NewString(), workflowInstanceID, p.ActorPrincipalID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE workflow_stages SET stage_status='APPROVED', acted_at=NOW() WHERE workflow_instance_id=$1 AND stage_order=1`, workflowInstanceID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE workflow_instances SET workflow_status='APPROVED', current_stage=0, completed_at=NOW() WHERE workflow_instance_id=$1`, workflowInstanceID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO workflow_transitions (workflow_transition_id, workflow_instance_id, from_state, to_state, acted_by, correlation_id, acted_at)
			VALUES ($1,$2,'PENDING','APPROVED',$3,$4,NOW())`, uuid.NewString(), workflowInstanceID, p.ActorPrincipalID, p.CorrelationID); err != nil {
			return err
		}

		row := tx.QueryRow(ctx, `INSERT INTO sign_offs (sign_off_id, review_scope_id, tenant_id, workflow_instance_id, signed_by_principal_id, content_fingerprint, correlation_id)
			VALUES ($1,$2,$3,$4,$5,$6,$7) RETURNING `+signOffColumns,
			uuid.NewString(), p.ReviewScopeID, p.TenantID, workflowInstanceID, p.ActorPrincipalID, p.ContentFingerprint, p.CorrelationID)
		out, err = scanSignOff(row)
		return err
	})
	if errors.Is(err, domain.ErrReviewScopeNotFound) || errors.Is(err, domain.ErrSelfReviewNotAllowed) ||
		errors.Is(err, domain.ErrReviewAssignmentRequired) || errors.Is(err, domain.ErrReviewNoteMandatoryUnresolved) {
		return nil, err
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, nil
}

func (s *PgStore) WithdrawSignOff(ctx context.Context, p domain.WithdrawSignOffParams) (*domain.SignOff, error) {
	var out *domain.SignOff
	err := s.withRLS(ctx, p.TenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `UPDATE sign_offs SET status=$1 WHERE sign_off_id=$2 AND status=$3 AND signed_by_principal_id=$4 RETURNING `+signOffColumns,
			domain.SignOffWithdrawn, p.SignOffID, domain.SignOffValid, p.ActorPrincipalID)
		var err error
		out, err = scanSignOff(row)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrSignOffInvalidState
		}
		return err
	})
	if errors.Is(err, domain.ErrSignOffInvalidState) {
		return nil, err
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, nil
}

// InvalidateSignOffsForTarget is the direct/transactional mechanism for
// "changed protected content invalidates downstream sign-offs" — called
// from LockWorkpaper/AddPostLockAddendum in the SAME transaction that
// mutates the target, so there is no event-driven race.
func InvalidateSignOffsForTarget(ctx context.Context, tx pgx.Tx, targetType, targetID, newFingerprint string) error {
	_, err := tx.Exec(ctx, `UPDATE sign_offs SET status='INVALIDATED', invalidated_at=NOW(), invalidation_reason='target content changed after sign-off'
		WHERE status='VALID' AND content_fingerprint <> $3
		AND review_scope_id IN (SELECT review_scope_id FROM review_scopes WHERE target_type=$1 AND target_id=$2)`,
		targetType, targetID, newFingerprint)
	return err
}

func (s *PgStore) StartQualityReview(ctx context.Context, p domain.StartQualityReviewParams) (*domain.QualityReviewRecord, bool, error) {
	if p.CorrelationID == "" {
		return nil, false, fmt.Errorf("%w: correlation id is required", domain.ErrStoreUnavailable)
	}
	var out *domain.QualityReviewRecord
	created := false
	err := s.withRLS(ctx, p.TenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `INSERT INTO quality_review_records (quality_review_id, engagement_id, tenant_id, started_by_principal_id, correlation_id)
			VALUES ($1,$2,$3,$4,$5) ON CONFLICT DO NOTHING
			RETURNING quality_review_id, engagement_id, tenant_id, status, started_by_principal_id, started_at, completed_by_principal_id, completed_at`,
			uuid.NewString(), p.EngagementID, p.TenantID, p.ActorPrincipalID, p.CorrelationID)
		q := &domain.QualityReviewRecord{}
		err := row.Scan(&q.QualityReviewID, &q.EngagementID, &q.TenantID, &q.Status, &q.StartedByPrincipalID, &q.StartedAt, &q.CompletedByPrincipalID, &q.CompletedAt)
		if err == nil {
			out = q
			created = true
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		row = tx.QueryRow(ctx, `SELECT quality_review_id, engagement_id, tenant_id, status, started_by_principal_id, started_at, completed_by_principal_id, completed_at
			FROM quality_review_records WHERE tenant_id=$1 AND correlation_id=$2`, p.TenantID, p.CorrelationID)
		q2 := &domain.QualityReviewRecord{}
		if err := row.Scan(&q2.QualityReviewID, &q2.EngagementID, &q2.TenantID, &q2.Status, &q2.StartedByPrincipalID, &q2.StartedAt, &q2.CompletedByPrincipalID, &q2.CompletedAt); err != nil {
			return err
		}
		out = q2
		return nil
	})
	if err != nil {
		return nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, created, nil
}

func (s *PgStore) CompleteQualityReview(ctx context.Context, p domain.CompleteQualityReviewParams) (*domain.QualityReviewRecord, bool, error) {
	var out *domain.QualityReviewRecord
	changed := false
	err := s.withRLS(ctx, p.TenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `UPDATE quality_review_records SET status=$1, completed_by_principal_id=$2, completed_at=NOW()
			WHERE quality_review_id=$3 AND status=$4
			RETURNING quality_review_id, engagement_id, tenant_id, status, started_by_principal_id, started_at, completed_by_principal_id, completed_at`,
			domain.QualityReviewCompleted, p.ActorPrincipalID, p.QualityReviewID, domain.QualityReviewInProgress)
		q := &domain.QualityReviewRecord{}
		err := row.Scan(&q.QualityReviewID, &q.EngagementID, &q.TenantID, &q.Status, &q.StartedByPrincipalID, &q.StartedAt, &q.CompletedByPrincipalID, &q.CompletedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			var exists bool
			if chkErr := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM quality_review_records WHERE quality_review_id=$1)`, p.QualityReviewID).Scan(&exists); chkErr != nil {
				return chkErr
			}
			if !exists {
				return domain.ErrQualityReviewNotFound
			}
			return domain.ErrQualityReviewInvalidState
		}
		if err != nil {
			return err
		}
		out = q
		changed = true
		return nil
	})
	if errors.Is(err, domain.ErrQualityReviewNotFound) || errors.Is(err, domain.ErrQualityReviewInvalidState) {
		return nil, false, err
	}
	if err != nil {
		return nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, changed, nil
}

// GetAuditEngagementReportGates is AUD-01's own real completion-gate
// query for the REPORT stage: no VALID-review-scope sign-off may be
// missing/invalid, and no mandatory review note may remain unresolved,
// across every review scope belonging to this engagement.
func (s *PgStore) GetAuditEngagementReportGates(ctx context.Context, tenantID, engagementID string) (allSignOffsValid bool, noUnresolvedMandatoryNotes bool, err error) {
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var scopesWithoutValidSignOff int
		if scanErr := tx.QueryRow(ctx, `SELECT COUNT(*) FROM review_scopes rs
			WHERE rs.engagement_id=$1 AND NOT EXISTS(SELECT 1 FROM sign_offs so WHERE so.review_scope_id=rs.review_scope_id AND so.status='VALID')`,
			engagementID).Scan(&scopesWithoutValidSignOff); scanErr != nil {
			return scanErr
		}
		allSignOffsValid = scopesWithoutValidSignOff == 0

		var unresolvedNotes int
		if scanErr := tx.QueryRow(ctx, `SELECT COUNT(*) FROM review_notes rn
			JOIN review_scopes rs ON rs.review_scope_id = rn.review_scope_id
			WHERE rs.engagement_id=$1 AND rn.mandatory=true AND rn.status<>$2`,
			engagementID, domain.ReviewNoteResolved).Scan(&unresolvedNotes); scanErr != nil {
			return scanErr
		}
		noUnresolvedMandatoryNotes = unresolvedNotes == 0
		return nil
	})
	if err != nil {
		return false, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return allSignOffsValid, noUnresolvedMandatoryNotes, nil
}
