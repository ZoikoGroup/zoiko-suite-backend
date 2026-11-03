package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"go.uber.org/zap"

	"zoiko.io/authorization-svc/internal/domain"
)

// RepointMergedSubject applies identity-context-svc's identity.subject.merged
// (ORG-1 MergeDuplicateIdentity): every live assignment the duplicate holds in
// the tenant is reissued to the survivor, and the duplicate's own row ends now.
//
// End-and-reissue rather than rewriting principal_id in place, so the history
// trigger and the iam.assignment.revoked / .granted events say what happened
// to each subject, and the duplicate's past authority stays answerable as-of.
// A suspended assignment stays suspended on the survivor. An assignment the
// survivor already holds live (same role, entity, book and org unit) is ended
// on the duplicate without a copy. Idempotent: a redelivery finds nothing live.
func (s *PgStore) RepointMergedSubject(ctx context.Context, p domain.RepointMergedSubjectParams) (int, error) {
	ctx = domain.WithReason(domain.WithAudit(ctx, p.Actor, p.CorrelationID), "identity.subject.merged into "+p.SurvivorID)
	var moved int
	err := s.withRLS(ctx, p.TenantID, func(tx pgx.Tx) error {
		const live = `a.principal_id = $1 AND a.approval_status = 'APPROVED'
		   AND (a.effective_to IS NULL OR a.effective_to > clock_timestamp())
		   AND EXISTS (SELECT 1 FROM roles r WHERE r.role_id = a.role_id
		               AND r.tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)`
		tag, err := tx.Exec(ctx, `
			INSERT INTO principal_role_assignments
			    (principal_id, role_id, legal_entity_id, effective_from, effective_to, assigned_by,
			     book_id, org_unit_id, approval_status, approved_by, approval_reference, approval_decided_at,
			     suspended_at, suspended_by, suspension_reason)
			SELECT $2, a.role_id, a.legal_entity_id, clock_timestamp(), a.effective_to, $3,
			       a.book_id, a.org_unit_id, 'APPROVED', a.approved_by, a.approval_reference, a.approval_decided_at,
			       a.suspended_at, a.suspended_by, a.suspension_reason
			  FROM principal_role_assignments a
			 WHERE `+live+`
			   AND NOT EXISTS (SELECT 1 FROM principal_role_assignments h
			                    WHERE h.principal_id = $2 AND h.role_id = a.role_id
			                      AND h.legal_entity_id IS NOT DISTINCT FROM a.legal_entity_id
			                      AND h.book_id IS NOT DISTINCT FROM a.book_id
			                      AND h.org_unit_id IS NOT DISTINCT FROM a.org_unit_id
			                      AND h.approval_status = 'APPROVED'
			                      AND (h.effective_to IS NULL OR h.effective_to > clock_timestamp()))`,
			p.DuplicateID, p.SurvivorID, "merge:"+p.DuplicateID)
		if err != nil {
			return fmt.Errorf("reissue to survivor: %w", err)
		}
		moved = int(tag.RowsAffected())
		if _, err := tx.Exec(ctx, `UPDATE principal_role_assignments a SET effective_to = clock_timestamp()
			 WHERE `+live, p.DuplicateID); err != nil {
			return fmt.Errorf("end duplicate assignments: %w", err)
		}
		return nil
	})
	if err != nil {
		s.log.Error("pg RepointMergedSubject failed", zap.Error(err))
		return 0, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return moved, nil
}
