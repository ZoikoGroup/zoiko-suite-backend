package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"go.uber.org/zap"

	"zoiko.io/workflow-svc/internal/domain"
	"zoiko.io/workflow-svc/internal/notify"
	"zoiko.io/workflow-svc/internal/outbox"
)

// noticeSettings turns on approver and requester notices (26 Nov 2026).
type noticeSettings struct {
	// consoleBaseURL prefixes the link a notice carries to the workflow.
	consoleBaseURL string
}

// WithNotices makes every workflow transition also write its notices to the
// outbox (destination notification), in the transition's own transaction. Off
// by default: with no notification-svc to deliver them, rows would only pile
// up. consoleBaseURL is the console's public base URL ("" gives relative
// links).
func (s *PgStore) WithNotices(consoleBaseURL string) *PgStore {
	s.notices = &noticeSettings{consoleBaseURL: consoleBaseURL}
	return s
}

// enqueueNoticesTx writes the notices of one transition (internal/notify) to
// the outbox in tx. approverStage, when > 0, is the stage whose named
// approvers are told: the one that now waits (started, next stage, reopened)
// or the one that was waiting (cancelled, invalidated). in.Approvers may
// already name others (an escalation target). The instance fields are filled
// from inst.
func (s *PgStore) enqueueNoticesTx(ctx context.Context, tx pgx.Tx, inst *domain.WorkflowInstance, in notify.Input,
	approverStage int, correlationID *string) error {
	if s.notices == nil || inst == nil {
		return nil
	}
	if approverStage > 0 {
		approvers, err := s.stageApproversTx(ctx, tx, inst.WorkflowInstanceID, approverStage)
		if err != nil {
			return err
		}
		in.Approvers = append(in.Approvers, approvers...)
	}
	in.WorkflowInstanceID = inst.WorkflowInstanceID
	in.WorkflowType = inst.WorkflowType
	in.InitiatedBy = inst.InitiatedBy
	if inst.SubjectType != nil {
		in.SubjectType = *inst.SubjectType
	}
	if inst.SubjectID != nil {
		in.SubjectID = *inst.SubjectID
	}
	in.BaseURL = s.notices.consoleBaseURL

	actor := in.Actor
	for _, n := range notify.Build(in) {
		if err := outbox.Insert(ctx, tx, outbox.Event{
			AggregateType: "workflow_instance",
			AggregateID:   inst.WorkflowInstanceID,
			EventType:     n.SourceEventType,
			TenantID:      inst.TenantID,
			LegalEntityID: inst.LegalEntityID,
			ActorID:       &actor,
			CorrelationID: correlationID,
			Payload:       n,
			Destination:   outbox.DestinationNotification,
		}); err != nil {
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}
	}
	return nil
}

// stageApproversTx names the people who decide stage order of an instance: a
// SINGLE stage's approver, or a QUORUM stage's eligible pool. A ROLE stage
// names nobody (its holders live in authorization-svc), so nobody on the
// approver side is told; the holders still see it under My Approvals.
func (s *PgStore) stageApproversTx(ctx context.Context, tx pgx.Tx, instanceID string, order int) ([]string, error) {
	var stageID, stageType, approver, role string
	err := tx.QueryRow(ctx, `
		SELECT workflow_stage_id::text, stage_type, COALESCE(approver_principal_id, ''), COALESCE(approver_role, '')
		FROM workflow_stages WHERE workflow_instance_id = $1 AND stage_order = $2`, instanceID, order).
		Scan(&stageID, &stageType, &approver, &role)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	switch stageType {
	case domain.StageTypeQuorum:
		rows, err := tx.Query(ctx, `
			SELECT approver_principal_id FROM workflow_stage_quorum_approvers
			WHERE workflow_stage_id = $1 ORDER BY approver_principal_id`, stageID)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}
		defer rows.Close()
		var pool []string
		for rows.Next() {
			var p string
			if err := rows.Scan(&p); err != nil {
				return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
			}
			pool = append(pool, p)
		}
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}
		return pool, nil
	case domain.StageTypeRole:
		s.log.Info("workflow notices: a ROLE stage names no approver to notify",
			zap.String("workflow_instance_id", instanceID), zap.Int("stage_order", order), zap.String("approver_role", role))
		return nil, nil
	default:
		if approver == "" {
			return nil, nil
		}
		return []string{approver}, nil
	}
}

// personReason is the reason a person gave, or "" when they gave none.
func personReason(r *string) string {
	if r == nil {
		return ""
	}
	return *r
}
